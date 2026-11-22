package football

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// API-Football predictions as a consensus source (v2.18).
//
// Endpoint: GET /predictions?fixture=<id> (https://www.api-football.com/documentation-v3#tag/Predictions,
// included in every plan, 1 request per call). The response is the usual
// envelope with response[0] = {predictions: {winner: {id, name, comment},
// win_or_draw, under_over, goals: {home, away}, advice, percent: {home: "45%",
// draw: "25%", away: "30%"}}, league, teams: {home, away}, comparison, h2h}.
// Only upcoming fixtures of enabled competitions in the next 48 h are asked
// once, plus one refresh about 3 h before kickoff, at the lowest priority
// of API-Football's daily budget and within this source's own daily budget.
// A prediction already fetched as v2.16's reference (fb_fixtures.prediction_ref)
// is reused without a new request.

type afPredEntry struct {
	Predictions struct {
		Winner *struct {
			ID      *int64  `json:"id"`
			Name    *string `json:"name"`
			Comment *string `json:"comment"`
		} `json:"winner"`
		WinOrDraw *bool   `json:"win_or_draw"`
		UnderOver *string `json:"under_over"`
		Goals     struct {
			Home *string `json:"home"`
			Away *string `json:"away"`
		} `json:"goals"`
		Advice  *string `json:"advice"`
		Percent struct {
			Home string `json:"home"`
			Draw string `json:"draw"`
			Away string `json:"away"`
		} `json:"percent"`
	} `json:"predictions"`
	Teams struct {
		Home struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"home"`
		Away struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"away"`
	} `json:"teams"`
}

// ParseAFPrediction reads a /predictions response list: the percentages
// ("45%") normalised to 1, and a detail with advice, winner, under/over.
func ParseAFPrediction(es []afPredEntry) (P3, map[string]any, bool) {
	if len(es) == 0 {
		return P3{}, nil, false
	}
	pr := es[0].Predictions
	p := P3{parsePct(pr.Percent.Home), parsePct(pr.Percent.Draw), parsePct(pr.Percent.Away)}
	s := p[0] + p[1] + p[2]
	if s <= 0 {
		return P3{}, nil, false
	}
	for k := range p {
		p[k] /= s
	}
	d := map[string]any{"advice": strp(pr.Advice), "under_over": strp(pr.UnderOver),
		"percent": map[string]string{"home": pr.Percent.Home, "draw": pr.Percent.Draw, "away": pr.Percent.Away}}
	if pr.Winner != nil {
		d["winner"] = strp(pr.Winner.Name)
		d["winner_comment"] = strp(pr.Winner.Comment)
	}
	if pr.WinOrDraw != nil {
		d["win_or_draw"] = *pr.WinOrDraw
	}
	return p, d, true
}

// apTarget is a fixture the API-Football predictions source may ask.
type apTarget struct {
	FixtureID int64
	Kickoff   time.Time
	FetchedAt *time.Time // stored source probability
	HasRef    bool       // v2.16 reference prediction already stored
}

// Task kinds of the v2.18 sources.
const (
	TAPPred     = "apifootball_pred"
	TAPImport   = "apifootball_pred_import"
	TPMSports   = "polymarket_sports"
	TPMEvents   = "polymarket_events"
	TOFFile     = "openfootball"
	winKindsAll = "apifootball_pred polymarket openfootball"
)

// PlanAFPred plans API-Football prediction requests (pure).
func PlanAFPred(now time.Time, ts []apTarget, due func(string, time.Duration) bool) []Task {
	out := []Task{}
	for _, t := range ts {
		until := t.Kickoff.Sub(now)
		if until <= 15*time.Minute || until > 48*time.Hour {
			continue
		}
		id := strconv.FormatInt(t.FixtureID, 10)
		if t.FetchedAt == nil {
			if t.HasRef {
				out = append(out, Task{Key: "ap:import:" + id, Kind: TAPImport, Provider: ProviderAFPred, Prio: PrioBackfill, Cost: 0, Fixtures: []int64{t.FixtureID},
					Reason: "reuse the stored API-Football prediction", Urgency: until.Hours()})
				continue
			}
			key := "ap:pred:" + id
			if due(key, 24*time.Hour) {
				out = append(out, Task{Key: key, Kind: TAPPred, Provider: ProviderAFPred, Prio: PrioBackfill, Cost: 1, Endpoint: "predictions", Fixtures: []int64{t.FixtureID},
					Params: url.Values{"fixture": {id}}, Reason: "API-Football prediction (next 48 h)", Urgency: until.Hours()})
			}
			continue
		}
		if until <= 3*time.Hour && t.FetchedAt.Before(t.Kickoff.Add(-3*time.Hour)) {
			key := "ap:pred3:" + id
			if due(key, 6*time.Hour) {
				out = append(out, Task{Key: key, Kind: TAPPred, Provider: ProviderAFPred, Prio: PrioLow, Cost: 1, Endpoint: "predictions", Fixtures: []int64{t.FixtureID},
					Params: url.Values{"fixture": {id}}, Reason: "API-Football prediction refresh (3 h before kickoff)", Urgency: until.Hours()})
			}
		}
	}
	return out
}

