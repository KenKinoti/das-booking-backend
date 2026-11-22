package football

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Planning and running the v2.18 sources inside the free-sources pass
// (freeplan.go runFree): API-Football predictions, Polymarket, openfootball.

// WinPlanInput is the planner's view.
type WinPlanInput struct {
	Now      time.Time
	On       map[string]bool
	State    map[string]TaskState
	Force    bool
	Only     map[string]bool
	AP       []apTarget
	PMLeague map[string]int64 // sport → competition with fixtures soon
	PMSports bool             // the /sports cache is fresh (< 24 h)
	OFFiles  []string
	Season   string
}

func (in WinPlanInput) due(key string, every time.Duration) bool {
	return PlanInput{Now: in.Now, State: in.State, Force: in.Force}.due(key, every)
}

func (in WinPlanInput) want(g string) bool { return len(in.Only) == 0 || in.Only[g] }

// PlanWin lists the due v2.18 source tasks, highest priority first (pure).
func PlanWin(in WinPlanInput) []Task {
	out := []Task{}
	if in.On[ProviderAFPred] && in.want(ProviderAFPred) {
		out = append(out, PlanAFPred(in.Now, in.AP, in.due)...)
	}
	if in.On[ProviderPolymarket] && in.want(ProviderPolymarket) && len(in.PMLeague) > 0 {
		if !in.PMSports && in.due("pm:sports", 20*time.Hour) {
			out = append(out, Task{Key: "pm:sports", Kind: TPMSports, Provider: ProviderPolymarket, Prio: PrioNormal, Cost: 1, Endpoint: "/sports", Reason: "Polymarket sports and tags (daily)"})
		}
		for _, sport := range sortedKeys(in.PMLeague) {
			key := "pm:events:" + sport
			if in.due(key, time.Hour) {
				out = append(out, Task{Key: key, Kind: TPMEvents, Provider: ProviderPolymarket, Prio: PrioLow, Cost: 1, Endpoint: "/events", Comp: in.PMLeague[sport],
					Params: url.Values{"sport": {sport}}, Urgency: 1, Reason: "Polymarket match markets: " + sport})
			}
		}
	}
	if in.On[ProviderOpenFootball] && in.want(ProviderOpenFootball) {
		for _, code := range in.OFFiles {
			key := "of:" + code + ":" + in.Season
			if in.due(key, 20*time.Hour) {
				out = append(out, Task{Key: key, Kind: TOFFile, Provider: ProviderOpenFootball, Prio: PrioLow, Cost: 1, Endpoint: "/" + in.Season + "/" + code + ".json",
					Params: url.Values{"code": {code}, "season": {in.Season}}, Comp: OFLeagues[code].Comp, Reason: "openfootball fixtures and results: " + code})
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

func winOnly(only map[string]bool) map[string]bool {
	if len(only) == 0 {
		return nil
	}
	out := map[string]bool{}
	for k := range only {
		for _, g := range strings.Fields(winKindsAll) {
			if k == g {
				out[g] = true
			}
		}
	}
	if len(out) == 0 {
		return map[string]bool{"__none__": true}
	}
	return out
}

func (s *Service) winPlanInput(o RunOpts) WinPlanInput {
	now := s.Now()
	in := WinPlanInput{Now: now, On: map[string]bool{}, State: map[string]TaskState{}, Force: o.Force, Only: winOnly(o.Only), Season: OFSeason(now)}
	for _, p := range []string{ProviderAFPred, ProviderPolymarket, ProviderOpenFootball} {
		in.On[p] = s.FreeOn(p)
	}
	if af, _ := s.Keys(); af == "" {
		in.On[ProviderAFPred] = false // stored predictions stay usable; new ones need the key
	}
	var states []TaskState
	s.DB.Where("key LIKE 'ap:%' OR key LIKE 'pm:%' OR key LIKE 'of:%'").Find(&states)
	for _, t := range states {
		in.State[t.Key] = t
	}
	if in.On[ProviderAFPred] {
		in.AP = s.apTargets(now)
	}
	if in.On[ProviderPolymarket] {
		in.PMLeague = s.pmTargets(now)
		st := s.WinSettingsGet()
		in.PMSports = st.PolySportsAt != nil && now.Sub(*st.PolySportsAt) < 24*time.Hour && len(st.PolySports) > 0
	}
	if in.On[ProviderOpenFootball] {
		in.OFFiles = ofCodes(s.WinSettingsGet().OFFiles)
	}
	return in
}

// winPerPass caps requests per source in one pass (the rest stay due).
var winPerPass = map[string]int{ProviderAFPred: 20, ProviderPolymarket: 6, ProviderOpenFootball: 4}

// runWinSources plans and runs the v2.18 source tasks (from runFree).
func (s *Service) runWinSources(ctx context.Context, o RunOpts, sum *RunSummary) {
	tasks := PlanWin(s.winPlanInput(o))
	ran := map[string]int{}
	blocked := map[string]bool{}
	for _, t := range tasks {
		if ctx.Err() != nil {
			break
		}
		sum.Planned++
		if blocked[t.Provider] || ran[t.Provider] >= winPerPass[t.Provider] {
			sum.Skipped++
			continue
		}
		if t.Cost > 0 {
			ran[t.Provider]++
		}
		tr := s.runWinTask(ctx, t)
		switch tr.Status {
		case "ok":
			sum.Done++
		case "skipped":
			sum.Skipped++
			if strings.HasPrefix(tr.Note, "budget") || strings.HasPrefix(tr.Note, "backoff") {
				blocked[t.Provider] = true
			}
		default:
			sum.Failed++
		}
		sum.Cost[t.Provider] += tr.Cost
		if len(sum.Tasks) < 120 {
			sum.Tasks = append(sum.Tasks, tr)
		}
		// Polymarket: the sports list first, then the leagues in the same pass.
		if t.Kind == TPMSports && tr.Status != "ok" {
			blocked[ProviderPolymarket] = true
		}
	}
}

func (s *Service) runWinTask(ctx context.Context, t Task) TaskRun {
	tr := TaskRun{Key: t.Key, Kind: t.Kind, Provider: t.Provider, Prio: t.Prio}
	before := s.quotaRow(t.Provider, dayPeriod(s.Now())).Used
	note, err := s.executeWin(ctx, t)
	tr.Cost = s.quotaRow(t.Provider, dayPeriod(s.Now())).Used - before
	if err != nil {
		var pe *ProviderError
		if errors.As(err, &pe) && (pe.Kind == "budget" || pe.Kind == "backoff" || pe.Kind == "no_key") {
			tr.Status, tr.Note = "skipped", pe.Kind+": "+pe.Msg
			return tr
		}
		if errors.As(err, &pe) && pe.Kind == "not_found" && t.Kind == TOFFile {
			tr.Status, tr.Note = "ok", "no file for this season yet"
			s.saveTaskState(t.Key, true, tr.Note)
			return tr
		}
		if !errors.As(err, &pe) && t.Provider != ProviderAFPred {
			s.markLogError(t.Provider, t.Key, "unreadable: "+err.Error())
		}
		tr.Status, tr.Note = "failed", clip(err.Error(), 200)
		s.saveTaskState(t.Key, false, tr.Note)
		return tr
	}
	tr.Status, tr.Note = "ok", note
	if t.Kind != TAPImport {
		s.saveTaskState(t.Key, true, note)
	}
	return tr
}

func (s *Service) executeWin(ctx context.Context, t Task) (string, error) {
	switch t.Kind {
	case TAPPred, TAPImport:
		return s.runAFPred(ctx, t)
	case TPMSports:
		n, err := s.fetchPMSports(ctx, t.Prio, t.Key)
		return fmt.Sprintf("%d sports", n), err
	case TPMEvents:
		n, m, err := s.fetchPMEvents(ctx, t.Params.Get("sport"), t.Comp, t.Prio, t.Key)
		return fmt.Sprintf("%d match events, %d matched", n, m), err
	case TOFFile:
		n, m, err := s.fetchOFFile(ctx, t.Params.Get("code"), t.Params.Get("season"), t.Prio, t.Key)
		return fmt.Sprintf("%d matches, %d linked", n, m), err
	}
	return "", fmt.Errorf("unknown task %s", t.Kind)
}
