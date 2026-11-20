package football

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football/model"
)

// Markets and selections (bets and odds use the same keys).
const (
	Mkt1X2  = "1x2"  // match result: home | draw | away
	MktDC   = "dc"   // double chance: 1x | 12 | x2
	MktDNB  = "dnb"  // draw no bet: home | away (draw = stake back)
	MktOU   = "ou"   // total goals over/under line (quarter lines allowed)
	MktBTTS = "btts" // both teams to score: yes | no
	MktAH   = "ah"   // Asian handicap on the selected team: home | away with line (quarter lines allowed)

	SelHome  = "home"
	SelDraw  = "draw"
	SelAway  = "away"
	Sel1X    = "1x"
	Sel12    = "12"
	SelX2    = "x2"
	SelOver  = "over"
	SelUnder = "under"
	SelYes   = "yes"
	SelNo    = "no"
)

// MarketSelections lists the valid selections per market.
var MarketSelections = map[string][]string{
	Mkt1X2: {SelHome, SelDraw, SelAway}, MktDC: {Sel1X, Sel12, SelX2}, MktDNB: {SelHome, SelAway},
	MktOU: {SelOver, SelUnder}, MktBTTS: {SelYes, SelNo}, MktAH: {SelHome, SelAway},
}

// MarketLabels are plain names.
var MarketLabels = map[string]string{Mkt1X2: "Match result (1X2)", MktDC: "Double chance", MktDNB: "Draw no bet",
	MktOU: "Total goals over/under", MktBTTS: "Both teams to score", MktAH: "Asian handicap"}

// NeedsLine reports whether a market needs a line.
func NeedsLine(m string) bool { return m == MktOU || m == MktAH }

// ValidLine: over/under lines are positive multiples of 0.25; handicaps
// multiples of 0.25 between −5 and +5.
func ValidLine(m string, l float64) bool {
	if math.Abs(l*4-math.Round(l*4)) > 1e-9 {
		return false
	}
	switch m {
	case MktOU:
		return l > 0 && l <= 12
	case MktAH:
		return l >= -5 && l <= 5
	}
	return true
}

// SelectionLabel is "Home win", "Over 2.5", "Away +0.25" …
func SelectionLabel(m, sel string, line *float64, home, away string) string {
	team := func(s string) string {
		if s == SelHome {
			if home != "" {
				return home
			}
			return "Home"
		}
		if away != "" {
			return away
		}
		return "Away"
	}
	switch m {
	case Mkt1X2:
		if sel == SelDraw {
			return "Draw"
		}
		return team(sel) + " to win"
	case MktDC:
		switch sel {
		case Sel1X:
			return team(SelHome) + " or draw"
		case Sel12:
			return team(SelHome) + " or " + team(SelAway)
		case SelX2:
			return "Draw or " + team(SelAway)
		}
	case MktDNB:
		return team(sel) + " (draw no bet)"
	case MktOU:
		l := ""
		if line != nil {
			l = " " + fmtLine(*line, false)
		}
		if sel == SelOver {
			return "Over" + l + " goals"
		}
		return "Under" + l + " goals"
	case MktBTTS:
		if sel == SelYes {
			return "Both teams to score"
		}
		return "Not both teams to score"
	case MktAH:
		l := ""
		if line != nil {
			l = " " + fmtLine(*line, true)
		}
		return team(sel) + l
	}
	return m + " " + sel
}

func fmtLine(l float64, signed bool) string {
	s := strconv.FormatFloat(l, 'f', -1, 64)
	if signed && l > 0 {
		s = "+" + s
	}
	if signed && l == 0 {
		s = "0"
	}
	return s
}

// lineKey is the map key of a line ("2.5", "-0.25").
func lineKey(l *float64) string {
	if l == nil {
		return ""
	}
	return strconv.FormatFloat(*l, 'f', -1, 64)
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// bookmakerSlug normalises a bookmaker name: "Pinnacle" → pinnacle,
// "1xBet" → 1xbet, "Betway (UK)" → betway.
func bookmakerSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "("); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	switch s {
	case "onexbet":
		return "1xbet"
	case "betfair_ex_uk", "betfair_ex_eu", "betfair exchange":
		return "betfair_exchange"
	}
	return strings.Trim(slugRe.ReplaceAllString(s, ""), "")
}

