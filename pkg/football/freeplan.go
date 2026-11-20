package football

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What to fetch from the free sources, when (pure planner, like Plan) — all
// within each source's budget (freequota.go):
//   - ClubElo: the day's ratings and /Fixtures once a day (2 requests).
//   - football-data.co.uk: current-season files weekly, past seasons once.
//   - football-data.org: standings daily, matches (−14…+7 days) twice a day,
//     per covered enabled competition.
//   - OddsPapi: fixture discovery (one request for the next days) when
//     targets aren't mapped yet (at most every 6 h), then per bookmaker one
//     odds-by-tournaments request per 10 tournaments for targets due a snapshot.
//   - News: per team about to play (jackpot / watched first, then the next
//     48 h of enabled competitions), Google News and GDELT at most twice a
//     day each; feeds every 6 h.
//   - API-Football helpers for jackpots: the day's fixture list when a
//     jackpot has unmatched games (to find lower-league fixtures), and
//     results of jackpot fixtures outside the enabled competitions.

// Free task kinds.
const (
	TCERatings   = "clubelo_ratings"
	TCEFixtures  = "clubelo_fixtures"
	TFDFile      = "fdcouk"
	TFDOStand    = "fdorg_standings"
	TFDOMatches  = "fdorg_matches"
	TOPFixtures  = "oddspapi_fixtures"
	TOPOdds      = "oddspapi_odds"
	TNewsGoogle  = "news_google"
	TNewsGDELT   = "news_gdelt"
	TNewsFeed    = "news_feed"
	TJPDate      = "jackpot_date"
	TJPResults   = "jackpot_results"
	freeKindsAll = "clubelo fdcouk fdorg oddspapi news jackpot"
)

// FreePlanInput is the planner's view of the free sources.
type FreePlanInput struct {
	Now      time.Time
	Settings FreeSettings
	On       map[string]bool
	HasAF    bool
	State    map[string]TaskState
	Force    bool
	Only     map[string]bool
	FD       []fdTarget
	FDOrg    []struct {
		Code string
		Comp int64
	}
	OP        []opTarget
	NewsTeams []newsTarget
	Feeds     []NewsFeed
	JPDates   []string // YYYY-MM-DD with unmatched jackpot games
	JPResults []int64  // jackpot fixtures waiting for a result
}

type newsTarget struct {
	newsTeam
	Kickoff time.Time
	Prio    int
}

func (in FreePlanInput) due(key string, every time.Duration) bool {
	return PlanInput{Now: in.Now, State: in.State, Force: in.Force}.due(key, every)
}

// wantFree: manual runs may ask for some providers ("clubelo", "news" …).
func (in FreePlanInput) wantFree(group string) bool {
	if len(in.Only) == 0 {
		return true
	}
	return in.Only[group]
}

