package football

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Paste parser for jackpot games and the fuzzy team-name rules used for
// jackpot matching and the free sources (ClubElo, football-data.co.uk,
// OddsPapi, football-data.org names).

// ParsedGame is one pasted line.
type ParsedGame struct {
	Line    int        `json:"line"`
	Raw     string     `json:"raw"`
	Home    string     `json:"home"`
	Away    string     `json:"away"`
	Odds    []float64  `json:"odds"`
	Kickoff *time.Time `json:"kickoff"`
	Country string     `json:"country"`
	Match   *GameMatch `json:"match,omitempty"`
}

// SkippedLine is a line the parser could not use.
type SkippedLine struct {
	Line int    `json:"line"`
	Raw  string `json:"raw"`
	Why  string `json:"why"`
}

// ParseResult is the parser's answer.
type ParseResult struct {
	Games    []ParsedGame  `json:"games"`
	Skipped  []SkippedLine `json:"skipped"`
	Warnings []string      `json:"warnings"`
}

var (
	reOdds3     = regexp.MustCompile(`(\d{1,3}[.,]\d{1,3})\s*[/|;]?\s+(\d{1,3}[.,]\d{1,3})\s*[/|;]?\s+(\d{1,3}[.,]\d{1,3})\s*$`)
	reOddsOnly  = regexp.MustCompile(`^\s*(\d{1,3}[.,]\d{1,3})\s*[/|;]?\s+(\d{1,3}[.,]\d{1,3})\s*[/|;]?\s+(\d{1,3}[.,]\d{1,3})\s*$`)
	reNumbering = regexp.MustCompile(`^\s*(?:#|no\.?\s*|game\s*)?\d{1,2}\s*(?:[.):\]-]\s*|\s+)`)
	reISODate   = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})(?:[ T](\d{1,2}):(\d{2}))?\b`)
	reDMYDate   = regexp.MustCompile(`\b(\d{1,2})[./](\d{1,2})(?:[./](\d{2,4}))?\b`)
	reTime      = regexp.MustCompile(`\b([01]?\d|2[0-3])[:h]([0-5]\d)\b`)
	reWeekday   = regexp.MustCompile(`(?i)\b(mon|tue|tues|wed|thu|thur|thurs|fri|sat|sun)(day)?\b\.?`)
	reMonthDay  = regexp.MustCompile(`(?i)\b(\d{1,2})\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?(?:\s+(\d{4}))?\b`)
	reCountry   = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z .'-]{1,30}?)\s*(?::|\s[-–]\s)\s*`)
	reParenTag  = regexp.MustCompile(`\s*[\(\[][^)\]]{1,20}[\)\]]\s*`)
	reMultiSp   = regexp.MustCompile(`\s+`)
)

var monthNum = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}

// knownCountries are stripped when they prefix a line ("Poland: Piast …").
var knownCountries = map[string]string{"poland": "Poland", "pol": "Poland", "austria": "Austria", "aut": "Austria", "germany": "Germany", "ger": "Germany",
	"deu": "Germany", "denmark": "Denmark", "den": "Denmark", "dnk": "Denmark", "romania": "Romania", "rou": "Romania", "rom": "Romania", "belgium": "Belgium",
	"bel": "Belgium", "netherlands": "Netherlands", "ned": "Netherlands", "nld": "Netherlands", "holland": "Netherlands", "northern ireland": "Northern Ireland",
	"nir": "Northern Ireland", "england": "England", "eng": "England", "spain": "Spain", "esp": "Spain", "italy": "Italy", "ita": "Italy", "france": "France",
	"fra": "France", "portugal": "Portugal", "por": "Portugal", "scotland": "Scotland", "sco": "Scotland", "kenya": "Kenya", "ken": "Kenya", "sweden": "Sweden",
	"swe": "Sweden", "norway": "Norway", "nor": "Norway", "switzerland": "Switzerland", "sui": "Switzerland", "turkey": "Turkey", "tur": "Turkey",
	"czech republic": "Czech Republic", "cze": "Czech Republic", "ireland": "Ireland", "irl": "Ireland", "greece": "Greece", "gre": "Greece",
	"world": "World", "europe": "Europe", "international": "World"}

func parseDec(s string) float64 {
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil {
		return 0
	}
	return v
}