// SharpBooks are preferred for the market's view of the probabilities and
// for closing-line value, in this order.
var SharpBooks = []string{"pinnacle", "betfair_exchange", "matchbook", "smarkets"}

// sharpTitle is how a sharp book is named to people ("Pinnacle (de-margined)").
func sharpTitle(slug string) string {
	switch slug {
	case "pinnacle":
		return "Pinnacle"
	case "betfair_exchange":
		return "Betfair Exchange"
	case "matchbook":
		return "Matchbook"
	case "smarkets":
		return "Smarkets"
	}
	return slug
}

// KenyanBookmakers are suggested when recording a bet or a price seen on an
// app (any name can be typed).
var KenyanBookmakers = []string{"SportPesa", "Betika", "Odibets", "Betway", "1xBet", "Mozzartbet", "Shabiki", "SportyBet", "Betin", "Bangbet", "22Bet", "Melbet", "Dafabet", "Helabet"}

// KenyanIn lists the names of a bookmaker list that match a bookmaker
// operating in Kenya (by slug). International brands such as 1xBet and Betway
// match, but their prices can differ from their Kenyan sites.
func KenyanIn(names []string) []string {
	ke := map[string]bool{}
	for _, k := range KenyanBookmakers {
		ke[bookmakerSlug(k)] = true
	}
	out := []string{}
	seen := map[string]bool{}
	for _, n := range names {
		sl := bookmakerSlug(n)
		if ke[sl] && !seen[sl] {
			out = append(out, n)
			seen[sl] = true
		}
	}
	return out
}

// ---------------------------------------------------------------- API-Football odds

var afHandicapRe = regexp.MustCompile(`(?i)^\s*(home|away)\s*([+-]?\d+(?:\.\d+)?)\s*$`)
var afTotalRe = regexp.MustCompile(`(?i)^\s*(over|under)\s*(\d+(?:\.\d+)?)\s*$`)

// oddsRowsFromAF converts an API-Football /odds entry (bookmakers → bets →
// values) into odds rows. Bets used: "Match Winner", "Home/Away", "Double
// Chance", "Goals Over/Under", "Both Teams Score", "Asian Handicap".
func oddsRowsFromAF(e afOddsEntry, now time.Time) []Odd {
	out := []Odd{}
	upd, ok := parseTime(e.Update)
	var updp *time.Time
	if ok {
		updp = &upd
	}
	for _, b := range e.Bookmakers {
		bk := bookmakerSlug(b.Name)
		for _, bet := range b.Bets {
			name := strings.ToLower(strings.TrimSpace(bet.Name))
			for _, v := range bet.Values {
				price := 0.0
				if f := statNumber(v.Odd); f != nil {
					price = *f
				}
				if price <= 1 || v.Suspended {
					continue
				}
				val := strings.TrimSpace(asString(v.Value))
				r := Odd{FixtureID: e.Fixture.ID, Source: ProviderAPIFootball, Bookmaker: bk, BookmakerTitle: b.Name, Price: price,
					ProviderUpdatedAt: updp, FetchedAt: now, CheckedAt: now}
				if !afSelection(name, val, v.Handicap, &r) {
					continue
				}
				out = append(out, r)
			}
		}
	}
	return out
}

