package football

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// OddsPapi v4 (https://oddspapi.io/en/docs; host https://api.oddspapi.io;
// "All endpoints require an API key to be passed as a request parameter"
// — apiKey). Free tier: 250 requests a month, every bookmaker including
// Betika KE (slug "betika", https://oddspapi.io/sportsbooks/betika) and
// Pinnacle. Endpoints used:
//   GET /v4/fixtures?sportId=10&from=…&to=…&bookmakers=…&hasOdds=true
//       (https://oddspapi.io/en/docs/get-fixtures — sportId with from/to
//        under 10 days apart; 2 s cooldown) → [{fixtureId, participant1Name,
//        participant2Name, tournamentId, tournamentName, categoryName, startTime, statusId, …}]
//   GET /v4/odds-by-tournaments?bookmaker=betika&tournamentIds=17,8
//       (https://oddspapi.io/en/docs/get-odds-by-tournament) → [{fixtureId,
//        startTime, bookmakerOdds:{betika:{markets:{"101":{outcomes:{"101":{players:{"0":{price, changedAt}}}}}}}}}]
//   GET /v4/odds?fixtureId=…&bookmakers=… (https://oddspapi.io/en/docs/get-odds; 500 ms cooldown)
// Market 101 is the full-time 1X2 with outcomes 101 home / 102 draw / 103
// away (bookmakerOutcomeId "home" / "draw" / "away" in the docs' example).

// OddsPapi soccer.
const opSportID = 10

type opFixture struct {
	FixtureID        string `json:"fixtureId"`
	Participant1ID   int64  `json:"participant1Id"`
	Participant2ID   int64  `json:"participant2Id"`
	Participant1Name string `json:"participant1Name"`
	Participant2Name string `json:"participant2Name"`
	TournamentID     int64  `json:"tournamentId"`
	TournamentName   string `json:"tournamentName"`
	TournamentSlug   string `json:"tournamentSlug"`
	CategoryName     string `json:"categoryName"`
	StatusID         int    `json:"statusId"`
	StartTime        string `json:"startTime"`
	UpdatedAt        string `json:"updatedAt"`
	BookmakerOdds    map[string]struct {
		BookmakerIsActive bool `json:"bookmakerIsActive"`
		Markets           map[string]struct {
			Outcomes map[string]struct {
				Players map[string]struct {
					Active             *bool   `json:"active"`
					BookmakerOutcomeID string  `json:"bookmakerOutcomeId"`
					ChangedAt          string  `json:"changedAt"`
					Price              float64 `json:"price"`
				} `json:"players"`
			} `json:"outcomes"`
		} `json:"markets"`
	} `json:"bookmakerOdds"`
}

// decodeOP accepts a bare array, a single object or {"data": …}.
func decodeOP(body []byte) ([]opFixture, error) {
	b := strings.TrimSpace(string(body))
	var arr []opFixture
	if strings.HasPrefix(b, "[") {
		if err := json.Unmarshal([]byte(b), &arr); err != nil {
			return nil, fmt.Errorf("unexpected OddsPapi response: %w", err)
		}
		return arr, nil
	}
	var wrap struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(b), &wrap) == nil && len(wrap.Data) > 0 {
		return decodeOP(wrap.Data)
	}
	var one opFixture
	if err := json.Unmarshal([]byte(b), &one); err != nil {
		return nil, fmt.Errorf("unexpected OddsPapi response: %w", err)
	}
	if one.FixtureID == "" {
		return []opFixture{}, nil
	}
	return []opFixture{one}, nil
}

// opTitle names a bookmaker slug.
func opTitle(slug string) string {
	switch slug {
	case "betika":
		return "Betika (KE)"
	case "pinnacle":
		return "Pinnacle"
	case "sportpesa":
		return "SportPesa"
	case "odibets":
		return "Odibets"
	case "betway":
		return "Betway"
	case "1xbet":
		return "1xBet"
	}
	if slug == "" {
		return ""
	}
	return strings.ToUpper(slug[:1]) + slug[1:]
}

// KenyanOP are OddsPapi slugs of bookmakers operating in Kenya (others are
// shown, but not as "Kenyan").
var KenyanOP = map[string]bool{"betika": true, "sportpesa": true, "odibets": true, "mozzartbet": true, "shabiki": true, "betwaykenya": true, "betway-ke": true}

