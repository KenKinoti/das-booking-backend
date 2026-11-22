package football

// Hit-rate tables from open history: Club Football Match Data (Adam Gábor,
// MIT licence, https://github.com/xgabora/Club-Football-Match-Data-2000-2025),
// whose Matches table is football-data.co.uk results with Bet365 1X2 odds.
// Sample: every match from 1 July 2019 (season 2019/20) to the file's last
// match (3 Sep 2026) with all three odds and a result — 76,528 matches in 38
// divisions. The favourite is the outcome with the highest de-margined
// (proportional: implied probabilities divided by their sum) chance; tiers
// use that chance. Reproduced by TestHistoryTablesFromXgabora
// (FBMODEL_XGABORA=<path to Matches.csv>; skipped by default), which also
// prints this literal with FBMODEL_XGABORA_PRINT=1.

// HistoryTier is one tier's history.
type HistoryTier struct {
	Key             string  `json:"key"`
	Label           string  `json:"label"`
	Range           string  `json:"range"`
	Share           float64 `json:"share"`
	N               int     `json:"n"`
	FavouriteWon    float64 `json:"favourite_won"`
	DoubleChanceWon float64 `json:"double_chance_won"` // the top two outcomes
	PickOrDrawWon   float64 `json:"pick_or_draw_won"`  // the favourite or the draw (draw favourite: draw or the next)
	DrawRate        float64 `json:"draw_rate"`
}

// HistoryGroup is a division group's history (top-5 Europe, rest of
// Europe, rest of the world — the dataset has no African league).
type HistoryGroup struct {
	Group        string        `json:"group"`
	Label        string        `json:"label"`
	N            int           `json:"n"`
	FavouriteWon float64       `json:"favourite_won"`
	DrawRate     float64       `json:"draw_rate"`
	Tiers        []HistoryTier `json:"tiers"`
}

// HistoryScore is an exact score's share.
type HistoryScore struct {
	Score string  `json:"score"`
	Share float64 `json:"share"`
}

// HistoryTables is GET /football/history.
type HistoryTables struct {
	Source    string        `json:"source"`
	URL       string        `json:"url"`
	Licence   string        `json:"licence"`
	From      string        `json:"from"`
	To        string        `json:"to"`
	N         int           `json:"n"`
	Divisions int           `json:"divisions"`
	Method    string        `json:"method"`
	Tiers     []HistoryTier `json:"tiers"`
	Overall   struct {
		FavouriteWon         float64 `json:"favourite_won"`
		DrawRate             float64 `json:"draw_rate"`
		DrawFavouriteMatches int     `json:"draw_favourite_matches"`
		Over25               float64 `json:"over25"`
		BTTS                 float64 `json:"btts"`
	} `json:"overall"`
	Scores []HistoryScore `json:"scores"`
	Groups []HistoryGroup `json:"groups"`
	Note   string         `json:"note"`
}

// HistorySummary is the short attribution carried by other payloads.
type HistorySummary struct {
	Source string `json:"source"`
	URL    string `json:"url"`
	From   string `json:"from"`
	To     string `json:"to"`
	N      int    `json:"n"`
}

// Summary returns the attribution.
func (h HistoryTables) Summary() HistorySummary {
	return HistorySummary{Source: h.Source, URL: h.URL, From: h.From, To: h.To, N: h.N}
}

// Tier returns a tier's row.
func (h HistoryTables) Tier(key string) HistoryTier {
	for _, t := range h.Tiers {
		if t.Key == key {
			return t
		}
	}
	return HistoryTier{Key: key}
}

// historySourceLine is shown with every tier's history.
const historySourceLine = "Club Football Match Data 2019/20–2026 (76,528 matches, 38 divisions)"

// Top-5 European divisions of the dataset (and their API-Football ids).
var histTop5Divs = map[string]bool{"E0": true, "D1": true, "SP1": true, "I1": true, "F1": true}
var histWorldDivs = map[string]bool{"ARG": true, "BRA": true, "CHN": true, "JAP": true, "MEX": true, "USA": true}

func histDivGroup(div string) string {
	switch {
	case histTop5Divs[div]:
		return "top5"
	case histWorldDivs[div]:
		return "world"
	}
	return "europe"
}

var tierMeta = map[string][2]string{"strong": {"Strong", "60% or more"}, "good": {"Good", "50–60%"}, "close": {"Close call", "42–50%"}, "coinflip": {"Coin-flip", "under 42%"}}

// TierKeys in order, strongest first.
var TierKeys = []string{"strong", "good", "close", "coinflip"}

func ht(key string, share float64, n int, fav, dc, pod, draw float64) HistoryTier {
	m := tierMeta[key]
	return HistoryTier{Key: key, Label: m[0], Range: m[1], Share: share, N: n, FavouriteWon: fav, DoubleChanceWon: dc, PickOrDrawWon: pod, DrawRate: draw}
}

