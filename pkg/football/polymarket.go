package football

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// Polymarket (Gamma API, public, no key — https://docs.polymarket.com,
// OpenAPI https://docs.polymarket.com/api-spec/gamma-openapi.yaml, server
// https://gamma-api.polymarket.com) as a consensus source: crowd prices,
// thin markets ignored.
//
//   - GET /sports → SportsMetadata[] {sport, image, resolution, ordering,
//     tags ("1,82,306,…" comma-separated tag ids), series}; cached a day.
//   - GET /events?tag_id=<league tag>&closed=false&limit=100 → Event[] with
//     markets[]; a match is one event ("Arsenal FC vs. Chelsea FC") with a
//     binary Yes/No market per outcome (home win / draw / away win). Market
//     fields used: outcomes and outcomePrices (JSON-encoded string arrays,
//     same index = same outcome), bestBid, bestAsk, spread, liquidityNum /
//     liquidity, gameStartTime, sportsMarketType, groupItemTitle, question.
//     A single market listing the three outcomes is handled too.
//   - Mid price = (bestBid + bestAsk) / 2 per market, used only when the
//     market's liquidity ≥ the minimum (5,000 USD) and its spread ≤ the
//     maximum (0.04); the three mids are normalised to 1.
//   - Matched to fixtures by team aliases (provider "polymarket") and names
//     with kickoff ± 3 h. At most one discovery pass per hour; off by default.

// PMWindow is how far a Polymarket start time may be from the kickoff.
const PMWindow = 3 * time.Hour

// flexStr accepts a JSON string or number.
type flexStr string

func (f *flexStr) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexStr(s)
		return nil
	}
	*f = flexStr(string(b))
	return nil
}

// flexNum accepts a JSON number or a numeric string.
type flexNum struct {
	V  float64
	OK bool
}

func (f *flexNum) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	s := strings.Trim(string(b), `"`)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	f.V, f.OK = v, true
	return nil
}

type pmMarket struct {
	ID               flexStr `json:"id"`
	Question         string  `json:"question"`
	Slug             string  `json:"slug"`
	GroupItemTitle   string  `json:"groupItemTitle"`
	Outcomes         flexStr `json:"outcomes"`
	OutcomePrices    flexStr `json:"outcomePrices"`
	BestBid          flexNum `json:"bestBid"`
	BestAsk          flexNum `json:"bestAsk"`
	Spread           flexNum `json:"spread"`
	Liquidity        flexNum `json:"liquidity"`
	LiquidityNum     flexNum `json:"liquidityNum"`
	GameStartTime    string  `json:"gameStartTime"`
	SportsMarketType string  `json:"sportsMarketType"`
	Active           *bool   `json:"active"`
	Closed           *bool   `json:"closed"`
}

type pmEvent struct {
	ID        flexStr    `json:"id"`
	Slug      string     `json:"slug"`
	Title     string     `json:"title"`
	StartTime string     `json:"startTime"`
	StartDate string     `json:"startDate"`
	EventDate string     `json:"eventDate"`
	EndDate   string     `json:"endDate"`
	Closed    *bool      `json:"closed"`
	Markets   []pmMarket `json:"markets"`
}

// PMMarketOut is a market as shown in the Sources detail.
type PMMarketOut struct {
	Outcome   string   `json:"outcome"`
	Question  string   `json:"question"`
	Mid       *float64 `json:"mid"`
	BestBid   *float64 `json:"best_bid"`
	BestAsk   *float64 `json:"best_ask"`
	Spread    *float64 `json:"spread"`
	Liquidity *float64 `json:"liquidity"`
	Used      bool     `json:"used"`
	Why       string   `json:"why"`
}

// PMParsed is a parsed match event.
type PMParsed struct {
	ID, Slug, Title string
	Home, Away      string
	Kickoff         time.Time
	HasKickoff      bool
	Probs           P3
	Usable          bool
	Why             string
	Markets         []PMMarketOut
}

func jsonStrings(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err == nil {
		return out
	}
	var nums []float64
	if err := json.Unmarshal([]byte(s), &nums); err == nil {
		for _, n := range nums {
			out = append(out, strconv.FormatFloat(n, 'f', -1, 64))
		}
	}
	return out
}

var pmVsRe = regexp.MustCompile(`(?i)\s+(?:vs\.?|v\.?)\s+`)
var pmWillRe = regexp.MustCompile(`(?i)^will\s+|\s+win\b.*$|\?$`)