// OPOddsRows converts OddsPapi odds into odds rows (1X2 only) for fixtures
// already matched (fixtureId → our fixture id).
func OPOddsRows(fs []opFixture, matched map[string]int64, now time.Time) []Odd {
	out := []Odd{}
	for _, f := range fs {
		fid := matched[f.FixtureID]
		if fid == 0 {
			continue
		}
		for slug, bo := range f.BookmakerOdds {
			if !bo.BookmakerIsActive && len(bo.Markets) == 0 {
				continue
			}
			m, ok := bo.Markets["101"]
			if !ok {
				continue
			}
			for oid, oc := range m.Outcomes {
				p, ok := oc.Players["0"]
				if !ok || p.Price <= 1 || (p.Active != nil && !*p.Active) {
					continue
				}
				sel := ""
				switch oid {
				case "101":
					sel = SelHome
				case "102":
					sel = SelDraw
				case "103":
					sel = SelAway
				}
				switch strings.ToLower(p.BookmakerOutcomeID) {
				case "home", "1":
					sel = SelHome
				case "draw", "x":
					sel = SelDraw
				case "away", "2":
					sel = SelAway
				}
				if sel == "" {
					continue
				}
				r := Odd{FixtureID: fid, Source: ProviderOddsPapi, Bookmaker: bookmakerSlug(slug), BookmakerTitle: opTitle(slug), Market: Mkt1X2, Selection: sel,
					Price: p.Price, FetchedAt: now, CheckedAt: now}
				if t, ok := parseTime(p.ChangedAt); ok {
					r.ProviderUpdatedAt = &t
				}
				out = append(out, r)
			}
		}
	}
	return out
}

// fetchOPFixtures discovers OddsPapi fixtures in a window (one request) and
// matches them to API-Football fixtures.
func (s *Service) fetchOPFixtures(ctx context.Context, from, to time.Time, prio int, task string) (int, int, error) {
	st := s.FreeSettingsGet()
	if to.Sub(from) >= 10*24*time.Hour {
		to = from.Add(10*24*time.Hour - time.Hour)
	}
	p := url.Values{"sportId": {strconv.Itoa(opSportID)}, "from": {from.UTC().Format("2006-01-02T15:04:05Z")}, "to": {to.UTC().Format("2006-01-02T15:04:05Z")}}
	if b := strings.TrimSpace(st.OddsPapiBookmakers); b != "" {
		p.Set("bookmakers", b)
		p.Set("hasOdds", "true")
	}
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderOddsPapi, Path: "/v4/fixtures", Params: p, Prio: prio, Task: task})
	if err != nil {
		return 0, 0, err
	}
	fs, err := decodeOP(out.Body)
	if err != nil {
		return 0, 0, err
	}
	s.setResults(ProviderOddsPapi, task, len(fs))
	n, matched := s.storeOPFixtures(fs)
	return n, matched, nil
}

func (s *Service) storeOPFixtures(fs []opFixture) (int, int) {
	now := s.Now()
	if len(fs) == 0 {
		return 0, 0
	}
	minT, maxT := time.Time{}, time.Time{}
	for _, f := range fs {
		if t, ok := parseTime(f.StartTime); ok {
			if minT.IsZero() || t.Before(minT) {
				minT = t
			}
			if t.After(maxT) {
				maxT = t
			}
		}
	}
	cands := []Candidate{}
	for _, c := range s.fixtureCands(minT.Add(-MatchWindow), maxT.Add(MatchWindow)) {
		cands = append(cands, Candidate{FixtureID: c.ID, Kickoff: c.Kickoff, HomeID: c.HomeID, AwayID: c.AwayID, Home: c.Home, Away: c.Away})
	}
	aliases := s.aliasMap()
	matched := 0
	for _, f := range fs {
		t, ok := parseTime(f.StartTime)
		if !ok || f.FixtureID == "" {
			continue
		}
		row := OPFixture{ID: clip(f.FixtureID, 64), TournamentID: f.TournamentID, Start: t, Home: clip(f.Participant1Name, 160), Away: clip(f.Participant2Name, 160),
			HomePID: f.Participant1ID, AwayPID: f.Participant2ID, FetchedAt: now}
		var prev OPFixture
		if s.DB.Where("id = ?", row.ID).Limit(1).Find(&prev).RowsAffected > 0 {
			row.FixtureID, row.OddsAt = prev.FixtureID, prev.OddsAt
		}
		if row.FixtureID == 0 && row.Home != "" && row.Away != "" {
			m := matchFree(row.Home, row.Away, t, cands, aliases)
			if m.FixtureID != 0 {
				row.FixtureID = m.FixtureID
				for _, c := range cands {
					if c.FixtureID == m.FixtureID {
						s.learnAliasFrom(row.Home, c.HomeID, ProviderOddsPapi)
						s.learnAliasFrom(row.Away, c.AwayID, ProviderOddsPapi)
					}
				}
			}
		}
		if row.FixtureID != 0 {
			matched++
		} else {
			// Record names nobody could map (for an alias), with the best guess at that kickoff.
			for _, side := range []struct {
				name string
				home bool
			}{{row.Home, true}, {row.Away, false}} {
				best, bestName, bestID := 0.0, "", int64(0)
				for _, c := range cands {
					if absDur(c.Kickoff.Sub(t)) > MatchWindow {
						continue
					}
					n, id := c.Away, c.AwayID
					if side.home {
						n, id = c.Home, c.HomeID
					}
					if v := TeamSim(side.name, n); v > best {
						best, bestName, bestID = v, n, id
					}
				}
				if best < 0.6 && side.name != "" {
					s.noteUnmatched(ProviderOddsPapi, side.name, f.CategoryName, bestName, bestID)
				}
			}
		}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row)
		if f.TournamentID != 0 {
			s.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{"name", "category", "slug", "fetched_at"})}).
				Create(&OPTournament{ID: f.TournamentID, Name: clip(f.TournamentName, 160), Slug: clip(f.TournamentSlug, 120), Category: clip(f.CategoryName, 120), FetchedAt: now})
		}
	}
	s.DB.Where("start < ?", now.AddDate(0, 0, -7)).Delete(&OPFixture{})
	return len(fs), matched
}