// ParseJackpotText reads pasted jackpot games, one per line (see the
// contract for the accepted shapes). base is used for dates without a year.
func ParseJackpotText(text string, base time.Time, loc *time.Location) ParseResult {
	if loc == nil {
		loc = time.UTC
	}
	res := ParseResult{Games: []ParsedGame{}, Skipped: []SkippedLine{}, Warnings: []string{}}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	type pending struct {
		line int
		raw  string
		home string
		away string
	}
	var pend *pending
	var loose []struct {
		line int
		text string
	}
	for i := 0; i < len(lines); i++ {
		raw := strings.TrimSpace(strings.ReplaceAll(lines[i], " ", " "))
		if raw == "" {
			continue
		}
		ln := i + 1
		// Odds alone on a line: complete the previous teams line (or the
		// 3-line copy format: home, away, odds).
		if m := reOddsOnly.FindStringSubmatch(raw); m != nil {
			odds := []float64{parseDec(m[1]), parseDec(m[2]), parseDec(m[3])}
			switch {
			case pend != nil:
				g := ParsedGame{Line: pend.line, Raw: pend.raw + " " + raw, Home: pend.home, Away: pend.away, Odds: odds}
				res.Games = append(res.Games, g)
				pend = nil
			case len(loose) >= 2:
				h, a := loose[len(loose)-2], loose[len(loose)-1]
				for _, x := range loose[:len(loose)-2] {
					res.Skipped = append(res.Skipped, SkippedLine{Line: x.line, Raw: x.text, Why: "no home/away teams found"})
				}
				res.Games = append(res.Games, ParsedGame{Line: h.line, Raw: h.text + " / " + a.text + " " + raw, Home: cleanTeam(h.text), Away: cleanTeam(a.text), Odds: odds})
				loose = nil
			default:
				res.Skipped = append(res.Skipped, SkippedLine{Line: ln, Raw: raw, Why: "odds without teams"})
			}
			continue
		}
		g, why := parseGameLine(raw, base, loc)
		if why == "no odds" && g.Home != "" && g.Away != "" {
			if pend != nil {
				res.Skipped = append(res.Skipped, SkippedLine{Line: pend.line, Raw: pend.raw, Why: "no odds found"})
			}
			pend = &pending{line: ln, raw: raw, home: g.Home, away: g.Away}
			for _, x := range loose {
				res.Skipped = append(res.Skipped, SkippedLine{Line: x.line, Raw: x.text, Why: "no home/away teams found"})
			}
			loose = nil
			continue
		}
		if why != "" {
			// A line with a single team name (3-line format) is kept aside.
			if why == "no home/away teams found" && !strings.ContainsAny(raw, "0123456789") && len([]rune(raw)) <= 60 {
				loose = append(loose, struct {
					line int
					text string
				}{ln, raw})
				continue
			}
			res.Skipped = append(res.Skipped, SkippedLine{Line: ln, Raw: raw, Why: why})
			continue
		}
		if pend != nil {
			res.Skipped = append(res.Skipped, SkippedLine{Line: pend.line, Raw: pend.raw, Why: "no odds found"})
			pend = nil
		}
		for _, x := range loose {
			res.Skipped = append(res.Skipped, SkippedLine{Line: x.line, Raw: x.text, Why: "no home/away teams found"})
		}
		loose = nil
		g.Line = ln
		res.Games = append(res.Games, g)
	}
	if pend != nil {
		res.Skipped = append(res.Skipped, SkippedLine{Line: pend.line, Raw: pend.raw, Why: "no odds found"})
	}
	for _, x := range loose {
		res.Skipped = append(res.Skipped, SkippedLine{Line: x.line, Raw: x.text, Why: "no home/away teams found"})
	}
	sort.SliceStable(res.Skipped, func(i, j int) bool { return res.Skipped[i].Line < res.Skipped[j].Line })
	for _, g := range res.Games {
		if len(g.Odds) == 3 {
			inv := 0.0
			bad := false
			for _, o := range g.Odds {
				if o < 1.01 || o > 1000 {
					bad = true
				} else {
					inv += 1 / o
				}
			}
			switch {
			case bad:
				res.Warnings = append(res.Warnings, "Line "+strconv.Itoa(g.Line)+": odds must be between 1.01 and 1000 — check them.")
			case inv-1 > 0.15:
				res.Warnings = append(res.Warnings, "Line "+strconv.Itoa(g.Line)+": the odds' margin is "+strconv.FormatFloat((inv-1)*100, 'f', 1, 64)+"% — check them against the app.")
			case inv < 0.97:
				res.Warnings = append(res.Warnings, "Line "+strconv.Itoa(g.Line)+": the odds add up to less than 100% — check for a typo.")
			}
		}
	}
	return res
}