// afSelection maps an API-Football bet name and value to market/selection/line.
func afSelection(bet, val string, handicap *string, r *Odd) bool {
	lv := strings.ToLower(val)
	switch bet {
	case "match winner", "fulltime result", "1x2":
		r.Market = Mkt1X2
		switch lv {
		case "home", "1":
			r.Selection = SelHome
		case "draw", "x":
			r.Selection = SelDraw
		case "away", "2":
			r.Selection = SelAway
		default:
			return false
		}
	case "home/away", "draw no bet":
		r.Market = MktDNB
		switch lv {
		case "home", "1":
			r.Selection = SelHome
		case "away", "2":
			r.Selection = SelAway
		default:
			return false
		}
	case "double chance":
		r.Market = MktDC
		switch lv {
		case "home/draw", "1x":
			r.Selection = Sel1X
		case "home/away", "12":
			r.Selection = Sel12
		case "draw/away", "x2":
			r.Selection = SelX2
		default:
			return false
		}
	case "goals over/under", "over/under":
		m := afTotalRe.FindStringSubmatch(val)
		if m == nil {
			// Live odds: value "Over" with the line in handicap.
			if handicap != nil && (lv == "over" || lv == "under") {
				m = []string{"", lv, *handicap}
			} else {
				return false
			}
		}
		l, err := strconv.ParseFloat(m[2], 64)
		if err != nil || !ValidLine(MktOU, l) {
			return false
		}
		r.Market, r.Selection, r.Line = MktOU, strings.ToLower(m[1]), ptr(l)
	case "both teams score", "both teams to score":
		r.Market = MktBTTS
		switch lv {
		case "yes":
			r.Selection = SelYes
		case "no":
			r.Selection = SelNo
		default:
			return false
		}
	case "asian handicap":
		m := afHandicapRe.FindStringSubmatch(val)
		if m == nil {
			if handicap != nil && (lv == "home" || lv == "away") {
				m = []string{"", lv, *handicap}
			} else {
				return false
			}
		}
		l, err := strconv.ParseFloat(m[2], 64)
		if err != nil || !ValidLine(MktAH, l) {
			return false
		}
		r.Market, r.Selection, r.Line = MktAH, strings.ToLower(m[1]), ptr(l)
	default:
		return false
	}
	return true
}

// ---------------------------------------------------------------- settlement

// halfResult: +1 win, 0 push, −1 loss for a margin.
func halfResult(margin float64) int {
	switch {
	case margin > 1e-9:
		return 1
	case margin < -1e-9:
		return -1
	}
	return 0
}

func isQuarter(l float64) bool {
	q := math.Round(math.Abs(l) * 4)
	return int(q)%2 == 1
}

// combine two half-stakes.
func combine(a, b int) string {
	switch a + b {
	case 2:
		return BetWon
	case 1:
		return BetHalfWon
	case 0:
		if a == 0 {
			return BetVoid
		}
		return BetVoid // win + loss on the halves cannot happen with 0.5 apart
	case -1:
		return BetHalfLost
	}
	return BetLost
}

func lineResult(margin func(l float64) float64, line float64) string {
	if isQuarter(line) {
		return combine(halfResult(margin(line-0.25)), halfResult(margin(line+0.25)))
	}
	switch halfResult(margin(line)) {
	case 1:
		return BetWon
	case -1:
		return BetLost
	}
	return BetVoid
}

// Settle decides a selection from the 90-minute score (home goals, away
// goals): won | lost | void | half_won | half_lost.
func Settle(m, sel string, line *float64, hg, ag int) string {
	yes := func(b bool) string {
		if b {
			return BetWon
		}
		return BetLost
	}
	switch m {
	case Mkt1X2:
		switch sel {
		case SelHome:
			return yes(hg > ag)
		case SelDraw:
			return yes(hg == ag)
		case SelAway:
			return yes(ag > hg)
		}
	case MktDC:
		switch sel {
		case Sel1X:
			return yes(hg >= ag)
		case Sel12:
			return yes(hg != ag)
		case SelX2:
			return yes(ag >= hg)
		}
	case MktDNB:
		if hg == ag {
			return BetVoid
		}
		if sel == SelHome {
			return yes(hg > ag)
		}
		return yes(ag > hg)
	case MktBTTS:
		both := hg > 0 && ag > 0
		if sel == SelYes {
			return yes(both)
		}
		return yes(!both)
	case MktOU:
		if line == nil {
			return ""
		}
		t := float64(hg + ag)
		if sel == SelOver {
			return lineResult(func(l float64) float64 { return t - l }, *line)
		}
		return lineResult(func(l float64) float64 { return l - t }, *line)
	case MktAH:
		if line == nil {
			return ""
		}
		d := float64(hg - ag)
		if sel == SelAway {
			d = -d
		}
		return lineResult(func(l float64) float64 { return d + l }, *line)
	}
	return ""
}

