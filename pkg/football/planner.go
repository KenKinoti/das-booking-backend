package football

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The planner decides what to fetch now. It is a pure function of the
// state (PlanInput) so it can be tested without a database or network; the
// scheduler executes its tasks in priority order and the quota manager
// (Allow) refuses whatever doesn't fit the budget at that priority.
//
// Priority plan (highest first):
//  1. live fixtures every LiveIntervalSec (60 s) while in play — the interval
//     stretches when the day's budget can't pay for it; results of fixtures
//     that should have finished; closing odds (The Odds API, T-10 min).
//  2. today's fixtures, line-ups from T-60 min, odds snapshots T-1 h.
//  3. the next days' fixtures, standings after a matchday, injuries,
//     API-Football pre-match odds, earlier odds snapshots (T-24 h, T-6 h).
//  4. head-to-head, player season stats (weekly), match details (xG) for
//     history, top scorers.
//  5. previous seasons, team details, API-Football's reference predictions.

// PlanFixture is the planner's view of a fixture.
type PlanFixture struct {
	ID, Comp   int64
	Season     int
	Kickoff    time.Time
	Status     string
	HomeID     int64
	AwayID     int64
	DetailsAt  *time.Time
	LineupsAt  *time.Time
	InjuriesAt *time.Time
	PredRefAt  *time.Time
	H2HAt      *time.Time
	OddsOAAt   *time.Time
}

// PlanComp is the planner's view of an enabled competition.
type PlanComp struct {
	ID       int64
	Season   int
	SportKey string
	Type     string
	Coverage Coverage
	Known    bool
}

// PlanInput is everything the planner looks at.
type PlanInput struct {
	Now           time.Time
	Settings      Settings
	HasAF, HasOA  bool
	Comps         []PlanComp
	Fixtures      []PlanFixture
	State         map[string]TaskState
	StandingsAt   map[int64]time.Time // last standings snapshot per competition
	LastFinished  map[int64]time.Time // latest finished_at per competition
	PlayersAt     map[int64]time.Time // last player stats fetch per team
	Backlog       []int64             // finished fixtures without details, newest first
	AFUsedToday   int
	LiveUsedToday int             // API-Football requests spent on live polling today
	Force         bool            // manual refresh: ignore intervals (budgets still apply)
	Only          map[string]bool // manual refresh of some kinds only
}

// Task kinds.
const (
	TLeagues   = "leagues"
	TSeason    = "season"
	TDate      = "date"
	TLive      = "live"
	TResults   = "results"
	TPrematch  = "lineups"
	TInjuries  = "injuries"
	TStandings = "standings"
	TOddsAF    = "odds_af"
	TOddsOA    = "odds_oa"
	TH2H       = "h2h"
	TPlayers   = "players"
	TScorers   = "topscorers"
	TDetails   = "details"
	TPredRef   = "predictions"
	TTeams     = "teams"
	TSports    = "sports"
	TOddsLive  = "odds_live"
	TBooks     = "bookmakers"
)

// Task is one planned fetch.
type Task struct {
	Key      string     `json:"key"`
	Kind     string     `json:"kind"`
	Provider string     `json:"provider"`
	Prio     int        `json:"prio"`
	Cost     int        `json:"cost"`
	Reason   string     `json:"reason"`
	Endpoint string     `json:"endpoint"`
	Params   url.Values `json:"-"`
	Fixtures []int64    `json:"fixtures,omitempty"`
	Comp     int64      `json:"comp,omitempty"`
	Season   int        `json:"season,omitempty"`
	Team     int64      `json:"team,omitempty"`
	Pages    bool       `json:"pages,omitempty"`
	SportKey string     `json:"sport_key,omitempty"`
	Urgency  float64    `json:"-"` // smaller = sooner within a priority
}

// due reports whether a task key ran long enough ago. A failing task is
// retried sooner than its interval (10 min × failures, at most the interval).
func (in PlanInput) due(key string, every time.Duration) bool {
	if in.Force {
		return true
	}
	st, ok := in.State[key]
	if !ok {
		return true
	}
	if st.Status != "ok" && st.Status != "" {
		wait := time.Duration(st.Fails) * 10 * time.Minute
		if wait > every {
			wait = every
		}
		return in.Now.Sub(st.LastRunAt) >= wait
	}
	ref := st.LastRunAt
	if st.LastOKAt != nil {
		ref = *st.LastOKAt
	}
	return in.Now.Sub(ref) >= every
}

