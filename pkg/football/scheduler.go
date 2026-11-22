package football

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RunSummary is what one scheduler pass (or manual refresh) did.
type RunSummary struct {
	At         time.Time      `json:"at"`
	Trigger    string         `json:"trigger"`
	DurationMS int64          `json:"duration_ms"`
	Planned    int            `json:"planned"`
	Done       int            `json:"done"`
	Skipped    int            `json:"skipped"`
	Failed     int            `json:"failed"`
	Cost       map[string]int `json:"cost"`
	Tasks      []TaskRun      `json:"tasks"`
	Fitted     int            `json:"fitted"`
	Predicted  int            `json:"predicted"`
	Frozen     int            `json:"frozen"`
	Closed     int            `json:"closed"`
	Graded     int            `json:"graded"`
	Settled    int            `json:"settled"`
	Notes      []string       `json:"notes"`
}

// TaskRun is one task's outcome.
type TaskRun struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Provider string `json:"provider"`
	Prio     int    `json:"prio"`
	Status   string `json:"status"` // ok | skipped | failed
	Cost     int    `json:"cost"`
	Note     string `json:"note,omitempty"`
}

// RunOpts controls RunOnce.
type RunOpts struct {
	Trigger string
	Force   bool            // ignore intervals (budgets still apply)
	Only    map[string]bool // task kinds
	NoFetch bool            // only the internal work (models, freeze, grading, settlement)
	Refit   bool            // refit every competition's models now
}

// StartScheduler runs a pass every 30 seconds on the instance holding the
// advisory lock (FOOTBALL_SCHEDULER=off disables it).
func (s *Service) StartScheduler(ctx context.Context) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("FOOTBALL_SCHEDULER")), "off") {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Second):
		}
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("football: scheduler pass failed: %v", r)
					}
				}()
				st, err := s.Settings()
				if err != nil || !st.SchedulerOn {
					return
				}
				af, oa := s.Keys()
				_, err = s.RunOnce(ctx, RunOpts{Trigger: "schedule", NoFetch: af == "" && oa == ""})
				if err != nil && !errors.Is(err, ErrBusy) {
					log.Printf("football: scheduler: %v", err)
				}
			}()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// withLock runs fn while holding the Postgres advisory lock (one instance
// fetches at a time).
func (s *Service) withLock(ctx context.Context, key string, fn func()) bool {
	got := false
	err := s.DB.WithContext(ctx).Connection(func(tx *gorm.DB) error {
		if err := tx.Raw("SELECT pg_try_advisory_lock(hashtext(?))", key).Scan(&got).Error; err != nil {
			return err
		}
		if !got {
			return nil
		}
		defer tx.Exec("SELECT pg_advisory_unlock(hashtext(?))", key)
		fn()
		return nil
	})
	if err != nil {
		log.Printf("football: lock %s: %v", key, err)
		return false
	}
	return got
}

// RunOnce plans and runs the due fetches, then fits models, refreshes
// predictions, freezes those about to kick off, captures closing odds,
// grades finished matches and settles bets.
func (s *Service) RunOnce(ctx context.Context, o RunOpts) (RunSummary, error) {
	if !s.runMu.TryLock() {
		return RunSummary{}, ErrBusy
	}
	defer s.runMu.Unlock()
	var sum RunSummary
	var runErr error
	ok := s.withLock(ctx, "football:scheduler", func() { sum, runErr = s.runLocked(ctx, o) })
	if !ok {
		return sum, ErrBusy
	}
	return sum, runErr
}