// pmTeams splits "Arsenal FC vs. Chelsea FC" (companion events such as
// "… - Halftime Result" are not full-time result markets).
func pmTeams(title string) (string, string, bool) {
	parts := pmVsRe.Split(strings.TrimSpace(title), 2)
	if len(parts) != 2 {
		return "", "", false
	}
	away := parts[1]
	if i := strings.Index(away, " - "); i >= 0 {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(away), true
}

func pmLiquidity(m pmMarket) (float64, bool) {
	if m.LiquidityNum.OK {
		return m.LiquidityNum.V, true
	}
	return m.Liquidity.V, m.Liquidity.OK
}

// ParsePMEvent turns a Gamma event into match probabilities (pure).
func ParsePMEvent(e pmEvent, minLiq, maxSpread float64) PMParsed {
	out := PMParsed{ID: string(e.ID), Slug: e.Slug, Title: e.Title, Markets: []PMMarketOut{}}
	h, a, ok := pmTeams(e.Title)
	if !ok {
		out.Why = "not a match-result event"
		return out
	}
	out.Home, out.Away = h, a
	for _, ts := range []string{e.StartTime, e.EventDate, e.EndDate} {
		if ts == "" {
			continue
		}
		if t, ok := flexTime(ts); ok {
			out.Kickoff, out.HasKickoff = t, true
			break
		}
	}
	for _, m := range e.Markets {
		if m.GameStartTime != "" {
			if t, ok := flexTime(m.GameStartTime); ok {
				out.Kickoff, out.HasKickoff = t, true
				break
			}
		}
	}
	classify := func(label string) string {
		l := strings.ToLower(label)
		if strings.Contains(l, "draw") || strings.Contains(l, "tie") {
			return "X"
		}
		name := strings.TrimSpace(pmWillRe.ReplaceAllString(strings.TrimSpace(label), ""))
		sh, sa := TeamSim(name, h), TeamSim(name, a)
		switch {
		case sh >= 0.6 && sh > sa:
			return "1"
		case sa >= 0.6 && sa > sh:
			return "2"
		}
		return ""
	}
	got := map[string]*PMMarketOut{}
	price := map[string]float64{}
	reject := ""
	for _, m := range e.Markets {
		if m.Closed != nil && *m.Closed {
			continue
		}
		if t := strings.ToLower(m.SportsMarketType); t != "" && t != "moneyline" {
			continue
		}
		outs := jsonStrings(string(m.Outcomes))
		prices := jsonStrings(string(m.OutcomePrices))
		liq, liqOK := pmLiquidity(m)
		mo := PMMarketOut{Question: firstNonEmpty(m.Question, m.GroupItemTitle)}
		if liqOK {
			mo.Liquidity = ptr(math.Round(liq*100) / 100)
		}
		if m.BestBid.OK {
			mo.BestBid = ptr(m.BestBid.V)
		}
		if m.BestAsk.OK {
			mo.BestAsk = ptr(m.BestAsk.V)
		}
		spread, spOK := m.Spread.V, m.Spread.OK
		if m.BestBid.OK && m.BestAsk.OK && m.BestAsk.V > 0 {
			spread, spOK = m.BestAsk.V-m.BestBid.V, true
		}
		if spOK {
			mo.Spread = ptr(math.Round(spread*10000) / 10000)
		}
		check := func(mo *PMMarketOut) bool {
			switch {
			case !liqOK || liq < minLiq:
				mo.Why = fmt.Sprintf("liquidity under $%s", fmtThousands(minLiq))
			case !spOK:
				mo.Why = "no bid/ask spread"
			case spread > maxSpread+1e-9:
				mo.Why = fmt.Sprintf("spread %.3f above %.3f", spread, maxSpread)
			default:
				return true
			}
			return false
		}
		if len(outs) >= 3 {
			// One market listing the outcomes.
			for i, o := range outs {
				k := classify(o)
				if k == "" || i >= len(prices) {
					continue
				}
				v, _ := strconv.ParseFloat(prices[i], 64)
				x := mo
				x.Outcome, x.Question = k, mo.Question+" — "+o
				x.Mid = ptr(v)
				if check(&x) {
					x.Used = true
					price[k] = v
				} else if reject == "" {
					reject = x.Why
				}
				got[k] = &x
			}
			continue
		}
		k := classify(firstNonEmpty(m.GroupItemTitle, m.Question))
		if k == "" {
			continue
		}
		mo.Outcome = k
		var mid float64
		midOK := false
		if m.BestBid.OK && m.BestAsk.OK && m.BestAsk.V > 0 {
			mid, midOK = (m.BestBid.V+m.BestAsk.V)/2, true
		} else {
			for i, o := range outs {
				if strings.EqualFold(o, "yes") && i < len(prices) {
					if v, err := strconv.ParseFloat(prices[i], 64); err == nil {
						mid, midOK = v, true
					}
				}
			}
		}
		if midOK {
			mo.Mid = ptr(math.Round(mid*10000) / 10000)
		}
		if midOK && check(&mo) {
			mo.Used = true
			price[k] = mid
		} else {
			if mo.Why == "" {
				mo.Why = "no price"
			}
			if reject == "" {
				reject = mo.Why
			}
		}
		if prev, ok := got[k]; !ok || (!prev.Used && mo.Used) {
			x := mo
			got[k] = &x
		}
	}
	for _, k := range []string{"1", "X", "2"} {
		if m, ok := got[k]; ok {
			out.Markets = append(out.Markets, *m)
		}
	}
	if len(price) < 3 {
		switch {
		case len(got) < 3:
			out.Why = "home / draw / away markets not all listed"
		default:
			out.Why = "thin market ignored: " + reject
		}
		return out
	}
	p := P3{price["1"], price["X"], price["2"]}
	s := p[0] + p[1] + p[2]
	if s <= 0 {
		out.Why = "no prices"
		return out
	}
	for k := range p {
		p[k] /= s
	}
	out.Probs, out.Usable = p, true
	return out
}

// pmLeagues parses "epl:39,lal:140".
func pmLeagues(s string) map[string]int64 {
	out := map[string]int64{}
	for _, part := range splitList(s, nil) {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
		if err == nil && id > 0 {
			out[strings.ToLower(strings.TrimSpace(kv[0]))] = id
		}
	}
	return out
}

// pmTagFor picks a sport's league tag: the tag unique to that sport among
// all sports (generic tags such as "sports" or "soccer" are shared); the
// last tag when none is unique.
func pmTagFor(sports []PMSport, sport string) string {
	count := map[string]int{}
	var mine []string
	for _, sp := range sports {
		tags := splitList(sp.Tags, nil)
		for _, t := range tags {
			count[strings.TrimSpace(t)]++
		}
		if strings.EqualFold(sp.Sport, sport) {
			mine = tags
		}
	}
	if len(mine) == 0 {
		return ""
	}
	pick := ""
	for _, t := range mine {
		if count[strings.TrimSpace(t)] == 1 {
			pick = strings.TrimSpace(t)
		}
	}
	if pick == "" {
		pick = strings.TrimSpace(mine[len(mine)-1])
	}
	return pick
}

// fetchPMSports refreshes the GET /sports cache.
func (s *Service) fetchPMSports(ctx context.Context, prio int, task string) (int, error) {
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderPolymarket, Path: "/sports", Prio: prio, Task: task, Accept: "application/json"})
	if err != nil {
		return 0, err
	}
	var sp []PMSport
	if err := json.Unmarshal(out.Body, &sp); err != nil {
		return 0, fmt.Errorf("unexpected /sports answer: %w", err)
	}
	s.setResults(ProviderPolymarket, task, len(sp))
	now := s.Now()
	s.DB.Model(&WinSettings{}).Where("id = 1").Updates(map[string]any{"poly_sports": mustJSON(sp), "poly_sports_at": now})
	return len(sp), nil
}

