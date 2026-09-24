package meetings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SummaryResult is the structured summary of a transcript.
type SummaryResult struct {
	Summary      string       `json:"summary"`
	KeyDecisions []string     `json:"key_decisions"`
	ActionItems  []ActionItem `json:"action_items"`
	Source       string       `json:"source"`
}

// Summarize uses the Anthropic Messages API when ANTHROPIC_API_KEY is set and
// falls back to the local extractive summariser otherwise (or on any error).
func Summarize(ctx context.Context, title, text string) SummaryResult {
	if key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); key != "" && strings.TrimSpace(text) != "" {
		model := os.Getenv("ANTHROPIC_MODEL")
		if model == "" {
			model = "claude-sonnet-4-5"
		}
		if res, err := summarizeAnthropic(ctx, key, model, title, text); err == nil {
			return res
		}
	}
	return ExtractiveSummary(text)
}

func summarizeAnthropic(ctx context.Context, key, model, title, text string) (SummaryResult, error) {
	const maxChars = 150000
	if len(text) > maxChars {
		text = text[:maxChars]
	}
	prompt := "You are summarising a business meeting transcript.\n" +
		"Meeting title: " + title + "\n\n<transcript>\n" + text + "\n</transcript>\n\n" +
		"Return ONLY a JSON object, no prose, with keys:\n" +
		`"summary": a concise 3-6 sentence summary,` + "\n" +
		`"key_decisions": array of short strings (empty if none),` + "\n" +
		`"action_items": array of {"text": string, "owner": string (person responsible if mentioned, else ""), "due": string (deadline if mentioned, else "")}.`
	body, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"max_tokens": 1500,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return SummaryResult{}, err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return SummaryResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return SummaryResult{}, fmt.Errorf("anthropic: status %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return SummaryResult{}, err
	}
	var txt strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			txt.WriteString(c.Text)
		}
	}
	s := txt.String()
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return SummaryResult{}, fmt.Errorf("anthropic: no JSON in response")
	}
	var res SummaryResult
	if err := json.Unmarshal([]byte(s[i:j+1]), &res); err != nil {
		return SummaryResult{}, err
	}
	if strings.TrimSpace(res.Summary) == "" {
		return SummaryResult{}, fmt.Errorf("anthropic: empty summary")
	}
	if res.KeyDecisions == nil {
		res.KeyDecisions = []string{}
	}
	if res.ActionItems == nil {
		res.ActionItems = []ActionItem{}
	}
	res.Source = "anthropic"
	return res, nil
}

// ---------------------------------------------------------------------------
// Local extractive summariser
// ---------------------------------------------------------------------------