func (s *Service) runLocked(ctx context.Context, o RunOpts) (RunSummary, error) {
	started := time.Now()
	sum := RunSummary{At: s.Now(), Trigger: o.Trigger, Cost: map[string]int{}, Tasks: []TaskRun{}, Notes: []string{}}
	if !o.NoFetch {
		in, err := s.planInput(o)
		if err != nil {
			return sum, err
		}
		tasks := Plan(in)
		sum.Planned = len(tasks)
		blocked := map[string]int{} // provider → lowest priority already refused
		for _, t := range tasks {
			if ctx.Err() != nil {
				break
			}
			if p, ok := blocked[t.Provider]; ok && t.Prio >= p {
				sum.Skipped++
				if len(sum.Tasks) < 80 {
					sum.Tasks = append(sum.Tasks, TaskRun{Key: t.Key, Kind: t.Kind, Provider: t.Provider, Prio: t.Prio, Status: "skipped", Note: "budget"})
				}
				continue
			}
			tr := s.runTask(ctx, t)
			switch tr.Status {
			case "ok":
				sum.Done++
			case "skipped":
				sum.Skipped++
				if strings.HasPrefix(tr.Note, "budget") || strings.HasPrefix(tr.Note, "backoff") {
					if p, ok := blocked[t.Provider]; !ok || t.Prio < p {
						blocked[t.Provider] = t.Prio
					}
				}
			default:
				sum.Failed++
			}
			sum.Cost[t.Provider] += tr.Cost
			if len(sum.Tasks) < 80 {
				sum.Tasks = append(sum.Tasks, tr)
			}
		}
	}
	// v2.17 free sources (ClubElo, football-data.co.uk / .org, OddsPapi,
	// news), seeded-fixture dedupe and jackpot grading (freeplan.go). They
	// run even when no paid key is saved.
	if !o.NoFetch || o.Trigger == "schedule" {
		s.runFree(ctx, o, &sum)
	}
	// Internal work (no provider calls).
	sum.Fitted = s.FitDue(o.Refit)
	sum.Predicted = s.PredictUpcoming()
	sum.Frozen = s.FreezeDue()
	s.winPass(&sum, "freeze") // v2.18: verdicts frozen / refreshed until kickoff, jackpots frozen at close (win_freeze.go)
	sum.Closed = s.CaptureClosing()
	sum.Graded = s.GradeFinished()
	s.winPass(&sum, "grade") // v2.18: ✓/✗ marks, weekly learned consensus weights
	sum.Settled = s.AutoSettle()
	sum.DurationMS = time.Since(started).Milliseconds()
	now := s.Now()
	if o.Trigger == "manual" || sum.Planned > 0 || sum.Graded+sum.Settled+sum.Frozen+sum.Closed > 0 {
		s.DB.Model(&Settings{}).Where("id = 1").Updates(map[string]any{"last_run_at": now, "last_run": mustJSON(sum)})
		s.invalidateSettings()
	}
	// Forget old one-off task states; keep 90 days of the fetch log.
	s.DB.Where("last_run_at < ? AND (key LIKE 'af:h2h:%' OR key LIKE 'af:pred:%' OR key LIKE 'af:date:%')", now.AddDate(0, 0, -30)).Delete(&TaskState{})
	s.DB.Where("at < ?", now.AddDate(0, 0, -90)).Delete(&FetchLog{})
	return sum, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// planInput loads the planner's view of the database.
func (s *Service) planInput(o RunOpts) (PlanInput, error) {
	st, err := s.Settings()
	if err != nil {
		return PlanInput{}, err
	}
	af, oa := s.Keys()
	now := s.Now()
	in := PlanInput{Now: now, Settings: st, HasAF: af != "", HasOA: oa != "", State: map[string]TaskState{}, StandingsAt: map[int64]time.Time{},
		LastFinished: map[int64]time.Time{}, PlayersAt: map[int64]time.Time{}, Force: o.Force, Only: o.Only}
	ids := []int64{}
	for _, c := range s.Competitions(true) {
		in.Comps = append(in.Comps, PlanComp{ID: c.ID, Season: guessSeason(c, now), SportKey: c.OddsSportKey, Type: c.Type, Coverage: c.Coverage, Known: c.Known})
		ids = append(ids, c.ID)
	}
	if len(ids) == 0 {
		return in, nil
	}
	var fx []Fixture
	s.DB.Where("competition_id IN ? AND kickoff BETWEEN ? AND ?", ids, now.Add(-96*time.Hour), now.Add(time.Duration(st.LookaheadDays+1)*24*time.Hour)).
		Order("kickoff").Find(&fx)
	for _, f := range fx {
		in.Fixtures = append(in.Fixtures, PlanFixture{ID: f.ID, Comp: f.CompetitionID, Season: f.Season, Kickoff: f.Kickoff, Status: f.Status, HomeID: f.HomeID,
			AwayID: f.AwayID, DetailsAt: f.DetailsAt, LineupsAt: f.LineupsAt, InjuriesAt: f.InjuriesAt, PredRefAt: f.PredRefAt, H2HAt: f.H2HAt, OddsOAAt: f.OddsOAAt})
	}
	var states []TaskState
	s.DB.Find(&states)
	for _, t := range states {
		in.State[t.Key] = t
	}
	var rows []struct {
		ID int64
		At time.Time
	}
	s.DB.Raw(`SELECT competition_id AS id, MAX(fetched_at) AS at FROM fb_standings WHERE source = ? AND competition_id IN ? GROUP BY 1`, ProviderAPIFootball, ids).Scan(&rows)
	for _, r := range rows {
		in.StandingsAt[r.ID] = r.At
	}
	rows = nil
	s.DB.Raw(`SELECT competition_id AS id, MAX(finished_at) AS at FROM fb_fixtures WHERE finished_at IS NOT NULL AND competition_id IN ? GROUP BY 1`, ids).Scan(&rows)
	for _, r := range rows {
		in.LastFinished[r.ID] = r.At
	}
	rows = nil
	s.DB.Raw(`SELECT team_id AS id, MAX(fetched_at) AS at FROM fb_player_seasons GROUP BY 1`).Scan(&rows)
	for _, r := range rows {
		in.PlayersAt[r.ID] = r.At
	}
	s.DB.Raw(`SELECT f.id FROM fb_fixtures f JOIN fb_competitions c ON c.id = f.competition_id
		WHERE c.enabled AND f.status IN ('FT','AET','PEN') AND f.details_at IS NULL AND f.kickoff > ?
		AND (NOT c.known OR c.coverage LIKE '%"statistics_fixtures":true%') ORDER BY f.kickoff DESC LIMIT 60`, now.AddDate(-2, 0, 0)).Scan(&in.Backlog)
	day := dayPeriod(now)
	in.AFUsedToday = s.quotaRow(ProviderAPIFootball, day).Used
	var live int64
	s.DB.Model(&FetchLog{}).Where("provider = ? AND task LIKE 'af:live:%' AND at >= ?", ProviderAPIFootball, now.Truncate(24*time.Hour)).Count(&live)
	in.LiveUsedToday = int(live)
	return in, nil
}

func (s *Service) saveTaskState(key string, ok bool, note string) {
	now := s.Now()
	ts := TaskState{Key: key, LastRunAt: now, Status: "ok", Note: clip(note, 300)}
	if ok {
		ts.LastOKAt = &now
		s.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}},
			DoUpdates: clause.Assignments(map[string]any{"last_run_at": now, "last_ok_at": now, "status": "ok", "fails": 0, "note": ts.Note})}).Create(&ts)
		return
	}
	ts.Status, ts.Fails = "failed", 1
	s.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}},
		DoUpdates: clause.Assignments(map[string]any{"last_run_at": now, "status": "failed", "fails": gorm.Expr("fb_task_state.fails + 1"), "note": ts.Note})}).Create(&ts)
}