// parseGameLine reads "1. GKS PIAST GLIWICE - KORONA KELCE 2.44 3.35 2.55".
func parseGameLine(raw string, base time.Time, loc *time.Location) (ParsedGame, string) {
	g := ParsedGame{Raw: raw}
	s := raw
	var odds []float64
	if m := reOdds3.FindStringSubmatchIndex(s); m != nil {
		odds = []float64{parseDec(s[m[2]:m[3]]), parseDec(s[m[4]:m[5]]), parseDec(s[m[6]:m[7]])}
		s = s[:m[0]]
	}
	s = reNumbering.ReplaceAllString(s, "")
	// Dates and times.
	var day *time.Time
	hh, mm := -1, -1
	if m := reISODate.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc)
		day = &t
		if m[4] != "" {
			hh, _ = strconv.Atoi(m[4])
			mm, _ = strconv.Atoi(m[5])
		}
		s = reISODate.ReplaceAllString(s, " ")
	}
	if m := reMonthDay.FindStringSubmatch(s); m != nil && day == nil {
		d, _ := strconv.Atoi(m[1])
		mo := monthNum[strings.ToLower(m[2])[:3]]
		y := base.Year()
		if m[3] != "" {
			y, _ = strconv.Atoi(m[3])
		}
		if mo > 0 && d >= 1 && d <= 31 {
			t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc)
			day = &t
		}
		s = reMonthDay.ReplaceAllString(s, " ")
	}
	if m := reTime.FindStringSubmatch(s); m != nil {
		hh, _ = strconv.Atoi(m[1])
		mm, _ = strconv.Atoi(m[2])
		s = reTime.ReplaceAllString(s, " ")
	}
	if m := reDMYDate.FindStringSubmatch(s); m != nil && day == nil {
		d, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		y := base.Year()
		if m[3] != "" {
			y, _ = strconv.Atoi(m[3])
			if y < 100 {
				y += 2000
			}
		}
		if mo >= 1 && mo <= 12 && d >= 1 && d <= 31 {
			t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc)
			day = &t
			s = strings.Replace(s, m[0], " ", 1)
		}
	}
	s = reWeekday.ReplaceAllString(s, " ")
	if day != nil {
		t := *day
		if hh >= 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), hh, mm, 0, 0, loc)
		}
		u := t.UTC()
		g.Kickoff = &u
	}
	// Country prefix.
	if m := reCountry.FindStringSubmatch(s); m != nil {
		if c, ok := knownCountries[strings.ToLower(strings.TrimSpace(m[1]))]; ok {
			g.Country = c
			s = s[len(m[0]):]
		}
	}
	var home, away string
	ok := false
	if strings.Contains(s, "\t") {
		// Tab-separated copy from a spreadsheet or an app table.
		fs := []string{}
		for _, x := range strings.Split(s, "\t") {
			if x = strings.TrimSpace(x); x != "" {
				fs = append(fs, x)
			}
		}
		if len(fs) >= 2 {
			home, away, ok = fs[len(fs)-2], fs[len(fs)-1], true
		}
	}
	s = strings.TrimSpace(reMultiSp.ReplaceAllString(s, " "))
	if !ok {
		home, away, ok = splitTeams(s)
	}
	if !ok {
		return g, "no home/away teams found"
	}
	g.Home, g.Away = cleanTeam(home), cleanTeam(away)
	if g.Home == "" || g.Away == "" {
		return g, "no home/away teams found"
	}
	if odds == nil {
		return g, "no odds"
	}
	g.Odds = odds
	return g, ""
}

var teamSeps = []string{"\t", " vs. ", " vs ", " v. ", " v ", " – ", " — ", " - ", " x ", " / ", "/", " -", "- "}

func splitTeams(s string) (string, string, bool) {
	ls := strings.ToLower(s)
	for _, sep := range teamSeps {
		if i := strings.Index(ls, sep); i > 0 {
			h, a := strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(sep):])
			if h != "" && a != "" {
				return h, a, true
			}
		}
	}
	return "", "", false
}

func cleanTeam(s string) string {
	s = reParenTag.ReplaceAllString(s, " ")
	s = strings.Trim(strings.TrimSpace(s), "-–—:,;.")
	return strings.TrimSpace(reMultiSp.ReplaceAllString(s, " "))
}

// ---------------------------------------------------------------- names

