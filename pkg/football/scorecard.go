package football

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The scorecard: DASYIN's graded frozen verdicts made simple — how many
// picks were right, how many their own chances said (with the normal range
// of the Poisson-binomial), the safer pick, goals, both teams to score and
// exact scores, the bookmakers' favourite on the same matches, by tier, by
// competition, per week, and the rows with ✓/✗.

// ScoreQuery filters the scorecard.
type ScoreQuery struct {
	Period      string // weekend | 7d | 30d | season | custom
	From, To    *time.Time
	Competition int64
	Tier        string
	Loc         *time.Location
	Org, User   string
}

// LineStats is one line's graded record.
type LineStats struct {
	Graded       int      `json:"graded"`
	Correct      int      `json:"correct"`
	HitRate      *float64 `json:"hit_rate"`
	Expected     float64  `json:"expected"`
	ExpectedLow  int      `json:"expected_low"`
	ExpectedHigh int      `json:"expected_high"`
}

// TierStats is a tier's record.
type TierStats struct {
	Tier           string   `json:"tier"`
	Label          string   `json:"label"`
	Graded         int      `json:"graded"`
	Correct        int      `json:"correct"`
	HitRate        *float64 `json:"hit_rate"`
	ExpectedRate   *float64 `json:"expected_rate"`
	HistoryHitRate float64  `json:"history_hit_rate"`
}

// CompStats is a competition's record.
type CompStats struct {
	CompetitionID int64    `json:"competition_id"`
	Name          string   `json:"name"`
	Graded        int      `json:"graded"`
	Correct       int      `json:"correct"`
	HitRate       *float64 `json:"hit_rate"`
	ExpectedRate  *float64 `json:"expected_rate"`
}

// WeekStats is one Monday week.
type WeekStats struct {
	WeekStart string  `json:"week_start"`
	Graded    int     `json:"graded"`
	Correct   int     `json:"correct"`
	Expected  float64 `json:"expected"`
	Low       int     `json:"low"`
	High      int     `json:"high"`
}

// ScoreRow is a graded match.
type ScoreRow struct {
	FixtureID   int64            `json:"fixture_id"`
	Kickoff     time.Time        `json:"kickoff"`
	Competition map[string]any   `json:"competition"`
	Home        string           `json:"home"`
	Away        string           `json:"away"`
	Pick        string           `json:"pick"`
	PickLabel   string           `json:"pick_label"`
	P           float64          `json:"p"`
	Tier        string           `json:"tier"`
	TierLabel   string           `json:"tier_label"`
	Safer       string           `json:"safer"`
	SaferLabel  string           `json:"safer_label"`
	Score       string           `json:"score"`
	Outcome     string           `json:"outcome"`
	Status      string           `json:"status"`
	Marks       map[string]*bool `json:"marks"`
	Summary     string           `json:"summary"`
}

// JackpotLine is a graded jackpot in the scorecard.
type JackpotLine struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Bookmaker string     `json:"bookmaker"`
	Correct   int        `json:"correct"`
	Of        int        `json:"of"`
	Expected  float64    `json:"expected"`
	Low       int        `json:"low"`
	High      int        `json:"high"`
	GradedAt  *time.Time `json:"graded_at"`
}

// Scorecard is GET /football/scorecard.
type Scorecard struct {
	Period struct {
		Key      string    `json:"key"`
		From     time.Time `json:"from"`
		To       time.Time `json:"to"`
		Label    string    `json:"label"`
		Timezone string    `json:"timezone"`
	} `json:"period"`
	Totals struct {
		LineStats
		Verdict     string `json:"verdict"`
		VerdictText string `json:"verdict_text"`
		Void        int    `json:"void"`
		NoPick      int    `json:"no_pick"`
	} `json:"totals"`
	Safer       LineStats `json:"safer"`
	Goals       LineStats `json:"goals"`
	BTTS        LineStats `json:"btts"`
	ExactScores struct {
		Graded   int      `json:"graded"`
		Correct  int      `json:"correct"`
		HitRate  *float64 `json:"hit_rate"`
		Expected float64  `json:"expected"`
	} `json:"exact_scores"`
	Favourite struct {
		Graded        int      `json:"graded"`
		Correct       int      `json:"correct"`
		HitRate       *float64 `json:"hit_rate"`
		Agreement     int      `json:"agreement"`
		AgreementRate *float64 `json:"agreement_rate"`
		Note          string   `json:"note"`
	} `json:"favourite_baseline"`
	ByTier        []TierStats    `json:"by_tier"`
	ByCompetition []CompStats    `json:"by_competition"`
	Weekly        []WeekStats    `json:"weekly"`
	Rows          []ScoreRow     `json:"rows"`
	Jackpots      []JackpotLine  `json:"jackpots"`
	History       HistorySummary `json:"history"`
	Note          string         `json:"note"`
	Disclaimer    string         `json:"disclaimer"`
	AsOf          time.Time      `json:"as_of"`
}