func (in PlanInput) want(kind string) bool { return len(in.Only) == 0 || in.Only[kind] }

func idsParam(ids []int64) string {
	p := make([]string, len(ids))
	for i, id := range ids {
		p[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(p, "-")
}

func chunk(ids []int64, n int) [][]int64 {
	out := [][]int64{}
	for i := 0; i < len(ids); i += n {
		j := i + n
		if j > len(ids) {
			j = len(ids)
		}
		out = append(out, ids[i:j])
	}
	return out
}

// InPlay: between 5 minutes before kickoff and 4 hours after, not final.
func InPlay(f PlanFixture, now time.Time) bool {
	if IsFinal(f.Status) || voidStatuses[f.Status] || f.Status == "PST" {
		return false
	}
	return !now.Before(f.Kickoff.Add(-5*time.Minute)) && now.Before(f.Kickoff.Add(4*time.Hour))
}

// LiveInterval stretches live polling so the day's remaining live budget
// (half of the API-Football daily budget) lasts for the in-play minutes
// left today.
func LiveInterval(in PlanInput) time.Duration {
	base := time.Duration(in.Settings.LiveIntervalSec) * time.Second
	if base < 30*time.Second {
		base = 60 * time.Second
	}
	budget := in.Settings.APIFootballDaily/2 - in.LiveUsedToday
	// Union of the remaining in-play windows today (each ~2 h from kickoff).
	type span struct{ a, b time.Time }
	spans := []span{}
	end := in.Now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	for _, f := range in.Fixtures {
		if IsFinal(f.Status) || voidStatuses[f.Status] {
			continue
		}
		a, b := f.Kickoff, f.Kickoff.Add(115*time.Minute)
		if b.Before(in.Now) && !InPlay(f, in.Now) {
			continue
		}
		if a.Before(in.Now) {
			a = in.Now
		}
		if b.Before(a) {
			b = a.Add(10 * time.Minute)
		}
		if a.After(end) {
			continue
		}
		if b.After(end) {
			b = end
		}
		spans = append(spans, span{a, b})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].a.Before(spans[j].a) })
	var mins float64
	var cur span
	for i, s := range spans {
		if i == 0 {
			cur = s
			continue
		}
		if !s.a.After(cur.b) {
			if s.b.After(cur.b) {
				cur.b = s.b
			}
			continue
		}
		mins += cur.b.Sub(cur.a).Minutes()
		cur = s
	}
	if len(spans) > 0 {
		mins += cur.b.Sub(cur.a).Minutes()
	}
	if budget <= 0 {
		return 24 * time.Hour
	}
	need := time.Duration(math.Ceil(mins*60/float64(budget))) * time.Second
	if need > base {
		return need
	}
	return base
}