// foldMap covers letters that Unicode decomposition does not split.
var foldMap = map[rune]string{'ł': "l", 'Ł': "l", 'ø': "o", 'Ø': "o", 'æ': "ae", 'Æ': "ae", 'ß': "ss", 'đ': "d", 'Đ': "d", 'ı': "i",
	'þ': "th", 'ð': "d", 'œ': "oe", 'Œ': "oe", 'ħ': "h", 'ŀ': "l"}

// Fold lower-cases and removes diacritics (ł → l, ø → o, ß → ss included).
func Fold(s string) string {
	b := strings.Builder{}
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if x, ok := foldMap[r]; ok {
			b.WriteString(x)
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// extra club-form words dropped for fuzzy matching (on top of NormTeam's).
var freeStop = map[string]bool{"gks": true, "ks": true, "mks": true, "lks": false, "tsv": true, "vfl": true, "vfb": true, "bsc": true, "nk": true,
	"csm": false, "cs": true, "acs": true, "kv": true, "rsc": true, "krc": true, "kaa": true, "sv": true, "fk": true, "ifk": false, "bv": true,
	"sk": true, "ssc": true, "us": true, "ud": true, "rc": true, "cd": true, "ca": true, "se": true, "ec": true, "spvgg": true, "sc": true, "ac": true}

var youthRe = regexp.MustCompile(`\b(u ?1[5-9]|u ?2[0-3]|ii|b|reserves?|res|jong|youth|juniors?|2|amateure|primavera|nxt|w|women|ladies|fem)\b`)

// youthTag returns the youth / reserve / women marker of a name ("" for the first team).
func youthTag(name string) string {
	f := Fold(name)
	f = strings.NewReplacer(".", " ", "-", " ").Replace(f)
	if m := youthRe.FindString(" " + f + " "); m != "" {
		m = strings.ReplaceAll(m, " ", "")
		switch m {
		case "ii", "b", "reserves", "reserve", "res", "jong", "2", "amateure", "nxt":
			return "reserve"
		case "w", "women", "ladies", "fem":
			return "women"
		}
		return m
	}
	return ""
}

func freeTokens(s string) []string {
	out := []string{}
	for _, t := range strings.Fields(NormTeam(Fold(s))) {
		if freeStop[t] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// TeamSim is a 0..1 similarity for messy team names ("KORONA KELCE" ~
// "Korona Kielce", "CHOJNICE" ~ "Chojniczanka Chojnice", "U CLUJ" ~
// "Universitatea Cluj", "CLIFTONVILLE BELFAST" ~ "Cliftonville"): the best of
// NameSimilarity and a token alignment (equal, prefix, initial, typo), with
// a penalty when only one side is a youth / reserve / women team.
func TeamSim(a, b string) float64 {
	base := NameSimilarity(Fold(a), Fold(b))
	ta, tb := freeTokens(a), freeTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return base
	}
	short, long := ta, tb
	if len(short) > len(long) {
		short, long = long, short
	}
	used := make([]bool, len(long))
	tot := 0.0
	for _, t := range short {
		best, bj := 0.0, -1
		for j, u := range long {
			if used[j] {
				continue
			}
			v := 0.0
			switch {
			case t == u:
				v = 1
			case len(t) >= 3 && len(u) >= 3 && (strings.HasPrefix(u, t) || strings.HasPrefix(t, u)):
				v = 0.9
			case (len(t) == 1 && strings.HasPrefix(u, t)) || (len(u) == 1 && strings.HasPrefix(t, u)):
				v = 0.7
			default:
				if len(t) >= 4 && len(u) >= 4 {
					l := 1 - float64(levenshtein(t, u))/math.Max(float64(len(t)), float64(len(u)))
					if l >= 0.75 {
						v = l
					}
				}
			}
			if v > best {
				best, bj = v, j
			}
		}
		if bj >= 0 {
			used[bj] = true
		}
		tot += best
	}
	align := tot / float64(len(short)) * (0.85 + 0.15*float64(len(short))/float64(len(long)))
	// A single generic token ("city", "united") must not carry a match.
	if len(short) == 1 && len(short[0]) <= 3 && align > 0.7 {
		align = 0.7
	}
	sim := math.Max(base, align)
	if youthTag(a) != youthTag(b) {
		sim *= 0.5
	}
	return math.Min(1, sim)
}

// normFree is the alias key used by the free sources (diacritics folded).
func normFree(s string) string { return NormTeam(Fold(s)) }