func rate(c, n int) *float64 {
	if n == 0 {
		return nil
	}
	return ptr(round4(float64(c) / float64(n)))
}

func lineStats(ps []float64, oks []bool) LineStats {
	ls := LineStats{Graded: len(oks)}
	for _, ok := range oks {
		if ok {
			ls.Correct++
		}
	}
	ls.HitRate = rate(ls.Correct, ls.Graded)
	e, lo, hi := PBRange(ps)
	ls.Expected, ls.ExpectedLow, ls.ExpectedHigh = round2(e), lo, hi
	return ls
}

// weekendOf returns the Friday 00:00 – Tuesday 00:00 window (local) of a
// Fri/Sat/Sun/Mon moment.
func weekendOf(t time.Time, loc *time.Location) (time.Time, time.Time) {
	lt := t.In(loc)
	back := (int(lt.Weekday()) - int(time.Friday) + 7) % 7 // Fri 0, Sat 1, Sun 2, Mon 3
	d := time.Date(lt.Year(), lt.Month(), lt.Day()-back, 0, 0, 0, 0, loc)
	return d, d.AddDate(0, 0, 4)
}

func isWeekendDay(t time.Time, loc *time.Location) bool {
	switch t.In(loc).Weekday() {
	case time.Friday, time.Saturday, time.Sunday, time.Monday:
		return true
	}
	return false
}

// latestWeekend finds the latest Fri–Mon with graded verdicts (zero when none).
func (s *Service) latestWeekend(loc *time.Location, comp int64, tier string) (time.Time, time.Time, bool) {
	q := s.DB.Model(&VerdictRow{}).Where("status IN ('correct','wrong') AND kickoff <= ?", s.Now())
	if comp != 0 {
		q = q.Where("competition_id = ?", comp)
	}
	if tier != "" {
		q = q.Where("tier = ?", tier)
	}
	var kos []time.Time
	q.Order("kickoff DESC").Limit(400).Pluck("kickoff", &kos)
	for _, k := range kos {
		if isWeekendDay(k, loc) {
			a, b := weekendOf(k, loc)
			return a.UTC(), b.UTC(), true
		}
	}
	return time.Time{}, time.Time{}, false
}

func periodLabel(key string, from, to time.Time, loc *time.Location) string {
	f, t := from.In(loc), to.In(loc).Add(-time.Minute)
	switch key {
	case "weekend":
		return "Weekend of " + f.Format("Mon 2") + " – " + t.Format("Mon 2 Jan")
	case "7d":
		return "Last 7 days"
	case "30d":
		return "Last 30 days"
	case "season":
		return "Season so far (since " + f.Format("2 Jan 2006") + ")"
	}
	return f.Format("2 Jan 2006") + " – " + t.Format("2 Jan 2006")
}

