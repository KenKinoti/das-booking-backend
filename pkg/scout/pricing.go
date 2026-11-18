package scout

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Prices — every rate the scout uses to price a run lives here.
//
// Token prices (US$ per million tokens) are the published Claude API prices:
// https://platform.claude.com/en/docs/about-claude/pricing (read 18 Nov 2026).
// Web search: "$10 USD per 1,000 searches, plus standard token costs for
// search-generated content"; search results count as input tokens; a search
// that fails with an error is not billed —
// https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool
// and the pricing page above.
//
// The actual cost of a run is computed from the usage the API returns
// (usage.input_tokens, output_tokens, cache_creation_input_tokens,
// cache_read_input_tokens and server_tool_use.web_search_requests).

// SearchUSD is the price of one web search.
const SearchUSD = 10.0 / 1000

// PricingSource is shown next to estimates.
const PricingSource = "https://platform.claude.com/en/docs/about-claude/pricing"

// Rate is the price of one model family (US$ per million tokens).
type Rate struct {
	Model      string  `json:"model"` // model id prefix, e.g. "claude-sonnet-4-5"
	Name       string  `json:"name"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheWrite float64 `json:"cache_write"` // 5-minute cache writes
	CacheRead  float64 `json:"cache_read"`
	// Long-prompt tier: requests with more than LongOver input tokens are
	// priced at LongInput / LongOutput (Haiku 5.5).
	LongOver   int     `json:"long_over,omitempty"`
	LongInput  float64 `json:"long_input,omitempty"`
	LongOutput float64 `json:"long_output,omitempty"`
	Known      bool    `json:"known"`
}

// Rates, most specific prefix first is not required: the longest matching
// prefix wins.
var Rates = []Rate{
	{Model: "claude-fable-5-2", Name: "Claude Fable 5.2", Input: 8, Output: 40, CacheWrite: 10, CacheRead: 0.20},
	{Model: "claude-mythos-5-2", Name: "Claude Mythos 5.2", Input: 8, Output: 40, CacheWrite: 10, CacheRead: 0.20},
	{Model: "claude-fable-5-1", Name: "Claude Fable 5.1", Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 0.25},
	{Model: "claude-mythos-5-1", Name: "Claude Mythos 5.1", Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 0.25},
	{Model: "claude-fable-5", Name: "Claude Fable 5", Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1},
	{Model: "claude-mythos-5", Name: "Claude Mythos 5", Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1},
	{Model: "claude-opus-5-5", Name: "Claude Opus 5.5", Input: 4, Output: 20, CacheWrite: 5, CacheRead: 0.20},
	{Model: "claude-opus-5", Name: "Claude Opus 5", Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	{Model: "claude-opus-4-8", Name: "Claude Opus 4.8", Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	{Model: "claude-opus-4-7", Name: "Claude Opus 4.7", Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	{Model: "claude-opus-4-6", Name: "Claude Opus 4.6", Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	{Model: "claude-opus-4-5", Name: "Claude Opus 4.5", Input: 5, Output: 25, CacheWrite: 6.25, CacheRead: 0.50},
	{Model: "claude-opus-4-1", Name: "Claude Opus 4.1", Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50},
	{Model: "claude-opus-4", Name: "Claude Opus 4", Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.50},
	{Model: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", Input: 2, Output: 10, CacheWrite: 2.50, CacheRead: 0.10},
	{Model: "claude-sonnet-5", Name: "Claude Sonnet 5", Input: 2, Output: 10, CacheWrite: 2.50, CacheRead: 0.20},
	{Model: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	{Model: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	{Model: "claude-sonnet-4", Name: "Claude Sonnet 4", Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30},
	{Model: "claude-haiku-5-5", Name: "Claude Haiku 5.5", Input: 0.10, Output: 0.50, CacheWrite: 0.125, CacheRead: 0.01,
		LongOver: 100000, LongInput: 0.50, LongOutput: 2.50},
	{Model: "claude-haiku-4-5", Name: "Claude Haiku 4.5", Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.10},
}

// unknownRate prices a model the table does not know at the highest
// published rate, so a budget is never under-counted.
var unknownRate = Rate{Name: "unknown model (priced at the highest published rate)", Input: 10, Output: 50, CacheWrite: 12.50, CacheRead: 1}

var reDateSuffix = regexp.MustCompile(`-(20\d{6}|latest)$`)

// normModel turns "claude-sonnet-4.5-20250929" / "Claude-Sonnet-4-5" into
// "claude-sonnet-4-5".
func normModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	m = strings.ReplaceAll(m, ".", "-")
	m = strings.ReplaceAll(m, "_", "-")
	m = reDateSuffix.ReplaceAllString(m, "")
	if i := strings.LastIndex(m, "/"); i >= 0 { // gateway prefixes such as "anthropic/claude-…"
		m = m[i+1:]
	}
	return m
}

// RateFor finds the price of a model (longest matching prefix on whole
// segments), else the conservative unknown rate.
func RateFor(model string) Rate {
	m := normModel(model)
	best := Rate{}
	for _, r := range Rates {
		if m == r.Model || strings.HasPrefix(m, r.Model+"-") {
			if len(r.Model) > len(best.Model) {
				best = r
			}
		}
	}
	if best.Model == "" {
		u := unknownRate
		u.Model = m
		return u
	}
	best.Known = true
	return best
}

// Usage is what one or more API calls used.
type Usage struct {
	Input      int
	Output     int
	CacheWrite int
	CacheRead  int
	Searches   int
}

// Cost prices usage with a model's rate: searches, tokens and the total (US$).
func Cost(model string, u Usage) (search, tokens, total float64) {
	r := RateFor(model)
	in, out := r.Input, r.Output
	if r.LongOver > 0 && u.Input+u.CacheWrite+u.CacheRead > r.LongOver {
		in, out = r.LongInput, r.LongOutput
	}
	search = float64(u.Searches) * SearchUSD
	tokens = (float64(u.Input)*in + float64(u.Output)*out + float64(u.CacheWrite)*r.CacheWrite + float64(u.CacheRead)*r.CacheRead) / 1e6
	return round4(search), round4(tokens), round4(search + tokens)
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Run sizing. The input of a scan is the prompt plus the search results the
// model reads (they count as input tokens); the output is the JSON answer.
const (
	PromptTokens         = 3500  // system prompt + profile + focus + not-interested list
	TokensPerSearch      = 6000  // typical search results read per search
	TokensPerSearchWorst = 12000 // a heavy page of results
	LightMaxTokens       = 6000  // output limit of a daily scan
	DeepMaxTokens        = 12000 // output limit of a weekly scan
	MaxSearchesLight     = 10
	MaxSearchesDeep      = 40
)

// MaxTokensFor is the output-token limit of a kind of run.
func MaxTokensFor(kind string) int {
	if kind == KindDeep {
		return DeepMaxTokens
	}
	return LightMaxTokens
}

// WorstCase is the most a run with these limits can cost: every search used,
// heavy results, the whole output budget.
func WorstCase(model string, searches, maxTokens int) float64 {
	return WorstCasePer(model, searches, maxTokens, TokensPerSearchWorst)
}

// WorstCasePer is WorstCase with the input tokens one search may add.
func WorstCasePer(model string, searches, maxTokens, perSearch int) float64 {
	if perSearch < TokensPerSearchWorst {
		perSearch = TokensPerSearchWorst
	}
	_, _, t := Cost(model, Usage{Input: PromptTokens + searches*perSearch, Output: maxTokens, Searches: searches})
	return t
}

// Typical is what a run usually costs (for the monthly estimate).
func Typical(model string, searches, maxTokens int) float64 {
	_, _, t := Cost(model, Usage{Input: PromptTokens + searches*TokensPerSearch, Output: maxTokens / 2, Searches: searches})
	return t
}

// Estimate is the expected monthly cost of a schedule.
type Estimate struct {
	Model          string  `json:"model"`
	ModelName      string  `json:"model_name"`
	PriceKnown     bool    `json:"price_known"`
	InputPerMTok   float64 `json:"input_per_mtok"`
	OutputPerMTok  float64 `json:"output_per_mtok"`
	SearchPer1000  float64 `json:"search_per_1000"`
	DailyRuns      float64 `json:"daily_runs_per_month"`
	WeeklyRuns     float64 `json:"weekly_runs_per_month"`
	DailyRun       float64 `json:"daily_run_usd"`        // typical
	WeeklyRun      float64 `json:"weekly_run_usd"`       // typical
	DailyRunWorst  float64 `json:"daily_run_worst_usd"`  // the most one daily scan can cost
	WeeklyRunWorst float64 `json:"weekly_run_worst_usd"` // the most one weekly scan can cost
	Monthly        float64 `json:"monthly_usd"`          // typical
	MonthlyWorst   float64 `json:"monthly_worst_usd"`
	Budget         float64 `json:"budget_usd"`
	Capped         bool    `json:"capped"` // the worst case is above the budget: the cap will stop scans
	Searches       int     `json:"searches_per_month"`
	Assumptions    string  `json:"assumptions"`
	Source         string  `json:"source"`
}

// EstimateFor prices a schedule with a model.
func EstimateFor(model string, st Settings) Estimate {
	r := RateFor(model)
	e := Estimate{Model: model, ModelName: r.Name, PriceKnown: r.Known, InputPerMTok: r.Input, OutputPerMTok: r.Output, SearchPer1000: SearchUSD * 1000,
		Budget: st.MonthlyBudgetUSD, Source: PricingSource}
	weeks := 30.44 / 7
	if st.WeeklyEnabled {
		e.WeeklyRuns = round2(weeks)
	}
	if st.DailyEnabled {
		e.DailyRuns = 30.44
		if st.WeeklyEnabled {
			e.DailyRuns = round2(30.44 - weeks) // the weekly scan replaces that day's daily scan
		}
	}
	e.DailyRun = round4(Typical(model, st.DailySearches, LightMaxTokens))
	e.WeeklyRun = round4(Typical(model, st.WeeklySearches, DeepMaxTokens))
	e.DailyRunWorst = round4(WorstCase(model, st.DailySearches, LightMaxTokens))
	e.WeeklyRunWorst = round4(WorstCase(model, st.WeeklySearches, DeepMaxTokens))
	e.Monthly = round2(e.DailyRuns*e.DailyRun + e.WeeklyRuns*e.WeeklyRun)
	e.MonthlyWorst = round2(e.DailyRuns*e.DailyRunWorst + e.WeeklyRuns*e.WeeklyRunWorst)
	e.Searches = int(math.Round(e.DailyRuns*float64(st.DailySearches) + e.WeeklyRuns*float64(st.WeeklySearches)))
	e.Capped = e.Budget > 0 && e.MonthlyWorst > e.Budget
	e.Assumptions = fmt.Sprintf("%s at US$%s / US$%s per million input / output tokens and US$10 per 1,000 searches. A typical search adds about %s input tokens of results (up to %s), the prompt about %s; a daily scan writes up to %s output tokens, a weekly scan up to %s. Scans you start with “Scan now” come on top.",
		r.Name, trimNum(r.Input), trimNum(r.Output), money(TokensPerSearch, ""), money(TokensPerSearchWorst, ""), money(PromptTokens, ""), money(LightMaxTokens, ""), money(DeepMaxTokens, ""))
	return e
}

func trimNum(v float64) string {
	s := fmt.Sprintf("%.3f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// FitSearches lowers the search limit until the worst case fits what is left
// of the budget; 0 means not even one search fits.
func FitSearches(model string, want, maxTokens int, left float64) int {
	return FitSearchesPer(model, want, maxTokens, TokensPerSearchWorst, left)
}

// FitSearchesPer is FitSearches with the input tokens one search may add.
func FitSearchesPer(model string, want, maxTokens, perSearch int, left float64) int {
	for n := want; n >= 1; n-- {
		if WorstCasePer(model, n, maxTokens, perSearch) <= left {
			return n
		}
	}
	return 0
}