func (s *Service) apTargets(now time.Time) []apTarget {
	var rows []struct {
		ID        int64
		Kickoff   time.Time
		FetchedAt *time.Time
		HasRef    bool
	}
	s.DB.Raw(`SELECT f.id, f.kickoff, sp.fetched_at, (f.prediction_ref IS NOT NULL AND f.prediction_ref <> '' AND f.prediction_ref <> 'null') AS has_ref
		FROM fb_fixtures f JOIN fb_competitions c ON c.id = f.competition_id AND c.enabled
		LEFT JOIN fb_source_probs sp ON sp.fixture_id = f.id AND sp.source = ?
		WHERE f.status IN ('NS','TBD') AND f.kickoff > ? AND f.kickoff <= ? ORDER BY f.kickoff LIMIT 400`, ProviderAFPred, now, now.Add(48*time.Hour)).Scan(&rows)
	out := []apTarget{}
	for _, r := range rows {
		out = append(out, apTarget{FixtureID: r.ID, Kickoff: r.Kickoff, FetchedAt: r.FetchedAt, HasRef: r.HasRef})
	}
	return out
}

// storeAFPred stores a fixture's API-Football prediction as a source and
// keeps the v2.16 reference copy current.
func (s *Service) storeAFPred(fid int64, p P3, detail map[string]any, at time.Time) error {
	sp := SourceProb{FixtureID: fid, Source: ProviderAFPred, PHome: round4(p[0]), PDraw: round4(p[1]), PAway: round4(p[2]), Detail: detail, URL: docAFPred, FetchedAt: at}
	return s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&sp).Error
}

// runAFPred executes one planned task.
func (s *Service) runAFPred(ctx context.Context, t Task) (string, error) {
	fid := t.Fixtures[0]
	if t.Kind == TAPImport {
		var f Fixture
		if s.DB.Where("id = ?", fid).Limit(1).Find(&f).RowsAffected == 0 || f.PredictionRef == nil {
			return "no stored prediction", nil
		}
		r := f.PredictionRef
		p := P3{r.Home, r.Draw, r.Away}
		if p[0]+p[1]+p[2] <= 0 {
			return "empty stored prediction", nil
		}
		normalizeP3(&p)
		return "reused the stored prediction", s.storeAFPred(fid, p, map[string]any{"advice": r.Advice, "winner": r.Winner, "under_over": r.UnderOver, "reused": true}, r.FetchedAt)
	}
	st := s.FreeSettingsGet()
	budget := st.Budgets[ProviderAFPred]
	if budget <= 0 {
		budget = defaultFreeBudgets[ProviderAFPred]
	}
	if used := s.quotaRow(ProviderAFPred, dayPeriod(s.Now())).Used; used+1 > budget {
		return "", &ProviderError{Provider: ProviderAFPred, Kind: "budget", Msg: fmt.Sprintf("daily budget: %d of %d requests used", used, budget)}
	}
	out, err := s.call(ctx, callSpec{Provider: ProviderAPIFootball, Endpoint: "predictions", Params: t.Params, Cost: 1, Prio: t.Prio, Task: t.Key})
	now := s.Now()
	lg := FetchLog{Provider: ProviderAFPred, Endpoint: "predictions", Params: "fixture=" + strconv.FormatInt(fid, 10), Task: clip(t.Key, 120), At: now, Cost: 1}
	if err != nil {
		var pe *ProviderError
		if asProviderErr(err, &pe) && (pe.Kind == "budget" || pe.Kind == "backoff" || pe.Kind == "no_key") {
			return "", err
		}
		lg.Error = clip(err.Error(), 300)
		s.record(lg, nil, callSpec{Provider: ProviderAFPred}, 1)
		return "", err
	}
	var es []afPredEntry
	if err := s.decodeAF(out, &es); err != nil {
		lg.Error = "unreadable: " + clip(err.Error(), 200)
		s.record(lg, nil, callSpec{Provider: ProviderAFPred}, 1)
		return "", err
	}
	lg.OK, lg.Status, lg.Results = true, 200, len(es)
	s.record(lg, nil, callSpec{Provider: ProviderAFPred}, 1)
	p, d, ok := ParseAFPrediction(es)
	if !ok {
		s.DB.Model(&Fixture{}).Where("id = ?", fid).Update("pred_ref_at", now)
		return "no prediction for this fixture", nil
	}
	if err := s.storeAFPred(fid, p, d, now); err != nil {
		return "", err
	}
	ref := &RefPrediction{Home: p[0], Draw: p[1], Away: p[2], Advice: clip(fmt.Sprint(d["advice"]), 200), UnderOver: fmt.Sprint(d["under_over"]), FetchedAt: now}
	if w, ok := d["winner"].(string); ok {
		ref.Winner = w
	}
	b, _ := json.Marshal(ref)
	s.DB.Model(&Fixture{}).Where("id = ?", fid).Updates(map[string]any{"prediction_ref": string(b), "pred_ref_at": now})
	return fmt.Sprintf("%.0f%% / %.0f%% / %.0f%%", p[0]*100, p[1]*100, p[2]*100), nil
}

func normalizeP3(p *P3) {
	s := p[0] + p[1] + p[2]
	if s <= 0 {
		return
	}
	for k := range p {
		p[k] /= s
	}
}

func asProviderErr(err error, pe **ProviderError) bool { return errors.As(err, pe) }

// flexTime parses the time formats the sources use.
func flexTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05Z07", "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	if strings.HasSuffix(s, "+00") {
		if t, err := time.Parse("2006-01-02 15:04:05", strings.TrimSuffix(s, "+00")); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