// ScorecardFor builds the scorecard.
func (s *Service) ScorecardFor(q ScoreQuery) (*Scorecard, error) {
	loc := q.Loc
	if loc == nil {
		loc, _ = time.LoadLocation("Africa/Nairobi")
		if loc == nil {
			loc = time.UTC
		}
	}
	now := s.Now()
	if q.Tier != "" && tierRank(q.Tier) < 0 {
		return nil, inputErr("tier must be strong, good, close or coinflip.")
	}
	sc := &Scorecard{ByTier: []TierStats{}, ByCompetition: []CompStats{}, Weekly: []WeekStats{}, Rows: []ScoreRow{}, Jackpots: []JackpotLine{},
		History: History.Summary(), Disclaimer: Disclaimer, AsOf: now,
		Note: "Win rate depends mostly on which games and markets you play: strong favourites win about 7 in 10, coin-flips about 4 in 10, whatever the formula. " +
			"Picks are frozen before kickoff and never regraded."}
	key := q.Period
	if key == "" {
		key = "weekend"
		if q.From != nil || q.To != nil {
			key = "custom"
		}
	}
	var from, to time.Time
	switch key {
	case "weekend":
		a, b, ok := s.latestWeekend(loc, q.Competition, q.Tier)
		if !ok {
			a, b = weekendOf(now, loc)
			if !isWeekendDay(now, loc) {
				a, b = a.AddDate(0, 0, -7), b.AddDate(0, 0, -7)
			}
		}
		from, to = a, b
	case "7d":
		from, to = now.AddDate(0, 0, -7), now
	case "30d":
		from, to = now.AddDate(0, 0, -30), now
	case "season":
		y := now.In(loc).Year()
		if now.In(loc).Month() < time.July {
			y--
		}
		from, to = time.Date(y, time.July, 1, 0, 0, 0, 0, loc), now
	case "custom":
		if q.From == nil {
			return nil, inputErr("Choose a start date (from) for a custom period.")
		}
		from = *q.From
		to = now
		if q.To != nil {
			to = q.To.AddDate(0, 0, 1) // the "to" day is included
		}
		if !to.After(from) {
			return nil, inputErr("The end date must be after the start date.")
		}
	default:
		return nil, inputErr("period must be weekend, 7d, 30d, season or custom.")
	}
	sc.Period.Key, sc.Period.From, sc.Period.To, sc.Period.Timezone = key, from.UTC(), to.UTC(), loc.String()
	sc.Period.Label = periodLabel(key, from, to, loc)

	rows := s.gradedRows(from, to, q.Competition, q.Tier)
	var ps, sps, gps, bps []float64
	var oks, soks, goks, boks []bool
	tierAgg := map[string]*[3]float64{} // graded, correct, Σp
	compAgg := map[int64]*[3]float64{}
	for _, r := range rows {
		if r.Status == "void" {
			sc.Totals.Void++
			continue
		}
		ps, oks = append(ps, r.P), append(oks, r.PickOK != nil && *r.PickOK)
		if r.SaferOK != nil {
			sps, soks = append(sps, r.SaferP), append(soks, *r.SaferOK)
		}
		if r.GoalsOK != nil && r.GoalsP != nil {
			gps, goks = append(gps, *r.GoalsP), append(goks, *r.GoalsOK)
		}
		if r.BTTSOK != nil && r.BTTSP != nil {
			bps, boks = append(bps, *r.BTTSP), append(boks, *r.BTTSOK)
		}
		if r.ScoreOK != nil {
			sc.ExactScores.Graded++
			sc.ExactScores.Expected += r.TopScoreP
			if *r.ScoreOK {
				sc.ExactScores.Correct++
			}
		}
		if r.FavOK != nil {
			sc.Favourite.Graded++
			if *r.FavOK {
				sc.Favourite.Correct++
			}
			if r.FavPick == r.Pick {
				sc.Favourite.Agreement++
			}
		}
		t := tierAgg[r.Tier]
		if t == nil {
			t = &[3]float64{}
			tierAgg[r.Tier] = t
		}
		c := compAgg[r.CompetitionID]
		if c == nil {
			c = &[3]float64{}
			compAgg[r.CompetitionID] = c
		}
		for _, a := range []*[3]float64{t, c} {
			a[0]++
			a[2] += r.P
			if r.PickOK != nil && *r.PickOK {
				a[1]++
			}
		}
	}
	sc.Totals.LineStats = lineStats(ps, oks)
	sc.Totals.Verdict, sc.Totals.VerdictText = RangeVerdict(sc.Totals.Correct, sc.Totals.Graded, sc.Totals.Expected, sc.Totals.ExpectedLow, sc.Totals.ExpectedHigh, "picks")
	sc.Safer, sc.Goals, sc.BTTS = lineStats(sps, soks), lineStats(gps, goks), lineStats(bps, boks)
	sc.ExactScores.HitRate = rate(sc.ExactScores.Correct, sc.ExactScores.Graded)
	sc.ExactScores.Expected = round2(sc.ExactScores.Expected)
	sc.Favourite.HitRate = rate(sc.Favourite.Correct, sc.Favourite.Graded)
	sc.Favourite.AgreementRate = rate(sc.Favourite.Agreement, sc.Favourite.Graded)
	sc.Favourite.Note = "The bookmakers' favourite on the same matches (de-margined price when the pick was frozen)."
	for _, k := range TierKeys {
		ts := TierStats{Tier: k, Label: History.Tier(k).Label, HistoryHitRate: History.Tier(k).FavouriteWon}
		if a := tierAgg[k]; a != nil {
			ts.Graded, ts.Correct = int(a[0]), int(a[1])
			ts.HitRate, ts.ExpectedRate = rate(ts.Correct, ts.Graded), ptr(round4(a[2]/a[0]))
		}
		sc.ByTier = append(sc.ByTier, ts)
	}
	for id, a := range compAgg {
		sc.ByCompetition = append(sc.ByCompetition, CompStats{CompetitionID: id, Name: s.compName(id), Graded: int(a[0]), Correct: int(a[1]),
			HitRate: rate(int(a[1]), int(a[0])), ExpectedRate: ptr(round4(a[2] / a[0]))})
	}
	sort.Slice(sc.ByCompetition, func(i, j int) bool {
		if sc.ByCompetition[i].Graded != sc.ByCompetition[j].Graded {
			return sc.ByCompetition[i].Graded > sc.ByCompetition[j].Graded
		}
		return sc.ByCompetition[i].Name < sc.ByCompetition[j].Name
	})
	sc.Totals.NoPick = s.noPickCount(from, to, q.Competition)
	sc.Weekly = s.weeklySeries(to, loc, q.Competition, q.Tier)
	// Rows, newest first.
	for i := len(rows) - 1; i >= 0 && len(sc.Rows) < 300; i-- {
		sc.Rows = append(sc.Rows, s.scoreRow(rows[i]))
	}
	if q.Org != "" {
		sc.Jackpots = s.jackpotLines(q.Org, q.User, from, to)
	}
	return sc, nil
}