// matchFree is MatchEvent with the messy-name similarity (TeamSim): kickoff
// within ±15 minutes, both names ≥ 0.6, together ≥ 1.4.
func matchFree(home, away string, at time.Time, cands []Candidate, aliases map[string]int64) MatchResult {
	best := MatchResult{}
	for _, c := range cands {
		d := c.Kickoff.Sub(at)
		if d < -MatchWindow || d > MatchWindow {
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
		score := sh + sa - absDur(d).Minutes()/1000
		if sh >= 0.6 && sa >= 0.6 && score >= 1.4 && score > best.Score {
			best = MatchResult{FixtureID: c.FixtureID, Score: score}
		}
	}
	return best
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// fetchOPOdds gets one bookmaker's prices for some tournaments (one request)
// and stores the 1X2 prices of matched fixtures.
func (s *Service) fetchOPOdds(ctx context.Context, bookmaker string, tournaments []int64, prio int, task string) (int, error) {
	ids := make([]string, len(tournaments))
	for i, t := range tournaments {
		ids[i] = strconv.FormatInt(t, 10)
	}
	out, err := s.callFree(ctx, freeSpec{Provider: ProviderOddsPapi, Path: "/v4/odds-by-tournaments", Prio: prio, Task: task,
		Params: url.Values{"bookmaker": {bookmaker}, "tournamentIds": {strings.Join(ids, ",")}, "oddsFormat": {"decimal"}}})
	if err != nil {
		return 0, err
	}
	fs, err := decodeOP(out.Body)
	if err != nil {
		return 0, err
	}
	s.setResults(ProviderOddsPapi, task, len(fs))
	matched := map[string]int64{}
	opIDs := []string{}
	for _, f := range fs {
		opIDs = append(opIDs, f.FixtureID)
	}
	var rows []OPFixture
	s.DB.Where("id IN ? AND fixture_id <> 0", append(opIDs, "")).Find(&rows)
	for _, r := range rows {
		matched[r.ID] = r.FixtureID
	}
	now := s.Now()
	odds := OPOddsRows(fs, matched, now)
	n, _, err := s.storeOdds(odds)
	if err != nil {
		return 0, err
	}
	fixed := map[int64]bool{}
	for _, o := range odds {
		fixed[o.FixtureID] = true
	}
	if len(fixed) > 0 {
		s.DB.Model(&OPFixture{}).Where("fixture_id IN ?", keys(fixed)).Update("odds_at", now)
	}
	return n, nil
}

// opTarget is a fixture that needs Kenyan bookmakers' prices.
type opTarget struct {
	FixtureID  int64
	Kickoff    time.Time
	Prio       int
	Why        string
	OPID       string
	Tournament int64
	OddsAt     *time.Time
}

// opTargets: jackpot games (until the jackpot closes), watched fixtures and
// teams (next 3 days), and the next 24 h of top-priority competitions.
func (s *Service) opTargets(now time.Time) []opTarget {
	type row struct {
		ID      int64
		Kickoff time.Time
		Why     string
		Prio    int
	}
	var rows []row
	s.DB.Raw(`SELECT DISTINCT f.id, f.kickoff, 'jackpot' AS why, 1 AS prio FROM fb_jackpot_games g JOIN fb_jackpots j ON j.id = g.jackpot_id JOIN fb_fixtures f ON f.id = g.fixture_id
			WHERE f.kickoff BETWEEN ? AND ? AND (j.closes_at IS NULL OR j.closes_at > ?) AND j.status <> 'graded'
		UNION SELECT DISTINCT f.id, f.kickoff, 'watched' AS why, 1 AS prio FROM fb_watchlist w JOIN fb_fixtures f ON
			((w.kind = 'fixture' AND f.id = w.ref_id) OR (w.kind = 'team' AND (f.home_id = w.ref_id OR f.away_id = w.ref_id)))
			WHERE f.kickoff BETWEEN ? AND ?
		UNION SELECT f.id, f.kickoff, 'top competition' AS why, 2 AS prio FROM fb_fixtures f JOIN fb_competitions c ON c.id = f.competition_id
			WHERE c.enabled AND c.priority <= 3 AND f.kickoff BETWEEN ? AND ?`,
		now, now.Add(4*24*time.Hour), now, now, now.Add(3*24*time.Hour), now, now.Add(24*time.Hour)).Scan(&rows)
	best := map[int64]row{}
	for _, r := range rows {
		if b, ok := best[r.ID]; !ok || r.Prio < b.Prio {
			best[r.ID] = r
		}
	}
	ids := []int64{}
	for id := range best {
		ids = append(ids, id)
	}
	var ops []OPFixture
	if len(ids) > 0 {
		s.DB.Where("fixture_id IN ?", ids).Find(&ops)
	}
	byFix := map[int64]OPFixture{}
	for _, o := range ops {
		byFix[o.FixtureID] = o
	}
	out := []opTarget{}
	for _, r := range best {
		t := opTarget{FixtureID: r.ID, Kickoff: r.Kickoff, Prio: r.Prio, Why: r.Why}
		if o, ok := byFix[r.ID]; ok {
			t.OPID, t.Tournament, t.OddsAt = o.ID, o.TournamentID, o.OddsAt
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Prio != out[j].Prio {
			return out[i].Prio < out[j].Prio
		}
		return out[i].Kickoff.Before(out[j].Kickoff)
	})
	return out
}

// opNeedsOdds: a snapshot when none yet (within 48 h), every 12 h within
// 24 h, and once in the last 3 hours before kickoff.
func opNeedsOdds(t opTarget, now time.Time) bool {
	until := t.Kickoff.Sub(now)
	if until <= 0 || until > 48*time.Hour {
		return false
	}
	if t.OddsAt == nil {
		return true
	}
	age := now.Sub(*t.OddsAt)
	if until <= 3*time.Hour {
		return t.OddsAt.Before(t.Kickoff.Add(-3 * time.Hour))
	}
	return until <= 24*time.Hour && age >= 12*time.Hour
}

// KenyanOddsOut is the match page's Kenyan bookmakers section.
type KenyanOddsOut struct {
	Available  bool                   `json:"available"`
	Source     string                 `json:"source"`
	Bookmakers []KenyanBook           `json:"bookmakers"`
	Fair       map[string]float64     `json:"fair"`
	FairSource string                 `json:"fair_source"`
	Best       map[string]*KenyanBest `json:"best"`
	Note       string                 `json:"note"`
	Budget     map[string]any         `json:"budget"`
}

// KenyanBook is one bookmaker's 1X2 prices.
type KenyanBook struct {
	Key       string             `json:"key"`
	Title     string             `json:"title"`
	Kenyan    bool               `json:"kenyan"`
	Prices    map[string]float64 `json:"prices"`
	Margin    *float64           `json:"margin"`
	Fair      map[string]float64 `json:"fair"`
	VsFair    map[string]float64 `json:"vs_fair"`
	FetchedAt time.Time          `json:"fetched_at"`
	UpdatedAt *time.Time         `json:"updated_at"`
	Stale     bool               `json:"stale"`
	URL       string             `json:"url"`
}

// KenyanBest is the best price of a selection.
type KenyanBest struct {
	Bookmaker string  `json:"bookmaker"`
	Price     float64 `json:"price"`
}

// kenyanOddsFor builds the Kenyan bookmakers section from OddsPapi prices
// (non-sharp books) against the market's fair (sharp) probabilities.
func (s *Service) kenyanOddsFor(f Fixture, mv MarketView) KenyanOddsOut {
	st := s.FreeSettingsGet()
	mo := s.quotaRow(ProviderOddsPapi, monthPeriod(s.Now()))
	out := KenyanOddsOut{Source: ProviderOddsPapi, Bookmakers: []KenyanBook{}, Best: map[string]*KenyanBest{},
		Budget: map[string]any{"used": mo.Used, "budget": st.Budgets[ProviderOddsPapi], "period": "month"},
		Note: "Kenyan bookmakers' prices from OddsPapi (free tier: fetched for watched fixtures, jackpot games and the next 24 h of top competitions). " +
			"vs fair = price × the sharp fair probability − 1: negative means the price is below fair."}
	if len(mv.Fair1X2) == 3 {
		out.Fair = map[string]float64{"home": round4(mv.Fair1X2[SelHome]), "draw": round4(mv.Fair1X2[SelDraw]), "away": round4(mv.Fair1X2[SelAway])}
		out.FairSource = mv.FairSource
	}
	var rows []Odd
	s.DB.Raw(`SELECT DISTINCT ON (bookmaker, selection) * FROM fb_odds WHERE fixture_id = ? AND source = ? AND market = ? AND NOT live
		ORDER BY bookmaker, selection, fetched_at DESC, id DESC`, f.ID, ProviderOddsPapi, Mkt1X2).Scan(&rows)
	by := map[string]*KenyanBook{}
	order := []string{}
	for _, r := range rows {
		if isSharpSlug(r.Bookmaker) {
			continue
		}
		b := by[r.Bookmaker]
		if b == nil {
			b = &KenyanBook{Key: r.Bookmaker, Title: r.BookmakerTitle, Kenyan: KenyanOP[r.Bookmaker] || len(KenyanIn([]string{r.Bookmaker})) > 0,
				Prices: map[string]float64{}, URL: "https://oddspapi.io/sportsbooks/" + r.Bookmaker}
			by[r.Bookmaker] = b
			order = append(order, r.Bookmaker)
		}
		b.Prices[r.Selection] = r.Price
		last := r.CheckedAt
		if last.IsZero() {
			last = r.FetchedAt
		}
		if last.After(b.FetchedAt) {
			b.FetchedAt = last
		}
		if r.ProviderUpdatedAt != nil && (b.UpdatedAt == nil || r.ProviderUpdatedAt.After(*b.UpdatedAt)) {
			b.UpdatedAt = r.ProviderUpdatedAt
		}
	}
	sort.Strings(order)
	now := s.Now()
	for _, k := range order {
		b := by[k]
		if len(b.Prices) == 3 {
			p, m := DeMarginProportional([3]float64{b.Prices[SelHome], b.Prices[SelDraw], b.Prices[SelAway]})
			mm := round4(m)
			b.Margin = &mm
			b.Fair = map[string]float64{"home": round4(p[0]), "draw": round4(p[1]), "away": round4(p[2])}
		}
		if out.Fair != nil {
			b.VsFair = map[string]float64{}
			for sel, pr := range b.Prices {
				b.VsFair[sel] = round4(pr*out.Fair[sel] - 1)
			}
		}
		b.Stale = f.Kickoff.After(now) && now.Sub(b.FetchedAt) > 12*time.Hour
		for sel, pr := range b.Prices {
			if cur := out.Best[sel]; cur == nil || pr > cur.Price {
				out.Best[sel] = &KenyanBest{Bookmaker: b.Key, Price: pr}
			}
		}
		out.Bookmakers = append(out.Bookmakers, *b)
	}
	out.Available = len(out.Bookmakers) > 0
	if !out.Available {
		if !s.FreeOn(ProviderOddsPapi) {
			out.Note = "OddsPapi is off (System settings → Football data → Free sources). Prices seen on a Kenyan app can still be typed in by hand."
		} else {
			out.Note = "No Kenyan bookmaker prices from OddsPapi for this match yet (fetched for watched fixtures, jackpot games and the next 24 h of top competitions)."
		}
	}
	return out
}

func isSharpSlug(slug string) bool {
	for _, x := range SharpBooks {
		if x == slug {
			return true
		}
	}
	return false
}

var _ = gorm.ErrRecordNotFound