// fetchPMEvents gets a league's open events, matches them and stores
// usable prices.
func (s *Service) fetchPMEvents(ctx context.Context, sport string, comp int64, prio int, task string) (int, int, error) {
	st := s.WinSettingsGet()
	tag := pmTagFor(st.PolySports, sport)
	if tag == "" {
		return 0, 0, fmt.Errorf("Polymarket lists no sport %q", sport)
	}
	now := s.Now()
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderPolymarket, Path: "/events", Prio: prio, Task: task, Accept: "application/json",
		Params: url.Values{"tag_id": {tag}, "closed": {"false"}, "limit": {"100"}, "end_date_min": {now.Add(-3 * time.Hour).Format(time.RFC3339)}}})
	if err != nil {
		return 0, 0, err
	}
	var evs []pmEvent
	if err := json.Unmarshal(out.Body, &evs); err != nil {
		// /events/keyset style: {events: [...]}.
		var wrap struct {
			Events []pmEvent `json:"events"`
		}
		if err2 := json.Unmarshal(out.Body, &wrap); err2 != nil {
			return 0, 0, fmt.Errorf("unexpected /events answer: %w", err)
		}
		evs = wrap.Events
	}
	s.setResults(ProviderPolymarket, task, len(evs))
	n, matched := s.storePMEvents(evs, sport, st)
	s.DB.Model(&WinSettings{}).Where("id = 1").Update("last_poly_at", now)
	return n, matched, nil
}