// PlanFree lists the due free-source tasks, highest priority first.
func PlanFree(in FreePlanInput) []Task {
	now := in.Now
	out := []Task{}
	add := func(t Task) { out = append(out, t) }
	if in.On[ProviderClubElo] && in.wantFree("clubelo") {
		day := now.Format("2006-01-02")
		if in.due("ce:ratings", 20*time.Hour) {
			add(Task{Key: "ce:ratings", Kind: TCERatings, Provider: ProviderClubElo, Prio: PrioNormal, Cost: 1, Endpoint: "/" + day, Reason: "club Elo ratings of the day"})
		}
		if in.due("ce:fixtures", 12*time.Hour) {
			// After the ratings (the fixtures are linked through the mapped clubs).
			add(Task{Key: "ce:fixtures", Kind: TCEFixtures, Provider: ProviderClubElo, Prio: PrioNormal, Cost: 1, Endpoint: "/Fixtures", Urgency: 1, Reason: "ClubElo fixture probabilities"})
		}
	}
	if in.On[ProviderFDCOUK] && in.wantFree("fdcouk") {
		for _, t := range in.FD {
			every := 7 * 24 * time.Hour
			prio := PrioLow
			if !t.Weekly {
				every, prio = 180*24*time.Hour, PrioBackfill
			}
			if _, ok := in.State[t.Key]; !ok {
				prio = PrioLow
			}
			if in.due(t.Key, every) {
				p := url.Values{"div": {t.Div}, "season": {strconv.Itoa(t.Season)}, "comp": {strconv.FormatInt(t.Comp, 10)}, "min_season": {strconv.Itoa(t.MinSeason)}}
				add(Task{Key: t.Key, Kind: TFDFile, Provider: ProviderFDCOUK, Prio: prio, Cost: 1, Endpoint: t.Path, Params: p, Comp: t.Comp, Season: t.Season,
					Reason: "results history and closing odds (" + t.Div + ")"})
			}
		}
	}
	if in.On[ProviderFDOrg] && in.wantFree("fdorg") {
		for _, c := range in.FDOrg {
			k := "fo:standings:" + c.Code
			if in.due(k, 24*time.Hour) {
				add(Task{Key: k, Kind: TFDOStand, Provider: ProviderFDOrg, Prio: PrioLow, Cost: 1, Endpoint: "/v4/competitions/" + c.Code + "/standings", Comp: c.Comp,
					Params: url.Values{"code": {c.Code}}, Reason: "table cross-check"})
			}
			k = "fo:matches:" + c.Code
			if in.due(k, 12*time.Hour) {
				add(Task{Key: k, Kind: TFDOMatches, Provider: ProviderFDOrg, Prio: PrioLow, Cost: 1, Endpoint: "/v4/competitions/" + c.Code + "/matches", Comp: c.Comp,
					Params: url.Values{"code": {c.Code}}, Reason: "fixtures and results cross-check"})
			}
		}
	}
	if in.On[ProviderOddsPapi] && in.wantFree("oddspapi") && len(in.OP) > 0 {
		unmapped, maxKO, minPrio := 0, now, PrioNormal
		for _, t := range in.OP {
			if t.OPID == "" && t.Kickoff.Sub(now) <= 48*time.Hour {
				unmapped++
				if t.Kickoff.After(maxKO) {
					maxKO = t.Kickoff
				}
				if t.Prio < minPrio {
					minPrio = t.Prio
				}
			}
		}
		if unmapped > 0 && in.due("op:fixtures", 6*time.Hour) {
			to := maxKO.Add(2 * time.Hour)
			if to.Sub(now) < 24*time.Hour {
				to = now.Add(24 * time.Hour)
			}
			prio := PrioNormal
			if minPrio <= 1 {
				prio = PrioHigh
			}
			add(Task{Key: "op:fixtures", Kind: TOPFixtures, Provider: ProviderOddsPapi, Prio: prio, Cost: 1, Endpoint: "/v4/fixtures",
				Params: url.Values{"from": {now.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}, Reason: fmt.Sprintf("find %d fixture(s) at OddsPapi", unmapped)})
		}
		// Snapshots: tournaments of mapped targets due a snapshot.
		tours := map[int64]int{} // tournament → best prio
		fixes := []int64{}
		for _, t := range in.OP {
			if t.OPID == "" || t.Tournament == 0 || !opNeedsOdds(t, now) {
				continue
			}
			if p, ok := tours[t.Tournament]; !ok || t.Prio < p {
				tours[t.Tournament] = t.Prio
			}
			fixes = append(fixes, t.FixtureID)
		}
		if len(tours) > 0 && in.due("op:odds", 30*time.Minute) {
			ids := make([]int64, 0, len(tours))
			for id := range tours {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool {
				if tours[ids[i]] != tours[ids[j]] {
					return tours[ids[i]] < tours[ids[j]]
				}
				return ids[i] < ids[j]
			})
			for _, bk := range splitList(in.Settings.OddsPapiBookmakers, []string{"betika"}) {
				for ci, ch := range chunk(ids, 10) {
					prio := PrioNormal
					for _, id := range ch {
						if tours[id] <= 1 {
							prio = PrioHigh
						}
					}
					parts := []string{}
					for _, id := range ch {
						parts = append(parts, strconv.FormatInt(id, 10))
					}
					add(Task{Key: fmt.Sprintf("op:odds:%s:%d", bk, ci), Kind: TOPOdds, Provider: ProviderOddsPapi, Prio: prio, Cost: 1, Endpoint: "/v4/odds-by-tournaments",
						Params: url.Values{"bookmaker": {bk}, "tournaments": {strings.Join(parts, ",")}}, Fixtures: fixes, Reason: fmt.Sprintf("%s prices for %d tournament(s)", opTitle(bk), len(ch))})
				}
			}
		}
	}
	if in.wantFree("news") {
		for i, t := range in.NewsTeams {
			urg := float64(i)
			if in.On[ProviderGNews] {
				k := fmt.Sprintf("gn:%d", t.ID)
				if in.due(k, 12*time.Hour) {
					add(Task{Key: k, Kind: TNewsGoogle, Provider: ProviderGNews, Prio: t.Prio, Cost: 1, Endpoint: "/rss/search", Team: t.ID, Urgency: urg,
						Params: url.Values{"name": {t.Name}, "country": {t.Country}}, Reason: "team news: " + t.Name})
				}
				if lang, _, _ := newsLangOf(t.Country); lang != "en" {
					k := fmt.Sprintf("gn:%d:en", t.ID)
					if in.due(k, 24*time.Hour) {
						add(Task{Key: k, Kind: TNewsGoogle, Provider: ProviderGNews, Prio: t.Prio + 1, Cost: 1, Endpoint: "/rss/search", Team: t.ID, Urgency: urg,
							Params: url.Values{"name": {t.Name}, "country": {t.Country}, "english": {"1"}}, Reason: "team news (English): " + t.Name})
					}
				}
			}
			if in.On[ProviderGDELT] {
				k := fmt.Sprintf("gd:%d", t.ID)
				if in.due(k, 12*time.Hour) {
					add(Task{Key: k, Kind: TNewsGDELT, Provider: ProviderGDELT, Prio: t.Prio, Cost: 1, Endpoint: "/api/v2/doc/doc", Team: t.ID, Urgency: urg,
						Params: url.Values{"name": {t.Name}, "country": {t.Country}}, Reason: "team news (GDELT): " + t.Name})
				}
			}
		}
		if in.On[ProviderFeeds] {
			for _, fd := range in.Feeds {
				if !fd.Enabled {
					continue
				}
				k := "feed:" + fd.ID
				if in.due(k, 6*time.Hour) {
					add(Task{Key: k, Kind: TNewsFeed, Provider: ProviderFeeds, Prio: PrioNormal, Cost: 1, Endpoint: fd.URL, Params: url.Values{"id": {fd.ID}}, Reason: "feed: " + fd.Label})
				}
			}
		}
	}
	if in.HasAF && in.wantFree("jackpot") {
		for _, d := range in.JPDates {
			k := "af:jpdate:" + d
			if in.due(k, 6*time.Hour) {
				add(Task{Key: k, Kind: TJPDate, Provider: ProviderAPIFootball, Prio: PrioHigh, Cost: 1, Endpoint: "fixtures",
					Params: url.Values{"date": {d}, "timezone": {"UTC"}}, Reason: "find jackpot games' fixtures on " + d})
			}
		}
		for i, ids := range chunk(in.JPResults, 20) {
			k := fmt.Sprintf("af:jpresults:%d", i)
			if in.due(k, 30*time.Minute) {
				add(Task{Key: k, Kind: TJPResults, Provider: ProviderAPIFootball, Prio: PrioCritical, Cost: 1, Endpoint: "fixtures",
					Params: url.Values{"ids": {idsParam(ids)}}, Fixtures: ids, Reason: "results of jackpot games"})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Prio != out[j].Prio {
			return out[i].Prio < out[j].Prio
		}
		if out[i].Urgency != out[j].Urgency {
			return out[i].Urgency < out[j].Urgency
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// freePlanInput loads the planner's view.
func (s *Service) freePlanInput(o RunOpts) FreePlanInput {
	now := s.Now()
	st := s.FreeSettingsGet()
	af, _ := s.Keys()
	in := FreePlanInput{Now: now, Settings: st, On: map[string]bool{}, HasAF: af != "", State: map[string]TaskState{}, Force: o.Force, Only: freeOnly(o.Only)}
	for _, p := range FreeProviders {
		in.On[p] = s.FreeOn(p)
	}
	var states []TaskState
	s.DB.Where("key LIKE 'ce:%' OR key LIKE 'fd:%' OR key LIKE 'fo:%' OR key LIKE 'op:%' OR key LIKE 'gn:%' OR key LIKE 'gd:%' OR key LIKE 'feed:%' OR key LIKE 'af:jp%'").Find(&states)
	for _, t := range states {
		in.State[t.Key] = t
	}
	if in.On[ProviderFDCOUK] {
		in.FD = s.fdTargets(now)
	}
	if in.On[ProviderFDOrg] {
		for _, c := range s.Competitions(true) {
			if code := fdOrgCodeOf(c.ID); code != "" {
				in.FDOrg = append(in.FDOrg, struct {
					Code string
					Comp int64
				}{code, c.ID})
			}
		}
	}
	if in.On[ProviderOddsPapi] {
		in.OP = s.opTargets(now)
	}
	if in.On[ProviderGNews] || in.On[ProviderGDELT] {
		in.NewsTeams = s.newsTargets(now)
	}
	if in.On[ProviderFeeds] {
		s.DB.Where("enabled").Order("created_at").Find(&in.Feeds)
	}
	if in.HasAF {
		s.DB.Raw(`SELECT DISTINCT to_char(COALESCE(g.kickoff, j.closes_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD') FROM fb_jackpot_games g JOIN fb_jackpots j ON j.id = g.jackpot_id
			WHERE g.fixture_id IS NULL AND j.status <> 'graded' AND COALESCE(g.kickoff, j.closes_at) BETWEEN ? AND ?`, now.Add(-6*time.Hour), now.Add(8*24*time.Hour)).Scan(&in.JPDates)
		s.DB.Raw(`SELECT DISTINCT f.id FROM fb_jackpot_games g JOIN fb_fixtures f ON f.id = g.fixture_id LEFT JOIN fb_competitions c ON c.id = f.competition_id
			WHERE (c.id IS NULL OR NOT c.enabled) AND f.status NOT IN ('FT','AET','PEN','CANC','ABD','AWD','WO') AND f.id > 0
			AND f.kickoff BETWEEN ? AND ?`, now.Add(-72*time.Hour), now.Add(-110*time.Minute)).Scan(&in.JPResults)
	}
	return in
}

func freeOnly(only map[string]bool) map[string]bool {
	if len(only) == 0 {
		return nil
	}
	out := map[string]bool{}
	for k := range only {
		for _, g := range strings.Fields(freeKindsAll) {
			if k == g || strings.HasPrefix(k, g) {
				out[g] = true
			}
		}
	}
	if len(out) == 0 {
		return map[string]bool{"__none__": true} // only paid kinds asked for
	}
	return out
}

// newsTargets: teams of jackpot games and watched fixtures/teams first, then
// teams playing in the next 48 h in enabled competitions (soonest first).
func (s *Service) newsTargets(now time.Time) []newsTarget {
	var rows []struct {
		ID      int64
		Name    string
		Country string
		Kickoff time.Time
		Prio    int
	}
	s.DB.Raw(`SELECT t.id, t.name, COALESCE(NULLIF(t.country,''), c.country, '') AS country, MIN(x.kickoff) AS kickoff, MIN(x.prio) AS prio FROM (
			SELECT f.home_id AS team, f.kickoff, f.competition_id, 1 AS prio FROM fb_jackpot_games g JOIN fb_jackpots j ON j.id = g.jackpot_id JOIN fb_fixtures f ON f.id = g.fixture_id
				WHERE f.kickoff BETWEEN ? AND ? AND j.status <> 'graded'
			UNION ALL SELECT f.away_id, f.kickoff, f.competition_id, 1 FROM fb_jackpot_games g JOIN fb_jackpots j ON j.id = g.jackpot_id JOIN fb_fixtures f ON f.id = g.fixture_id
				WHERE f.kickoff BETWEEN ? AND ? AND j.status <> 'graded'
			UNION ALL SELECT f.home_id, f.kickoff, f.competition_id, 1 FROM fb_watchlist w JOIN fb_fixtures f ON w.kind = 'fixture' AND f.id = w.ref_id WHERE f.kickoff BETWEEN ? AND ?
			UNION ALL SELECT f.away_id, f.kickoff, f.competition_id, 1 FROM fb_watchlist w JOIN fb_fixtures f ON w.kind = 'fixture' AND f.id = w.ref_id WHERE f.kickoff BETWEEN ? AND ?
			UNION ALL SELECT f.home_id, f.kickoff, f.competition_id, 2 FROM fb_fixtures f JOIN fb_competitions c ON c.id = f.competition_id AND c.enabled WHERE f.kickoff BETWEEN ? AND ?
			UNION ALL SELECT f.away_id, f.kickoff, f.competition_id, 2 FROM fb_fixtures f JOIN fb_competitions c ON c.id = f.competition_id AND c.enabled WHERE f.kickoff BETWEEN ? AND ?
		) x JOIN fb_teams t ON t.id = x.team LEFT JOIN fb_competitions c ON c.id = x.competition_id
		GROUP BY t.id, t.name, COALESCE(NULLIF(t.country,''), c.country, '') ORDER BY MIN(x.prio), MIN(x.kickoff) LIMIT 120`,
		now, now.Add(4*24*time.Hour), now, now.Add(4*24*time.Hour), now, now.Add(3*24*time.Hour), now, now.Add(3*24*time.Hour), now, now.Add(48*time.Hour), now, now.Add(48*time.Hour)).Scan(&rows)
	out := []newsTarget{}
	seen := map[int64]bool{}
	for _, r := range rows {
		if seen[r.ID] || r.ID <= 0 {
			continue
		}
		seen[r.ID] = true
		prio := PrioNormal
		if r.Prio <= 1 {
			prio = PrioHigh
		}
		out = append(out, newsTarget{newsTeam: newsTeam{ID: r.ID, Name: r.Name, Country: r.Country}, Kickoff: r.Kickoff, Prio: prio})
	}
	return out
}

// perPass caps the requests per source in one scheduler pass.
var perPass = map[string]int{ProviderFDCOUK: 6, ProviderFDOrg: 4, ProviderGDELT: 6, ProviderGNews: 10, ProviderFeeds: 6, ProviderClubElo: 2, ProviderOddsPapi: 4}

// runFree plans and runs the free-source tasks (called from the scheduler
// pass under its lock), then dedupes seeded fixtures and grades jackpots.
func (s *Service) runFree(ctx context.Context, o RunOpts, sum *RunSummary) {
	in := s.freePlanInput(o)
	tasks := PlanFree(in)
	blocked := map[string]int{}
	ran := map[string]int{}
	pricedOP := map[string]bool{}
	discovered := false
	for _, t := range tasks {
		if ctx.Err() != nil {
			break
		}
		sum.Planned++
		// A pass does a few requests per source (they are spaced out to be
		// polite); the rest stay due and run in the next passes.
		if c, ok := perPass[t.Provider]; ok && ran[t.Provider] >= c && t.Prio > PrioCritical {
			sum.Skipped++
			continue
		}
		ran[t.Provider]++
		if p, ok := blocked[t.Provider]; ok && t.Prio >= p {
			sum.Skipped++
			if len(sum.Tasks) < 120 {
				sum.Tasks = append(sum.Tasks, TaskRun{Key: t.Key, Kind: t.Kind, Provider: t.Provider, Prio: t.Prio, Status: "skipped", Note: "budget"})
			}
			continue
		}
		tr := s.runFreeTask(ctx, t)
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
		if len(sum.Tasks) < 120 {
			sum.Tasks = append(sum.Tasks, tr)
		}
		if t.Kind == TOPOdds && tr.Status == "ok" {
			pricedOP[t.Params.Get("bookmaker")+"|"+t.Params.Get("tournaments")] = true
		}
		if t.Kind == TOPFixtures && tr.Status == "ok" {
			discovered = true
		}
	}
	// Fixtures OddsPapi just mapped are priced in the same pass.
	if discovered && ctx.Err() == nil {
		in2 := s.freePlanInput(RunOpts{Trigger: o.Trigger, Force: o.Force, Only: map[string]bool{"oddspapi": true}})
		for _, t := range PlanFree(in2) {
			if t.Kind != TOPOdds || pricedOP[t.Params.Get("bookmaker")+"|"+t.Params.Get("tournaments")] {
				continue
			}
			if p, ok := blocked[t.Provider]; ok && t.Prio >= p {
				continue
			}
			sum.Planned++
			tr := s.runFreeTask(ctx, t)
			switch tr.Status {
			case "ok":
				sum.Done++
			case "skipped":
				sum.Skipped++
			default:
				sum.Failed++
			}
			sum.Cost[t.Provider] += tr.Cost
			if len(sum.Tasks) < 120 {
				sum.Tasks = append(sum.Tasks, tr)
			}
		}
	}
	if n := s.dedupeSeeded(); n > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%d seeded football-data.co.uk fixture(s) merged into API-Football's", n))
	}
	if n := s.GradeJackpots(); n > 0 {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%d jackpot(s) graded", n))
	}
	// Forget old news (keep 60 days) and long-expired facts (180 days).
	now := s.Now()
	s.DB.Exec(`DELETE FROM fb_news_teams WHERE news_id IN (SELECT id FROM fb_news WHERE published_at < ?)`, now.AddDate(0, 0, -60))
	s.DB.Where("published_at < ?", now.AddDate(0, 0, -60)).Delete(&NewsItem{})
	s.DB.Where("expires_at < ? AND admin_action = ''", now.AddDate(0, 0, -180)).Delete(&TeamNewsFact{})
	s.DB.Where("last_run_at < ? AND (key LIKE 'gn:%' OR key LIKE 'gd:%' OR key LIKE 'af:jp%')", now.AddDate(0, 0, -30)).Delete(&TaskState{})
}

func (s *Service) runFreeTask(ctx context.Context, t Task) TaskRun {
	tr := TaskRun{Key: t.Key, Kind: t.Kind, Provider: t.Provider, Prio: t.Prio}
	before := s.quotaRow(t.Provider, dayPeriod(s.Now())).Used
	note, err := s.executeFree(ctx, t)
	tr.Cost = s.quotaRow(t.Provider, dayPeriod(s.Now())).Used - before
	if err != nil {
		var pe *ProviderError
		if errors.As(err, &pe) && (pe.Kind == "budget" || pe.Kind == "backoff" || pe.Kind == "no_key") {
			tr.Status, tr.Note = "skipped", pe.Kind+": "+pe.Msg
			return tr
		}
		if errors.As(err, &pe) && pe.Kind == "not_found" && t.Kind == TFDFile {
			tr.Status, tr.Note = "ok", "no file for this season"
			s.saveTaskState(t.Key, true, tr.Note)
			return tr
		}
		if !errors.As(err, &pe) {
			// The answer came but couldn't be read: the log row says so.
			s.markLogError(t.Provider, t.Key, "unreadable: "+err.Error())
		}
		tr.Status, tr.Note = "failed", clip(err.Error(), 200)
		s.saveTaskState(t.Key, false, tr.Note)
		return tr
	}
	tr.Status, tr.Note = "ok", note
	s.saveTaskState(t.Key, true, note)
	return tr
}

func (s *Service) executeFree(ctx context.Context, t Task) (string, error) {
	now := s.Now()
	switch t.Kind {
	case TCERatings:
		n, err := s.fetchClubEloRatings(ctx, now, t.Prio, t.Key)
		return fmt.Sprintf("%d clubs", n), err
	case TCEFixtures:
		n, err := s.fetchClubEloFixtures(ctx, t.Prio, t.Key)
		return fmt.Sprintf("%d fixtures", n), err
	case TFDFile:
		season, _ := strconv.Atoi(t.Params.Get("season"))
		comp, _ := strconv.ParseInt(t.Params.Get("comp"), 10, 64)
		minS, _ := strconv.Atoi(t.Params.Get("min_season"))
		r, err := s.fetchFDFile(ctx, t.Endpoint, t.Params.Get("div"), season, comp, t.Prio, t.Key, minS)
		if err == nil {
			s.markSeasonSeeded(fdTarget{Comp: comp, Season: season, Weekly: strings.Contains(t.Key, ":new:") || season == 0})
			if r.Inserted > 0 {
				ResetModelCache()
			}
		}
		return fmt.Sprintf("%d matches (%d merged, %d seeded, %d prices)", r.Matches, r.Merged, r.Inserted, r.Odds), err
	case TFDOStand:
		n, err := s.fetchFDOrgStandings(ctx, t.Params.Get("code"), t.Comp, t.Prio, t.Key)
		return fmt.Sprintf("%d rows", n), err
	case TFDOMatches:
		n, err := s.fetchFDOrgMatches(ctx, t.Params.Get("code"), t.Comp, now.AddDate(0, 0, -14), now.AddDate(0, 0, 7), t.Prio, t.Key)
		return fmt.Sprintf("%d matches", n), err
	case TOPFixtures:
		from, _ := time.Parse(time.RFC3339, t.Params.Get("from"))
		to, _ := time.Parse(time.RFC3339, t.Params.Get("to"))
		n, m, err := s.fetchOPFixtures(ctx, from, to, t.Prio, t.Key)
		return fmt.Sprintf("%d fixtures, %d matched", n, m), err
	case TOPOdds:
		ids := []int64{}
		for _, p := range strings.Split(t.Params.Get("tournaments"), ",") {
			if id, err := strconv.ParseInt(p, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		n, err := s.fetchOPOdds(ctx, t.Params.Get("bookmaker"), ids, t.Prio, t.Key)
		return fmt.Sprintf("%d new prices", n), err
	case TNewsGoogle:
		nt := newsTeam{ID: t.Team, Name: t.Params.Get("name"), Country: t.Params.Get("country")}
		a, f, err := s.fetchGoogleNews(ctx, nt, t.Params.Get("english") == "1", t.Prio, t.Key)
		return fmt.Sprintf("%d new items, %d facts", a, f), err
	case TNewsGDELT:
		nt := newsTeam{ID: t.Team, Name: t.Params.Get("name"), Country: t.Params.Get("country")}
		a, f, err := s.fetchGDELT(ctx, nt, t.Prio, t.Key)
		return fmt.Sprintf("%d new items, %d facts", a, f), err
	case TNewsFeed:
		var fd NewsFeed
		if s.DB.Where("id = ?", t.Params.Get("id")).Limit(1).Find(&fd).RowsAffected == 0 {
			return "feed removed", nil
		}
		a, f, err := s.fetchFeed(ctx, fd, t.Prio, t.Key)
		return fmt.Sprintf("%d new items, %d facts", a, f), err
	case TJPDate:
		out, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Params: t.Params, Cost: 1, Prio: t.Prio, Task: t.Key})
		if err != nil {
			return "", err
		}
		var fs []afFixtureEntry
		if err := s.decodeAF(out, &fs); err != nil {
			return "", err
		}
		n, err := s.storeJackpotDateFixtures(fs)
		return fmt.Sprintf("%d jackpot game(s) matched from %d fixtures", n, len(fs)), err
	case TJPResults:
		out, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "fixtures", Params: t.Params, Cost: 1, Prio: t.Prio, Task: t.Key})
		if err != nil {
			return "", err
		}
		var fs []afFixtureEntry
		if err := s.decodeAF(out, &fs); err != nil {
			return "", err
		}
		r, err := s.storeFixtures(fs, false)
		return fmt.Sprintf("%d fixtures, %d finished", r.Fixtures, r.Finished), err
	}
	return "", fmt.Errorf("unknown task %s", t.Kind)
}

// storeJackpotDateFixtures keeps only the fixtures of a day's list that
// match unmatched jackpot games (a day's list has every league in the world)
// and rematches those games.
func (s *Service) storeJackpotDateFixtures(fs []afFixtureEntry) (int, error) {
	var games []JackpotGame
	s.DB.Raw(`SELECT g.* FROM fb_jackpot_games g JOIN fb_jackpots j ON j.id = g.jackpot_id WHERE g.fixture_id IS NULL AND j.status <> 'graded'`).Scan(&games)
	if len(games) == 0 || len(fs) == 0 {
		return 0, nil
	}
	cands := make([]fixCand, 0, len(fs))
	byID := map[int64]afFixtureEntry{}
	for _, e := range fs {
		t, ok := parseTime(e.Fixture.Date)
		if !ok {
			continue
		}
		cands = append(cands, fixCand{ID: e.Fixture.ID, Kickoff: t, CompID: e.League.ID, HomeID: e.Teams.Home.ID, AwayID: e.Teams.Away.ID,
			Home: e.Teams.Home.Name, Away: e.Teams.Away.Name, CompName: e.League.Name})
		byID[e.Fixture.ID] = e
	}
	aliases := s.aliasMap()
	keep := map[int64]bool{}
	for _, g := range games {
		m := MatchGame(g.Home, g.Away, g.Kickoff, cands, aliases)
		if m.Status == "matched" && m.FixtureID != nil {
			keep[*m.FixtureID] = true
		}
	}
	sel := []afFixtureEntry{}
	for id := range keep {
		sel = append(sel, byID[id])
	}
	if len(sel) == 0 {
		return 0, nil
	}
	if _, err := s.storeFixtures(sel, false); err != nil {
		return 0, err
	}
	// Rematch the jackpots' unmatched games now that the fixtures exist.
	byJP := map[string]bool{}
	for _, g := range games {
		byJP[g.JackpotID] = true
	}
	n := 0
	for id := range byJP {
		var j Jackpot
		if s.DB.Where("id = ?", id).Limit(1).Find(&j).RowsAffected == 0 {
			continue
		}
		var gs []JackpotGame
		s.DB.Where("jackpot_id = ? AND fixture_id IS NULL", id).Order("n").Find(&gs)
		s.matchGames(gs, j.ClosesAt)
		for i := range gs {
			if gs[i].FixtureID != nil {
				n++
			}
			s.DB.Save(&gs[i])
		}
	}
	return n, nil
}

// RunFreeNow runs the free sources now (super admin).
func (s *Service) RunFreeNow(ctx context.Context, providers []string, force bool) (RunSummary, error) {
	if !s.runMu.TryLock() {
		return RunSummary{}, ErrBusy
	}
	defer s.runMu.Unlock()
	sum := RunSummary{At: s.Now(), Trigger: "manual", Cost: map[string]int{}, Tasks: []TaskRun{}, Notes: []string{}}
	only := map[string]bool{}
	for _, p := range providers {
		switch p {
		case ProviderGNews, ProviderGDELT, ProviderFeeds, "news":
			only["news"] = true
		case ProviderOddsPapi, ProviderClubElo, ProviderFDCOUK, ProviderFDOrg, "jackpot":
			only[p] = true
		}
	}
	started := time.Now()
	ok := s.withLock(ctx, "football:scheduler", func() { s.runFree(ctx, RunOpts{Trigger: "manual", Force: force, Only: only}, &sum) })
	if !ok {
		return sum, ErrBusy
	}
	sum.DurationMS = time.Since(started).Milliseconds()
	return sum, nil
}