func (s *Service) gradedRows(from, to time.Time, comp int64, tier string) []VerdictRow {
	q := s.DB.Where("graded_at IS NOT NULL AND status IN ('correct','wrong','void') AND kickoff >= ? AND kickoff < ?", from, to)
	if comp != 0 {
		q = q.Where("competition_id = ?", comp)
	}
	if tier != "" {
		q = q.Where("tier = ?", tier)
	}
	var rows []VerdictRow
	q.Order("kickoff, fixture_id").Find(&rows)
	return rows
}

// noPickCount: finished matches of enabled competitions without a frozen pick.
func (s *Service) noPickCount(from, to time.Time, comp int64) int {
	var n int64
	q := s.DB.Table("fb_fixtures f").Joins("JOIN fb_competitions c ON c.id = f.competition_id AND c.enabled").
		Where("f.status IN ('FT','AET','PEN') AND f.kickoff >= ? AND f.kickoff < ? AND f.id > 0", from, to).
		Where("NOT EXISTS (SELECT 1 FROM fb_verdicts v WHERE v.fixture_id = f.id)")
	if comp != 0 {
		q = q.Where("f.competition_id = ?", comp)
	}
	q.Count(&n)
	return int(n)
}

// weeklySeries: the 26 Monday weeks up to the period's end.
func (s *Service) weeklySeries(to time.Time, loc *time.Location, comp int64, tier string) []WeekStats {
	lt := to.In(loc)
	wd := (int(lt.Weekday()) + 6) % 7
	end := time.Date(lt.Year(), lt.Month(), lt.Day()-wd, 0, 0, 0, 0, loc).AddDate(0, 0, 7)
	start := end.AddDate(0, 0, -7*26)
	rows := s.gradedRows(start, end, comp, tier)
	type wk struct {
		ps  []float64
		cor int
	}
	weeks := map[string]*wk{}
	for _, r := range rows {
		if r.Status == "void" {
			continue
		}
		k := r.Kickoff.In(loc)
		d := (int(k.Weekday()) + 6) % 7
		key := time.Date(k.Year(), k.Month(), k.Day()-d, 0, 0, 0, 0, loc).Format("2006-01-02")
		w := weeks[key]
		if w == nil {
			w = &wk{}
			weeks[key] = w
		}
		w.ps = append(w.ps, r.P)
		if r.PickOK != nil && *r.PickOK {
			w.cor++
		}
	}
	keys := []string{}
	for k := range weeks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []WeekStats{}
	for _, k := range keys {
		w := weeks[k]
		e, lo, hi := PBRange(w.ps)
		out = append(out, WeekStats{WeekStart: k, Graded: len(w.ps), Correct: w.cor, Expected: round2(e), Low: lo, High: hi})
	}
	return out
}