func (s *Service) storePMEvents(evs []pmEvent, sport string, st WinSettings) (int, int) {
	now := s.Now()
	parsed := []PMParsed{}
	minT, maxT := time.Time{}, time.Time{}
	for _, e := range evs {
		p := ParsePMEvent(e, st.PolyMinLiquidity, st.PolyMaxSpread)
		if p.Home == "" || !p.HasKickoff {
			continue
		}
		if p.Kickoff.Before(now.Add(-3*time.Hour)) || p.Kickoff.After(now.Add(8*24*time.Hour)) {
			continue
		}
		parsed = append(parsed, p)
		if minT.IsZero() || p.Kickoff.Before(minT) {
			minT = p.Kickoff
		}
		if p.Kickoff.After(maxT) {
			maxT = p.Kickoff
		}
	}
	if len(parsed) == 0 {
		return 0, 0
	}
	cands := []Candidate{}
	for _, c := range s.fixtureCands(minT.Add(-PMWindow), maxT.Add(PMWindow)) {
		cands = append(cands, Candidate{FixtureID: c.ID, Kickoff: c.Kickoff, HomeID: c.HomeID, AwayID: c.AwayID, Home: c.Home, Away: c.Away})
	}
	aliases := s.aliasMap()
	matched := 0
	for _, p := range parsed {
		row := PMEvent{ID: clip(p.ID, 40), Slug: clip(p.Slug, 200), Title: clip(p.Title, 300), Sport: sport, Home: clip(p.Home, 160), Away: clip(p.Away, 160),
			Start: p.Kickoff, Usable: p.Usable, Why: clip(p.Why, 200), FetchedAt: now}
		var prev PMEvent
		if s.DB.Where("id = ?", row.ID).Limit(1).Find(&prev).RowsAffected > 0 && prev.FixtureID != 0 {
			row.FixtureID = prev.FixtureID
		}
		if row.FixtureID == 0 {
			m := matchWithin(p.Home, p.Away, p.Kickoff, cands, aliases, PMWindow)
			if m.FixtureID != 0 {
				row.FixtureID = m.FixtureID
				for _, c := range cands {
					if c.FixtureID == m.FixtureID {
						s.learnAliasFrom(p.Home, c.HomeID, ProviderPolymarket)
						s.learnAliasFrom(p.Away, c.AwayID, ProviderPolymarket)
					}
				}
			} else {
				s.noteUnmatched(ProviderPolymarket, p.Home, sport, "", 0)
				s.noteUnmatched(ProviderPolymarket, p.Away, sport, "", 0)
			}
		}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row)
		if row.FixtureID == 0 {
			continue
		}
		matched++
		if !p.Usable {
			s.DB.Where("fixture_id = ? AND source = ?", row.FixtureID, ProviderPolymarket).Delete(&SourceProb{})
			continue
		}
		used := 0
		for _, m := range p.Markets {
			if m.Used {
				used++
			}
		}
		sp := SourceProb{FixtureID: row.FixtureID, Source: ProviderPolymarket, PHome: round4(p.Probs[0]), PDraw: round4(p.Probs[1]), PAway: round4(p.Probs[2]),
			Detail: map[string]any{"event_id": p.ID, "slug": p.Slug, "title": p.Title, "markets": p.Markets, "n_markets": used},
			URL:    "https://polymarket.com/event/" + p.Slug, FetchedAt: now}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&sp)
	}
	s.DB.Where("start < ?", now.AddDate(0, 0, -14)).Delete(&PMEvent{})
	return len(parsed), matched
}

// matchWithin is matchFree with a custom kickoff window.
func matchWithin(home, away string, at time.Time, cands []Candidate, aliases map[string]int64, window time.Duration) MatchResult {
	best := MatchResult{}
	for _, c := range cands {
		d := c.Kickoff.Sub(at)
		if d < -window || d > window {
			continue
		}
		side := func(name string, id int64, fix string) float64 {
			for _, k := range []string{normFree(name), NormTeam(name)} {
				if t, ok := aliases[k]; ok && k != "" {
					if t == id {
						return 1
					}
					return 0
				}
			}
			return TeamSim(name, fix)
		}
		sh, sa := side(home, c.HomeID, c.Home), side(away, c.AwayID, c.Away)
		score := sh + sa - absDur(d).Minutes()/10000
		if sh >= 0.6 && sa >= 0.6 && score >= 1.4 && score > best.Score {
			best = MatchResult{FixtureID: c.FixtureID, Score: score}
		}
	}
	return best
}

// pmTargets: mapped leagues whose competition is enabled and plays in the
// next 4 days.
func (s *Service) pmTargets(now time.Time) map[string]int64 {
	st := s.WinSettingsGet()
	out := map[string]int64{}
	for sport, comp := range pmLeagues(st.PolyLeagues) {
		var n int64
		s.DB.Model(&Fixture{}).Joins("JOIN fb_competitions c ON c.id = fb_fixtures.competition_id AND c.enabled").
			Where("fb_fixtures.competition_id = ? AND fb_fixtures.kickoff BETWEEN ? AND ?", comp, now, now.Add(4*24*time.Hour)).Count(&n)
		if n > 0 {
			out[sport] = comp
		}
	}
	return out
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