// Payout is what a settled bet returns (stake included) and its profit.
func Payout(status string, odds, stake float64) (ret, pnl float64) {
	switch status {
	case BetWon:
		ret = stake * odds
	case BetHalfWon:
		ret = stake/2*odds + stake/2
	case BetVoid:
		ret = stake
	case BetHalfLost:
		ret = stake / 2
	case BetLost:
		ret = 0
	default:
		return 0, 0
	}
	return round2(ret), round2(ret - stake)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// SelectionProb integrates a selection over a score matrix: W and L are the
// probabilities of winning and losing a full stake (half results count as
// half), V the probability of the stake coming back.
func SelectionProb(matrix [][]float64, m, sel string, line *float64) (W, L, V float64) {
	for i := range matrix {
		for j, p := range matrix[i] {
			if p <= 0 {
				continue
			}
			switch Settle(m, sel, line, i, j) {
			case BetWon:
				W += p
			case BetHalfWon:
				W += p / 2
				V += p / 2
			case BetLost:
				L += p
			case BetHalfLost:
				L += p / 2
				V += p / 2
			case BetVoid:
				V += p
			}
		}
	}
	return
}

// EffP is the probability of winning given money changes hands (pushes
// removed), the input Kelly and "fair odds" need.
func EffP(W, L float64) float64 {
	if W+L <= 0 {
		return 0
	}
	return W / (W + L)
}

// EVOf is the expected profit per unit stake at decimal odds.
func EVOf(W, L, odds float64) float64 { return W*(odds-1) - L }

// ---------------------------------------------------------------- market views

// BookPrice is one bookmaker's latest price for a selection.
type BookPrice struct {
	Bookmaker string     `json:"bookmaker"`
	Title     string     `json:"title"`
	Source    string     `json:"source"`
	Price     float64    `json:"price"`
	UpdatedAt *time.Time `json:"updated_at"`
	FetchedAt time.Time  `json:"fetched_at"`
}

// selKey identifies market + line + selection.
type selKey struct {
	Market, Line, Sel string
}

// groupKey identifies a market + line (all selections priced together).
type groupKey struct{ Market, Line string }

// latestOdds keeps the latest price per source+bookmaker+market+line+selection.
func latestOdds(rows []Odd, before *time.Time) map[string]map[selKey]Odd {
	out := map[string]map[selKey]Odd{} // book (source:bookmaker) → selection → odd
	for _, r := range rows {
		if r.Live {
			continue
		}
		if before != nil && !r.FetchedAt.Before(*before) {
			continue
		}
		book := r.Source + ":" + r.Bookmaker
		k := selKey{r.Market, lineKey(r.Line), r.Selection}
		if out[book] == nil {
			out[book] = map[selKey]Odd{}
		}
		if cur, ok := out[book][k]; !ok || r.FetchedAt.After(cur.FetchedAt) || (r.FetchedAt.Equal(cur.FetchedAt) && r.ID > cur.ID) {
			out[book][k] = r
		}
	}
	return out
}

// fairGroup de-margins one bookmaker's complete set of prices for a market
// group. 1X2 uses the power method, two-way markets proportional
// (model.DeVig; Shin 1993 / power method references in pkg/football/model).
func fairGroup(m string, prices map[string]float64) (map[string]float64, float64, bool) {
	sels := MarketSelections[m]
	if m == MktDC {
		return nil, 0, false // overlapping outcomes: derived from 1X2 instead
	}
	odds := make([]float64, len(sels))
	for i, s := range sels {
		p, ok := prices[s]
		if !ok || p <= 1 {
			return nil, 0, false
		}
		odds[i] = p
	}
	method := "proportional"
	if m == Mkt1X2 {
		method = "power"
	}
	probs, margin := model.DeVig(odds, method)
	out := map[string]float64{}
	for i, s := range sels {
		out[s] = probs[i]
	}
	return out, margin, true
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64{}, xs...)
	sort.Float64s(c)
	if len(c)%2 == 1 {
		return c[len(c)/2]
	}
	return (c[len(c)/2-1] + c[len(c)/2]) / 2
}