// runTask executes one task.
func (s *Service) runTask(ctx context.Context, t Task) TaskRun {
	tr := TaskRun{Key: t.Key, Kind: t.Kind, Provider: t.Provider, Prio: t.Prio}
	spec := callSpec{Provider: t.Provider, Endpoint: t.Endpoint, Params: t.Params, Cost: t.Cost, Prio: t.Prio, Task: t.Key, Free: t.Cost == 0}
	note, cost, err := s.execute(ctx, t, spec)
	tr.Cost = cost
	if err != nil {
		var pe *ProviderError
		if errors.As(err, &pe) && (pe.Kind == "budget" || pe.Kind == "backoff" || pe.Kind == "no_key") {
			tr.Status, tr.Note = "skipped", pe.Kind+": "+pe.Msg
			return tr
		}
		tr.Status, tr.Note = "failed", clip(err.Error(), 200)
		s.saveTaskState(t.Key, false, tr.Note)
		return tr
	}
	tr.Status, tr.Note = "ok", note
	s.saveTaskState(t.Key, true, note)
	return tr
}

func (s *Service) decodeAF(out *callOut, v any) error {
	if out.Envelope == nil {
		return errors.New("empty response")
	}
	if err := json.Unmarshal(out.Envelope.Response, v); err != nil {
		return fmt.Errorf("unexpected response shape: %w", err)
	}
	return nil
}