var (
	// "[10:02:11] You:" / "10:02 - Alice:" / "Alice:" prefixes on transcript lines.
	linePrefixRe = regexp.MustCompile(`^\s*(?:\[?\d{1,2}:\d{2}(?::\d{2})?\]?\s*[-–]?\s*)?(?:([A-Z][\w .'-]{0,30}):\s+)?`)
	sentenceRe   = regexp.MustCompile(`[^.!?]+[.!?]*`)
	wordRe       = regexp.MustCompile(`[\p{L}\p{N}']+`)

	actionRe   = regexp.MustCompile(`(?i)\b(will|i'll|we'll|he'll|she'll|they'll|going to|need to|needs to|have to|has to|must|action item|action|follow[- ]?up|to-?do|assign(?:ed)?|take care of|responsible for|deadline|by (?:monday|tuesday|wednesday|thursday|friday|saturday|sunday|tomorrow|tonight|today|next \w+|end of \w+|eod|eow|cob|the \d+\w*|\d{1,2}[/-]\d{1,2}|\d+(?:st|nd|rd|th)?|january|february|march|april|may|june|july|august|september|october|november|december))\b`)
	decisionRe = regexp.MustCompile(`(?i)\b(decided|decision|agreed|agree to|we'll go with|we will go with|let's go with|approved|confirmed|settled on|resolved to|final answer|go ahead with|signed off)\b`)
	dueRe      = regexp.MustCompile(`(?i)\bby ((?:next |this |the end of |end of )?(?:monday|tuesday|wednesday|thursday|friday|saturday|sunday|tomorrow|tonight|today|week|month|eod|eow|cob|\d{1,2}(?:st|nd|rd|th)?(?: of)?(?: \w+)?|\d{1,2}[/-]\d{1,2}(?:[/-]\d{2,4})?|january|february|march|april|may|june|july|august|september|october|november|december)(?: \d{1,2}(?:st|nd|rd|th)?)?)`)
	ownerRe    = regexp.MustCompile(`\b([A-Z][a-z]+(?: [A-Z][a-z]+)?) (?:will|to|needs to|need to|is going to|has to|should|must|can)\b`)
	mentionRe  = regexp.MustCompile(`@(\w+)`)
	firstPerRe = regexp.MustCompile(`(?i)^\s*(i'll|i will|i need to|i have to|i'm going to|i can)\b`)
)

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an the and or but if then so of to in on at for with from by about as is are was were be been being am
	it its it's this that these those there here i me my we our us you your he she him her they them their what which who whom
	do does did doing have has had having not no yes ok okay yeah um uh like just really very can could would should will shall
	also too all any some more most much many one two get got go going know think well right now see say said let let's i'm you're
	we're they're don't didn't can't won't that's there's than into up down out over under again further once only own same`) {
		m[w] = true
	}
	return m
}()

type sentence struct {
	text    string
	speaker string
	idx     int
	score   float64
}

// splitSentences breaks a transcript into sentences, remembering the speaker
// label of each line when present.
func splitSentences(text string) []sentence {
	var out []sentence
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		speaker := ""
		if m := linePrefixRe.FindStringSubmatchIndex(line); m != nil {
			if m[2] >= 0 {
				speaker = strings.TrimSpace(line[m[2]:m[3]])
			}
			line = line[m[1]:]
		}
		for _, s := range sentenceRe.FindAllString(line, -1) {
			s = strings.TrimSpace(s)
			if len(wordRe.FindAllString(s, -1)) < 3 {
				continue
			}
			out = append(out, sentence{text: s, speaker: speaker, idx: len(out)})
		}
	}
	return out
}

func contentWords(s string) []string {
	var out []string
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		if len(w) < 3 || stopwords[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// ExtractiveSummary scores sentences by normalised term frequency and picks
// the top ones in their original order, and detects decisions and action
// items with phrase patterns. It needs no network access.
func ExtractiveSummary(text string) SummaryResult {
	res := SummaryResult{KeyDecisions: []string{}, ActionItems: []ActionItem{}, Source: "local"}
	sents := splitSentences(text)
	if len(sents) == 0 {
		t := strings.TrimSpace(text)
		if t == "" {
			res.Summary = "No transcript was recorded for this meeting."
		} else {
			res.Summary = t
		}
		return res
	}

	tf := map[string]float64{}
	for _, s := range sents {
		for _, w := range contentWords(s.text) {
			tf[w]++
		}
	}
	maxTF := 0.0
	for _, v := range tf {
		maxTF = math.Max(maxTF, v)
	}
	for i := range sents {
		words := contentWords(sents[i].text)
		if len(words) == 0 {
			continue
		}
		sum := 0.0
		for _, w := range words {
			sum += tf[w] / maxTF
		}
		// Normalise by length (sqrt dampens the bias towards long sentences).
		score := sum / math.Sqrt(float64(len(words)))
		if decisionRe.MatchString(sents[i].text) {
			score *= 1.25
		}
		sents[i].score = score
	}

	n := int(math.Ceil(float64(len(sents)) * 0.3))
	if n < 2 {
		n = 2
	}
	if n > len(sents) {
		n = len(sents)
	}
	if n > 5 {
		n = 5
	}
	ranked := append([]sentence(nil), sents...)
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a].score > ranked[b].score })
	top := ranked[:n]
	sort.Slice(top, func(a, b int) bool { return top[a].idx < top[b].idx })
	parts := make([]string, 0, n)
	for _, s := range top {
		parts = append(parts, ensurePeriod(s.text))
	}
	res.Summary = strings.Join(parts, " ")

	seen := map[string]bool{}
	for _, s := range sents {
		key := strings.ToLower(s.text)
		if decisionRe.MatchString(s.text) && !seen["d"+key] {
			seen["d"+key] = true
			res.KeyDecisions = append(res.KeyDecisions, ensurePeriod(s.text))
		}
		if actionRe.MatchString(s.text) && !isQuestion(s.text) && !seen["a"+key] {
			seen["a"+key] = true
			res.ActionItems = append(res.ActionItems, ActionItem{Text: ensurePeriod(s.text), Owner: findOwner(s), Due: findDue(s.text)})
		}
	}
	return res
}

func isQuestion(s string) bool { return strings.HasSuffix(strings.TrimSpace(s), "?") }

func ensurePeriod(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	switch s[len(s)-1] {
	case '.', '!', '?':
		return s
	}
	return s + "."
}

var notNames = map[string]bool{"We": true, "They": true, "It": true, "This": true, "That": true, "There": true, "Someone": true, "Everyone": true, "Action": true, "Next": true, "The": true, "Then": true, "So": true, "And": true, "But": true, "Also": true, "Please": true, "You": true, "I": true, "He": true, "She": true, "Who": true, "What": true, "Monday": true, "Tuesday": true, "Wednesday": true, "Thursday": true, "Friday": true}

func findOwner(s sentence) string {
	if m := mentionRe.FindStringSubmatch(s.text); m != nil {
		return m[1]
	}
	for _, m := range ownerRe.FindAllStringSubmatch(s.text, -1) {
		first := strings.Fields(m[1])[0]
		if !notNames[first] {
			return m[1]
		}
	}
	if firstPerRe.MatchString(s.text) && s.speaker != "" {
		return s.speaker
	}
	return ""
}

func findDue(s string) string {
	if m := dueRe.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}