// History is the embedded table (computed 22 Nov 2026 from Matches.csv of
// the repository as of 20 Nov 2026; see TestHistoryTablesFromXgabora).
var History = func() HistoryTables {
	h := HistoryTables{
		Source:    "Club Football Match Data (xgabora, MIT) — football-data.co.uk results and Bet365 1X2 odds",
		URL:       "https://github.com/xgabora/Club-Football-Match-Data-2000-2025",
		Licence:   "MIT",
		From:      "2019-07-04",
		To:        "2026-09-03",
		N:         76528,
		Divisions: 38,
		Method:    "Favourite = the outcome with the highest de-margined chance (implied probabilities divided by their sum) from the bookmaker's 1X2 odds; tiers by that chance.",
		Tiers: []HistoryTier{
			ht("strong", 0.1662, 12719, 0.7183, 0.8959, 0.8962, 0.1779),
			ht("good", 0.2201, 16843, 0.5613, 0.8069, 0.8077, 0.2464),
			ht("close", 0.2994, 22916, 0.4610, 0.7523, 0.7462, 0.2853),
			ht("coinflip", 0.3143, 24050, 0.3847, 0.7101, 0.6836, 0.2991),
		},
		Scores: []HistoryScore{{"1-1", 0.1229}, {"1-0", 0.1007}, {"2-1", 0.0889}, {"0-1", 0.0784}, {"0-0", 0.0747}, {"2-0", 0.0708},
			{"1-2", 0.0707}, {"2-2", 0.0539}, {"0-2", 0.0490}, {"3-1", 0.0405}, {"3-0", 0.0391}, {"1-3", 0.0273}},
		Groups: []HistoryGroup{
			{Group: "top5", Label: "Top-5 Europe", N: 12556, FavouriteWon: 0.5361, DrawRate: 0.2524, Tiers: []HistoryTier{
				ht("strong", 0, 3325, 0.7236, 0.8950, 0.8959, 0.1723), ht("good", 0, 2894, 0.5543, 0.8093, 0.8093, 0.2550),
				ht("close", 0, 3195, 0.4673, 0.7527, 0.7527, 0.2858), ht("coinflip", 0, 3142, 0.3908, 0.7072, 0.6922, 0.3008)}},
			{Group: "europe", Label: "Rest of Europe", N: 52207, FavouriteWon: 0.4962, DrawRate: 0.2636, Tiers: []HistoryTier{
				ht("strong", 0, 7743, 0.7212, 0.8986, 0.8989, 0.1777), ht("good", 0, 11155, 0.5625, 0.8050, 0.8065, 0.2439),
				ht("close", 0, 16186, 0.4606, 0.7508, 0.7427, 0.2822), ht("coinflip", 0, 17123, 0.3850, 0.7080, 0.6825, 0.2978)}},
			{Group: "world", Label: "Rest of the world (Americas, Asia)", N: 11765, FavouriteWon: 0.4904, DrawRate: 0.2728, Tiers: []HistoryTier{
				ht("strong", 0, 1651, 0.6941, 0.8849, 0.8843, 0.1902), ht("good", 0, 2794, 0.5637, 0.8121, 0.8110, 0.2473),
				ht("close", 0, 3535, 0.4574, 0.7587, 0.7564, 0.2990), ht("coinflip", 0, 3785, 0.3783, 0.7218, 0.6814, 0.3033)}},
		},
		Note: "Overall the favourite wins about half the time; the draw is the bookmakers' favourite in only 115 of 76,528 matches, yet 3 in 10 coin-flip games end level. " +
			"The market is well calibrated on draws, so DASYIN never 'corrects' market prices. Odds are Bet365's 1X2 prices as published by football-data.co.uk.",
	}
	h.Overall.FavouriteWon, h.Overall.DrawRate, h.Overall.DrawFavouriteMatches, h.Overall.Over25, h.Overall.BTTS = 0.5019, 0.2632, 115, 0.5034, 0.5264
	return h
}()

// ScoreNote is the exact-score honesty line (1-1, the most common score, is
// 12.3% of matches; a most-likely-score pick lands about 1 time in 9).
const ScoreNote = "Exact scores land about 1 time in 9"

// tierOf maps the consensus top probability to a tier.
func tierOf(p float64) string {
	switch {
	case p >= 0.60:
		return "strong"
	case p >= 0.50:
		return "good"
	case p >= 0.42:
		return "close"
	}
	return "coinflip"
}

// tierRank: strong 0 … coinflip 3 (-1 unknown).
func tierRank(k string) int {
	for i, t := range TierKeys {
		if t == k {
			return i
		}
	}
	return -1
}