// execute performs a task's calls and stores the results.
func (s *Service) execute(ctx context.Context, t Task, spec callSpec) (string, int, error) {
	cost := 0
	out, err := s.call(ctx, spec)
	if err != nil {
		return "", cost, err
	}
	cost += out.Cost
	switch t.Kind {
	case TLeagues:
		var ls []afLeagueEntry
		if err := s.decodeAF(out, &ls); err != nil {
			return "", cost, err
		}
		n, err := s.storeLeagues(ls)
		return fmt.Sprintf("%d competitions", n), cost, err
	case TSeason, TDate:
		var fs []afFixtureEntry
		if err := s.decodeAF(out, &fs); err != nil {
			return "", cost, err
		}
		if t.Kind == TDate {
			fs = s.onlyEnabled(fs)
		}
		r, err := s.storeFixtures(fs, false)
		return fmt.Sprintf("%d fixtures (%d finished)", r.Fixtures, r.Finished), cost, err
	case TLive, TResults, TPrematch, TDetails:
		var fs []afFixtureEntry
		if err := s.decodeAF(out, &fs); err != nil {
			return "", cost, err
		}
		r, err := s.storeFixtures(fs, true)
		if err == nil && t.Kind == TDetails {
			// Fixtures the provider has no details for are not asked again.
			got := map[int64]bool{}
			for _, f := range fs {
				got[f.Fixture.ID] = true
			}
			for _, id := range t.Fixtures {
				if !got[id] {
					s.DB.Model(&Fixture{}).Where("id = ?", id).Update("details_at", s.Now())
				}
			}
		}
		return fmt.Sprintf("%d fixtures, %d with details, %d line-ups, %d newly finished", r.Fixtures, r.Details, r.Lineups, len(r.NewlyFinal)), cost, err
	case TInjuries:
		var list []afInjury
		if err := s.decodeAF(out, &list); err != nil {
			return "", cost, err
		}
		want := map[int64]bool{}
		for _, id := range t.Fixtures {
			want[id] = true
		}
		keep := []afInjury{}
		for _, i := range list {
			if want[i.Fixture.ID] {
				keep = append(keep, i)
			}
		}
		n, err := s.storeInjuries(keep, t.Fixtures)
		return fmt.Sprintf("%d players out or doubtful for %d fixtures", n, len(t.Fixtures)), cost, err
	case TStandings:
		var es []afStandingsEntry
		if err := s.decodeAF(out, &es); err != nil {
			return "", cost, err
		}
		n, err := s.storeStandings(es)
		return fmt.Sprintf("%d table rows", n), cost, err
	case TOddsAF:
		total, fixtures := 0, map[int64]bool{}
		page := 1
		for {
			var es []afOddsEntry
			if err := s.decodeAF(out, &es); err != nil {
				return "", cost, err
			}
			rows := []Odd{}
			for _, e := range es {
				if s.fixtureKnown(e.Fixture.ID) {
					rows = append(rows, oddsRowsFromAF(e, s.Now())...)
					fixtures[e.Fixture.ID] = true
				}
			}
			n, _, err := s.storeOdds(rows)
			if err != nil {
				return "", cost, err
			}
			total += n
			if out.Envelope == nil || out.Envelope.Paging.Total <= page || page >= 10 {
				break
			}
			page++
			p := url.Values{}
			for k, v := range t.Params {
				p[k] = v
			}
			p.Set("page", strconv.Itoa(page))
			sp := spec
			sp.Params, sp.Task = p, fmt.Sprintf("%s:p%d", t.Key, page)
			out, err = s.call(ctx, sp)
			if err != nil {
				break // keep what we have; the rest next time
			}
			cost += out.Cost
		}
		ids := keys(fixtures)
		if len(ids) > 0 {
			s.DB.Model(&Fixture{}).Where("id IN ?", ids).Update("odds_af_at", s.Now())
		}
		return fmt.Sprintf("%d price changes for %d fixtures (%d pages)", total, len(ids), page), cost, nil
	case TOddsOA:
		var evs []OAEvent
		if err := json.Unmarshal(out.Body, &evs); err != nil {
			return "", cost, fmt.Errorf("unexpected response shape: %w", err)
		}
		var comps []int64
		s.DB.Model(&Competition{}).Where("enabled AND odds_sport_key = ?", t.SportKey).Pluck("id", &comps)
		m := s.matchOddsEvents(t.SportKey, comps, evs)
		rows := []Odd{}
		matched := []int64{}
		for _, e := range evs {
			fid, ok := m[e.ID]
			if !ok {
				continue
			}
			matched = append(matched, fid)
			rows = append(rows, oddsRowsFromOA(fid, e, s.Now())...)
		}
		n, _, err := s.storeOdds(rows)
		if len(matched) > 0 {
			s.DB.Model(&Fixture{}).Where("id IN ?", matched).Update("odds_oa_at", s.Now())
		}
		rem := ""
		if out.Remaining != nil {
			rem = fmt.Sprintf(", %d credits left", *out.Remaining)
		}
		return fmt.Sprintf("%d events, %d matched, %d price changes%s", len(evs), len(matched), n, rem), cost, err
	case TH2H:
		var fs []afFixtureEntry
		if err := s.decodeAF(out, &fs); err != nil {
			return "", cost, err
		}
		var f Fixture
		if len(t.Fixtures) == 0 || s.DB.Where("id = ?", t.Fixtures[0]).Limit(1).Find(&f).RowsAffected == 0 {
			return "", cost, errors.New("fixture gone")
		}
		if err := s.storeH2H(f.HomeID, f.AwayID, fs); err != nil {
			return "", cost, err
		}
		s.DB.Model(&Fixture{}).Where("(home_id = ? AND away_id = ?) OR (home_id = ? AND away_id = ?)", f.HomeID, f.AwayID, f.AwayID, f.HomeID).Update("h2h_at", s.Now())
		return fmt.Sprintf("%d meetings", len(fs)), cost, nil
	case TPlayers, TScorers:
		total, page := 0, 1
		for {
			var ps []afPlayerEntry
			if err := s.decodeAF(out, &ps); err != nil {
				return "", cost, err
			}
			n, err := s.storePlayers(ps)
			if err != nil {
				return "", cost, err
			}
			total += n
			if !t.Pages || out.Envelope.Paging.Total <= page || page >= 6 {
				break
			}
			page++
			p := url.Values{}
			for k, v := range t.Params {
				p[k] = v
			}
			p.Set("page", strconv.Itoa(page))
			sp := spec
			sp.Params, sp.Task = p, fmt.Sprintf("%s:p%d", t.Key, page)
			out, err = s.call(ctx, sp)
			if err != nil {
				break
			}
			cost += out.Cost
		}
		return fmt.Sprintf("%d players", total), cost, nil
	case TPredRef:
		var ps []afPrediction
		if err := s.decodeAF(out, &ps); err != nil {
			return "", cost, err
		}
		return "stored as reference", cost, s.storePredictionRef(t.Fixtures[0], ps)
	case TTeams:
		var ts []afTeamEntry
		if err := s.decodeAF(out, &ts); err != nil {
			return "", cost, err
		}
		return fmt.Sprintf("%d teams", len(ts)), cost, s.storeTeams(ts)
	case TBooks:
		var bs []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		if err := s.decodeAF(out, &bs); err != nil {
			return "", cost, err
		}
		names := []string{}
		for _, b := range bs {
			if n := strings.TrimSpace(b.Name); n != "" {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		err := s.DB.Model(&Settings{}).Where("id = 1").Update("af_bookmakers", mustJSON(names)).Error
		s.invalidateSettings()
		k := KenyanIn(names)
		if len(k) == 0 {
			k = []string{"none"}
		}
		return fmt.Sprintf("%d bookmakers; brands operating in Kenya: %s", len(names), strings.Join(k, ", ")), cost, err
	case TOddsLive:
		var es []afLiveOdds
		if err := s.decodeAF(out, &es); err != nil {
			return "", cost, err
		}
		n, err := s.storeLiveOdds(es)
		return fmt.Sprintf("%d in-play price changes", n), cost, err
	case TSports:
		var sp []OASport
		_ = json.Unmarshal(out.Body, &sp)
		n := 0
		for _, x := range sp {
			if strings.HasPrefix(x.Key, "soccer_") {
				n++
			}
		}
		return fmt.Sprintf("%d soccer competitions in season", n), cost, nil
	}
	return "", cost, fmt.Errorf("unknown task %s", t.Kind)
}

func keys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (s *Service) fixtureKnown(id int64) bool {
	var n int64
	s.DB.Model(&Fixture{}).Where("id = ?", id).Count(&n)
	return n > 0
}

// onlyEnabled keeps the fixtures of enabled competitions (a date fetch
// returns every competition in the world).
func (s *Service) onlyEnabled(fs []afFixtureEntry) []afFixtureEntry {
	var ids []int64
	s.DB.Model(&Competition{}).Where("enabled").Pluck("id", &ids)
	en := map[int64]bool{}
	for _, id := range ids {
		en[id] = true
	}
	out := []afFixtureEntry{}
	for _, f := range fs {
		if en[f.League.ID] {
			out = append(out, f)
		}
	}
	return out
}

// RefreshFixture fetches one fixture now with everything embedded (manual
// "refresh this match" for super admins; costs one request).
func (s *Service) RefreshFixture(ctx context.Context, id int64) (StoreResult, error) {
	out, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Params: url.Values{"id": {strconv.FormatInt(id, 10)}},
		Cost: 1, Prio: PrioHigh, Task: fmt.Sprintf("manual:fixture:%d", id)})
	if err != nil {
		return StoreResult{}, err
	}
	var fs []afFixtureEntry
	if err := s.decodeAF(out, &fs); err != nil {
		return StoreResult{}, err
	}
	return s.storeFixtures(fs, true)
}