// Plan lists the due tasks, highest priority (and most urgent) first.
func Plan(in PlanInput) []Task {
	now := in.Now
	st := in.Settings
	out := []Task{}
	add := func(t Task) { out = append(out, t) }
	comps := map[int64]PlanComp{}
	compIDs := []string{}
	for _, c := range in.Comps {
		comps[c.ID] = c
		compIDs = append(compIDs, strconv.FormatInt(c.ID, 10))
	}
	fixturesByComp := map[int64][]PlanFixture{}
	for _, f := range in.Fixtures {
		fixturesByComp[f.Comp] = append(fixturesByComp[f.Comp], f)
	}
	look := time.Duration(st.LookaheadDays) * 24 * time.Hour

	if in.HasAF {
		// 1. Leagues (coverage, current seasons) weekly.
		if in.want(TLeagues) && in.due("af:leagues", 7*24*time.Hour) {
			prio := PrioLow
			if _, ok := in.State["af:leagues"]; !ok {
				prio = PrioHigh
			}
			add(Task{Key: "af:leagues", Kind: TLeagues, Provider: ProviderAPIFootball, Prio: prio, Cost: 1, Endpoint: "leagues",
				Params: url.Values{"current": {"true"}}, Reason: "competitions, current seasons and coverage (weekly)"})
		}
		// Bookmakers API-Football carries (Kenyan ones?) monthly.
		if in.want(TBooks) && in.due("af:bookmakers", 30*24*time.Hour) {
			add(Task{Key: "af:bookmakers", Kind: TBooks, Provider: ProviderAPIFootball, Prio: PrioBackfill, Cost: 1, Endpoint: "odds/bookmakers",
				Reason: "bookmakers API-Football carries (monthly)"})
		}
		// 2. Live and overdue results.
		live, overdue := []int64{}, []int64{}
		for _, f := range in.Fixtures {
			switch {
			case InPlay(f, now):
				live = append(live, f.ID)
			case !IsFinal(f.Status) && !voidStatuses[f.Status] && now.After(f.Kickoff.Add(4*time.Hour)) && now.Before(f.Kickoff.Add(72*time.Hour)):
				overdue = append(overdue, f.ID)
			}
		}
		if len(live) > 0 && st.LivePolling && in.want(TLive) {
			every := LiveInterval(in)
			for i, ids := range chunk(live, 20) {
				key := fmt.Sprintf("af:live:%d", i)
				if in.due(key, every) || in.Force {
					add(Task{Key: key, Kind: TLive, Provider: ProviderAPIFootball, Prio: PrioCritical, Cost: 1, Endpoint: "fixtures",
						Params: url.Values{"ids": {idsParam(ids)}}, Fixtures: ids, Reason: fmt.Sprintf("in play: score, events, statistics (every %s)", every.Round(time.Second))})
				}
			}
		}
		if len(live) > 0 && st.LivePolling && st.APIFootballPlan != "free" && in.want(TOddsLive) && in.due("af:oddslive", 5*time.Minute) {
			add(Task{Key: "af:oddslive", Kind: TOddsLive, Provider: ProviderAPIFootball, Prio: PrioLow, Cost: 1, Endpoint: "odds/live",
				Reason: "in-play prices (API-Football, every 5 min while matches are live)"})
		}
		if len(overdue) > 0 && in.want(TResults) {
			for i, ids := range chunk(overdue, 20) {
				key := fmt.Sprintf("af:results:%d", i)
				if in.due(key, 30*time.Minute) {
					add(Task{Key: key, Kind: TResults, Provider: ProviderAPIFootball, Prio: PrioCritical, Cost: 1, Endpoint: "fixtures",
						Params: url.Values{"ids": {idsParam(ids)}}, Fixtures: ids, Reason: "results of fixtures that should have finished"})
				}
			}
		}
		// 3. Fixtures by date: today every 3 h, the next days every 12 h.
		if in.want(TDate) {
			for d := 0; d <= st.LookaheadDays; d++ {
				day := now.Truncate(24*time.Hour).AddDate(0, 0, d).Format("2006-01-02")
				every, prio := 12*time.Hour, PrioNormal
				if d == 0 {
					every, prio = 3*time.Hour, PrioHigh
				}
				key := "af:date:" + day
				if in.due(key, every) {
					add(Task{Key: key, Kind: TDate, Provider: ProviderAPIFootball, Prio: prio, Cost: 1, Endpoint: "fixtures", Urgency: float64(d),
						Params: url.Values{"date": {day}, "timezone": {"UTC"}}, Reason: "fixtures on " + day})
				}
			}
		}
		// 4. Line-ups from T-60 min (fixtures?ids= embeds them once published).
		if in.want(TPrematch) {
			pre := []int64{}
			for _, f := range in.Fixtures {
				c := comps[f.Comp]
				if f.LineupsAt == nil && f.Kickoff.After(now) && f.Kickoff.Sub(now) <= 60*time.Minute && (c.Coverage.Lineups || !c.Known) {
					pre = append(pre, f.ID)
				}
			}
			for i, ids := range chunk(pre, 20) {
				key := fmt.Sprintf("af:lineups:%d", i)
				if in.due(key, 15*time.Minute) {
					add(Task{Key: key, Kind: TPrematch, Provider: ProviderAPIFootball, Prio: PrioHigh, Cost: 1, Endpoint: "fixtures",
						Params: url.Values{"ids": {idsParam(ids)}}, Fixtures: ids, Reason: "line-ups (published ~20–40 min before kickoff)"})
				}
			}
		}
		for _, c := range in.Comps {
			season := c.Season
			fx := fixturesByComp[c.ID]
			var next *PlanFixture
			finishedToday := false
			for i := range fx {
				f := fx[i]
				if f.Kickoff.After(now) && (next == nil || f.Kickoff.Before(next.Kickoff)) {
					next = &fx[i]
				}
			}
			if lf, ok := in.LastFinished[c.ID]; ok && (in.StandingsAt[c.ID].IsZero() || lf.After(in.StandingsAt[c.ID])) {
				finishedToday = true
			}
			// History: the current season weekly, the previous once.
			if in.want(TSeason) {
				key := fmt.Sprintf("af:season:%d:%d", c.ID, season)
				prio := PrioLow
				if _, ok := in.State[key]; !ok {
					prio = PrioNormal
				}
				if in.due(key, 7*24*time.Hour) {
					add(Task{Key: key, Kind: TSeason, Provider: ProviderAPIFootball, Prio: prio, Cost: 1, Endpoint: "fixtures", Comp: c.ID, Season: season,
						Params: url.Values{"league": {strconv.FormatInt(c.ID, 10)}, "season": {strconv.Itoa(season)}}, Reason: "season fixtures and results"})
				}
				key = fmt.Sprintf("af:season:%d:%d", c.ID, season-1)
				if in.due(key, 120*24*time.Hour) {
					add(Task{Key: key, Kind: TSeason, Provider: ProviderAPIFootball, Prio: PrioBackfill, Cost: 1, Endpoint: "fixtures", Comp: c.ID, Season: season - 1,
						Params: url.Values{"league": {strconv.FormatInt(c.ID, 10)}, "season": {strconv.Itoa(season - 1)}}, Reason: "previous season's results (model history)"})
				}
			}
			// Standings after a matchday (at most every 2 h), else daily.
			if in.want(TStandings) && (c.Coverage.Standings || !c.Known) {
				key := fmt.Sprintf("af:standings:%d:%d", c.ID, season)
				every := 24 * time.Hour
				if finishedToday {
					every = 2 * time.Hour
				}
				if in.due(key, every) {
					add(Task{Key: key, Kind: TStandings, Provider: ProviderAPIFootball, Prio: PrioNormal, Cost: 1, Endpoint: "standings", Comp: c.ID, Season: season,
						Params: url.Values{"league": {strconv.FormatInt(c.ID, 10)}, "season": {strconv.Itoa(season)}}, Reason: "league table"})
				}
			}
			if next == nil || next.Kickoff.Sub(now) > look {
				// Nothing coming up: the rest only matters before matches.
				if in.want(TTeams) {
					key := fmt.Sprintf("af:teams:%d:%d", c.ID, season)
					if in.due(key, 120*24*time.Hour) {
						add(Task{Key: key, Kind: TTeams, Provider: ProviderAPIFootball, Prio: PrioBackfill, Cost: 1, Endpoint: "teams", Comp: c.ID, Season: season,
							Params: url.Values{"league": {strconv.FormatInt(c.ID, 10)}, "season": {strconv.Itoa(season)}}, Reason: "team details and venues"})
					}
				}
				continue
			}
			untilNext := next.Kickoff.Sub(now)
			// Injuries & suspensions (league list) every 12 h before matches, 3 h on the day.
			if in.want(TInjuries) && (c.Coverage.Injuries || !c.Known) && untilNext <= 36*time.Hour {
				key := fmt.Sprintf("af:injuries:%d:%d", c.ID, season)
				every, prio := 12*time.Hour, PrioNormal
				if untilNext <= 4*time.Hour {
					every, prio = 3*time.Hour, PrioHigh
				}
				if in.due(key, every) {
					ids := []int64{}
					for _, f := range fx {
						if f.Kickoff.After(now) && f.Kickoff.Sub(now) <= 36*time.Hour {
							ids = append(ids, f.ID)
						}
					}
					add(Task{Key: key, Kind: TInjuries, Provider: ProviderAPIFootball, Prio: prio, Cost: 1, Endpoint: "injuries", Comp: c.ID, Season: season, Fixtures: ids,
						Params: url.Values{"league": {strconv.FormatInt(c.ID, 10)}, "season": {strconv.Itoa(season)}}, Reason: "injuries and suspensions", Urgency: untilNext.Hours()})
				}
			}
			// API-Football pre-match odds (updated every 3 h) within 48 h.
			if in.want(TOddsAF) && (c.Coverage.Odds || !c.Known) && untilNext <= 48*time.Hour {
				key := fmt.Sprintf("af:odds:%d:%d", c.ID, season)
				every, prio := 6*time.Hour, PrioNormal
				if untilNext <= 6*time.Hour {
					every, prio = 3*time.Hour, PrioHigh
				}
				if in.due(key, every) {
					add(Task{Key: key, Kind: TOddsAF, Provider: ProviderAPIFootball, Prio: prio, Cost: 1, Endpoint: "odds", Comp: c.ID, Season: season, Pages: true,
						Params: url.Values{"league": {strconv.FormatInt(c.ID, 10)}, "season": {strconv.Itoa(season)}}, Reason: "pre-match odds (API-Football bookmakers)", Urgency: untilNext.Hours()})
				}
			}
			// Top scorers weekly.
			if in.want(TScorers) && (c.Coverage.TopScorers || !c.Known) {
				key := fmt.Sprintf("af:topscorers:%d:%d", c.ID, season)
				if in.due(key, 7*24*time.Hour) {
					add(Task{Key: key, Kind: TScorers, Provider: ProviderAPIFootball, Prio: PrioLow, Cost: 1, Endpoint: "players/topscorers", Comp: c.ID, Season: season,
						Params: url.Values{"league": {strconv.FormatInt(c.ID, 10)}, "season": {strconv.Itoa(season)}}, Reason: "top scorers"})
				}
			}
			for _, f := range fx {
				if !f.Kickoff.After(now) || f.Kickoff.Sub(now) > look {
					continue
				}
				// Head-to-head once a week per pair.
				if in.want(TH2H) && (f.H2HAt == nil || now.Sub(*f.H2HAt) > 7*24*time.Hour || in.Force) {
					a, b := f.HomeID, f.AwayID
					if a > b {
						a, b = b, a
					}
					key := fmt.Sprintf("af:h2h:%d-%d", a, b)
					if in.due(key, 7*24*time.Hour) {
						add(Task{Key: key, Kind: TH2H, Provider: ProviderAPIFootball, Prio: PrioLow, Cost: 1, Endpoint: "fixtures/headtohead", Fixtures: []int64{f.ID},
							Params: url.Values{"h2h": {fmt.Sprintf("%d-%d", f.HomeID, f.AwayID)}, "last": {"10"}}, Reason: "head-to-head (last 10)", Urgency: f.Kickoff.Sub(now).Hours()})
					}
				}
				// Player season stats weekly for teams about to play.
				if in.want(TPlayers) && (c.Coverage.Players || !c.Known) {
					for _, team := range []int64{f.HomeID, f.AwayID} {
						if t, ok := in.PlayersAt[team]; ok && now.Sub(t) < 7*24*time.Hour && !in.Force {
							continue
						}
						key := fmt.Sprintf("af:players:%d:%d", team, season)
						if in.due(key, 7*24*time.Hour) {
							add(Task{Key: key, Kind: TPlayers, Provider: ProviderAPIFootball, Prio: PrioLow, Cost: 1, Endpoint: "players", Team: team, Season: season, Comp: c.ID, Pages: true,
								Params: url.Values{"team": {strconv.FormatInt(team, 10)}, "season": {strconv.Itoa(season)}}, Reason: "player season statistics (weekly)", Urgency: f.Kickoff.Sub(now).Hours()})
						}
					}
				}
				// API-Football's own prediction, for reference only.
				if in.want(TPredRef) && f.PredRefAt == nil && f.Kickoff.Sub(now) <= 48*time.Hour && (c.Coverage.Predictions || !c.Known) {
					key := fmt.Sprintf("af:pred:%d", f.ID)
					if in.due(key, 24*time.Hour) {
						add(Task{Key: key, Kind: TPredRef, Provider: ProviderAPIFootball, Prio: PrioBackfill, Cost: 1, Endpoint: "predictions", Fixtures: []int64{f.ID},
							Params: url.Values{"fixture": {strconv.FormatInt(f.ID, 10)}}, Reason: "API-Football prediction (reference only)", Urgency: f.Kickoff.Sub(now).Hours()})
					}
				}
			}
		}
		// Match details (xG, events, player ratings) for history.
		if in.want(TDetails) && len(in.Backlog) > 0 {
			for i, ids := range chunk(in.Backlog, 20) {
				if i >= 3 {
					break
				}
				key := fmt.Sprintf("af:details:%d", i)
				if in.due(key, 20*time.Minute) {
					add(Task{Key: key, Kind: TDetails, Provider: ProviderAPIFootball, Prio: PrioLow, Cost: 1, Endpoint: "fixtures", Fixtures: ids, Urgency: float64(i),
						Params: url.Values{"ids": {idsParam(ids)}}, Reason: "match statistics incl. expected goals for the model"})
				}
			}
		}
	}

	if in.HasOA {
		if in.want(TSports) && in.due("oa:sports", 7*24*time.Hour) {
			add(Task{Key: "oa:sports", Kind: TSports, Provider: ProviderOddsAPI, Prio: PrioLow, Cost: 0, Endpoint: "/v4/sports", Reason: "soccer competitions in season (free)"})
		}
		// Odds snapshots per sport at T-24 h, T-6 h, T-1 h, T-10 min: one call
		// refreshes every event of the sport (cost markets × regions).
		if in.want(TOddsOA) {
			offs := st.Snapshots()
			cost := st.OddsCallCost()
			type need struct {
				prio    int
				urgency float64
				reason  string
			}
			bySport := map[string]*need{}
			sportComps := map[string][]int64{}
			for _, c := range in.Comps {
				if c.SportKey != "" {
					sportComps[c.SportKey] = append(sportComps[c.SportKey], c.ID)
				}
			}
			for _, f := range in.Fixtures {
				c := comps[f.Comp]
				if c.SportKey == "" || !f.Kickoff.After(now) || IsFinal(f.Status) {
					continue
				}
				until := f.Kickoff.Sub(now)
				// The window reached: the smallest offset not yet passed.
				win := -1
				for i, o := range offs {
					if until <= time.Duration(o)*time.Minute {
						win = i
					}
				}
				if win < 0 {
					continue
				}
				o := offs[win]
				if f.OddsOAAt != nil && !f.OddsOAAt.Before(f.Kickoff.Add(-time.Duration(o)*time.Minute)) && !in.Force {
					continue // already have a snapshot inside this window
				}
				prio := PrioNormal
				switch {
				case o <= 15:
					prio = PrioCritical
				case o <= 60:
					prio = PrioHigh
				}
				n := bySport[c.SportKey]
				if n == nil || prio < n.prio {
					bySport[c.SportKey] = &need{prio: prio, urgency: until.Minutes(), reason: fmt.Sprintf("odds snapshot T-%s", fmtMinutes(o))}
				}
			}
			for sport, n := range bySport {
				key := "oa:odds:" + sport
				if !in.due(key, 8*time.Minute) {
					continue
				}
				p := url.Values{"regions": {strings.Join(st.Regions(), ",")}, "markets": {strings.Join(st.Markets(), ",")}, "oddsFormat": {"decimal"},
					"dateFormat": {"iso"}, "commenceTimeFrom": {now.Add(-10 * time.Minute).Format("2006-01-02T15:04:05Z")},
					"commenceTimeTo": {now.Add(look).Format("2006-01-02T15:04:05Z")}}
				add(Task{Key: key, Kind: TOddsOA, Provider: ProviderOddsAPI, Prio: n.prio, Cost: cost, Endpoint: "/v4/sports/" + sport + "/odds",
					Params: p, SportKey: sport, Urgency: n.urgency, Reason: n.reason})
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
	_ = compIDs
	return out
}

func fmtMinutes(m int) string {
	switch {
	case m%1440 == 0:
		return fmt.Sprintf("%dh", m/60)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	}
	return fmt.Sprintf("%dmin", m)
}