func (s *Service) scoreRow(r VerdictRow) ScoreRow {
	var f Fixture
	s.DB.Where("id = ?", r.FixtureID).Limit(1).Find(&f)
	sr := ScoreRow{FixtureID: r.FixtureID, Kickoff: r.Kickoff, Competition: map[string]any{"id": r.CompetitionID, "name": s.compName(r.CompetitionID)},
		Home: s.teamName(f.HomeID), Away: s.teamName(f.AwayID), Pick: r.Pick, PickLabel: r.Verdict.PickLabel, P: r.P, Tier: r.Tier,
		TierLabel: History.Tier(r.Tier).Label, Safer: r.Safer, SaferLabel: r.Verdict.Safer.Label, Score: r.Score, Outcome: r.Outcome, Status: r.Status,
		Marks: map[string]*bool{"pick": r.PickOK, "safer": r.SaferOK, "goals": r.GoalsOK, "btts": r.BTTSOK, "exact_score": r.ScoreOK}}
	if r.Check != nil {
		sr.Summary = r.Check.Summary
	}
	return sr
}

// LastWeekendBanner is the match centre's slim banner (nil when nothing
// was graded in the last three weeks).
func (s *Service) LastWeekendBanner(loc *time.Location) map[string]any {
	a, b, ok := s.latestWeekend(loc, 0, "")
	if !ok || s.Now().Sub(b) > 21*24*time.Hour {
		return nil
	}
	rows := s.gradedRows(a, b, 0, "")
	ps, oks := []float64{}, []bool{}
	for _, r := range rows {
		if r.Status != "void" {
			ps, oks = append(ps, r.P), append(oks, r.PickOK != nil && *r.PickOK)
		}
	}
	ls := lineStats(ps, oks)
	_, text := RangeVerdict(ls.Correct, ls.Graded, ls.Expected, ls.ExpectedLow, ls.ExpectedHigh, "picks")
	label := a.In(loc).Format("Mon 2") + " – " + b.In(loc).Add(-time.Minute).Format("Mon 2 Jan")
	return map[string]any{"from": a, "to": b, "label": label, "graded": ls.Graded, "correct": ls.Correct, "expected": ls.Expected,
		"low": ls.ExpectedLow, "high": ls.ExpectedHigh, "verdict_text": text}
}

// ---------------------------------------------------------------- picks

// PickRow is an upcoming verdict.
type PickRow struct {
	FixtureID   int64          `json:"fixture_id"`
	Kickoff     time.Time      `json:"kickoff"`
	Competition map[string]any `json:"competition"`
	Home        TeamRef        `json:"home"`
	Away        TeamRef        `json:"away"`
	Status      string         `json:"status"`
	Verdict     *Verdict       `json:"verdict"`
}

// Picks lists upcoming verdicts, strongest first.
func (s *Service) Picks(from, to *time.Time, minTier string, comp int64, limit int, org, user string, loc *time.Location) (map[string]any, error) {
	if minTier != "" && tierRank(minTier) < 0 {
		return nil, inputErr("min_tier must be strong, good, close or coinflip.")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	now := s.Now()
	if from == nil || from.Before(now) {
		from = &now
	}
	rows, err := s.Fixtures(FixtureQuery{From: from, To: to, Competition: comp, Status: "upcoming", Limit: 300, Org: org, User: user, TZ: loc, MinTier: minTier})
	if err != nil {
		return nil, err
	}
	out := []PickRow{}
	for _, r := range rows {
		if r.Verdict == nil {
			continue
		}
		out = append(out, PickRow{FixtureID: r.ID, Kickoff: r.Kickoff, Competition: map[string]any{"id": r.Competition.ID, "name": r.Competition.Name, "country": r.Competition.Country},
			Home: r.Home, Away: r.Away, Status: r.Status, Verdict: r.Verdict})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Verdict.P != out[j].Verdict.P {
			return out[i].Verdict.P > out[j].Verdict.P
		}
		return out[i].Kickoff.Before(out[j].Kickoff)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	var ex any
	if len(out) >= 2 {
		n := minInt(3, len(out))
		p := 1.0
		for _, r := range out[:n] {
			p *= r.Verdict.P
		}
		ex = map[string]any{"picks": n, "p": round4(p), "fair_odds": fairOdds(p), "one_in": int(1/p + 0.5)}
	}
	return map[string]any{"picks": out, "count": len(out), "disclaimer": Disclaimer,
		"accumulator": map[string]any{"note": "A combo wins only if every pick wins: multiply the chances (this assumes the games are independent).", "example": ex}}, nil
}

// plainTier is "Strong (wins about 7 in 10 in history)".
func plainTier(k string) string {
	h := History.Tier(k)
	return fmt.Sprintf("%s (in history the pick won %.0f%%)", h.Label, h.FavouriteWon*100)
}

var _ = strings.TrimSpace