// FetchFixtureParts calls the per-fixture endpoints (statistics, events,
// lineups, players): four requests, used when the embedded data is missing.
func (s *Service) FetchFixtureParts(ctx context.Context, id int64) error {
	p := url.Values{"fixture": {strconv.FormatInt(id, 10)}}
	get := func(ep string, v any) error {
		out, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: ep, Params: p, Cost: 1, Prio: PrioNormal, Task: fmt.Sprintf("manual:%s:%d", ep, id)})
		if err != nil {
			return err
		}
		return s.decodeAF(out, v)
	}
	var st []afTeamStats
	var ev []afEvent
	var ln []afLineup
	var pl []afFixturePlayerTeam
	if err := get("fixtures/statistics", &st); err != nil {
		return err
	}
	if err := get("fixtures/events", &ev); err != nil {
		return err
	}
	if err := get("fixtures/lineups", &ln); err != nil {
		return err
	}
	if err := get("fixtures/players", &pl); err != nil {
		return err
	}
	return s.storeFixtureDetailParts(id, st, ev, ln, pl)
}

// FetchLiveOdds stores API-Football's in-play prices (GET /odds/live, one
// request for every fixture in play).
func (s *Service) FetchLiveOdds(ctx context.Context) (int, error) {
	out, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "odds/live", Cost: 1, Prio: PrioLow, Task: "af:oddslive"})
	if err != nil {
		return 0, err
	}
	var es []afLiveOdds
	if err := s.decodeAF(out, &es); err != nil {
		return 0, err
	}
	return s.storeLiveOdds(es)
}

func (s *Service) storeLiveOdds(es []afLiveOdds) (int, error) {
	rows := []Odd{}
	now := s.Now()
	for _, e := range es {
		if !s.fixtureKnown(e.Fixture.ID) || e.Status.Finished {
			continue
		}
		upd, _ := parseTime(e.Update)
		for _, b := range e.Odds {
			for _, v := range b.Values {
				price := 0.0
				if f := statNumber(v.Odd); f != nil {
					price = *f
				}
				if price <= 1 || v.Suspended || (v.Main != nil && !*v.Main && strings.EqualFold(b.Name, "Asian Handicap")) {
					continue
				}
				r := Odd{FixtureID: e.Fixture.ID, Source: ProviderAPIFootball, Bookmaker: "apifootball_live", BookmakerTitle: "API-Football in-play",
					Price: price, Live: true, ProviderUpdatedAt: &upd, FetchedAt: now, CheckedAt: now}
				name := strings.ToLower(b.Name)
				if name == "fulltime result" {
					name = "match winner"
				}
				if afSelection(name, asString(v.Value), v.Handicap, &r) {
					rows = append(rows, r)
				}
			}
		}
	}
	n, _, err := s.storeOdds(rows)
	return n, err
}
