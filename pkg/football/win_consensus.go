package football

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football/model"
)

// Consensus probability per fixture: a weighted average of the sources that
// exist for it. Market prices are de-margined, never "corrected" (the open
// history shows the market is well calibrated on draws).

// Consensus source keys in display order.
var ConsKeys = []string{"sharp", "market", "polymarket", "clubelo", "model", "apifootball_pred"}

// DefaultConsWeights are used until learned weights beat them out of sample.
var DefaultConsWeights = map[string]float64{"sharp": 0.55, "market": 0.20, "clubelo": 0.08, "model": 0.08, "polymarket": 0.05, "apifootball_pred": 0.04}

var consLabels = map[string]string{"sharp": "Sharp bookmaker", "market": "Bookmakers' consensus", "polymarket": "Polymarket", "clubelo": "ClubElo",
	"model": "DASYIN model (Dixon–Coles + Elo)", "apifootball_pred": "API-Football prediction"}

// Competition groups of the learned weights.
var consGroups = []string{"top5", "europe", "world"}

var groupLabels = map[string]string{"top5": "Top-5 Europe", "europe": "Rest of Europe", "world": "Africa & rest of world"}

// top5Comps are API-Football's Premier League, La Liga, Bundesliga, Serie A, Ligue 1.
var top5Comps = map[int64]bool{39: true, 140: true, 78: true, 135: true, 61: true}

var europeCountries = map[string]bool{"england": true, "scotland": true, "wales": true, "northern-ireland": true, "northern ireland": true, "ireland": true,
	"spain": true, "germany": true, "italy": true, "france": true, "netherlands": true, "portugal": true, "belgium": true, "austria": true, "switzerland": true,
	"poland": true, "czech-republic": true, "czech republic": true, "slovakia": true, "hungary": true, "romania": true, "bulgaria": true, "serbia": true,
	"croatia": true, "slovenia": true, "bosnia": true, "greece": true, "turkey": true, "cyprus": true, "denmark": true, "norway": true, "sweden": true,
	"finland": true, "iceland": true, "russia": true, "ukraine": true, "belarus": true, "lithuania": true, "latvia": true, "estonia": true, "israel": true,
	"luxembourg": true, "malta": true, "albania": true, "north-macedonia": true, "montenegro": true, "kosovo": true, "moldova": true, "georgia": true,
	"armenia": true, "azerbaijan": true, "kazakhstan": true, "faroe-islands": true, "andorra": true, "san-marino": true, "gibraltar": true}

// uefaComps: Champions League, Europa League, Conference League, European qualifiers.
var uefaComps = map[int64]bool{2: true, 3: true, 848: true, 32: true, 4: true, 5: true}

// groupOf names a competition's weights group.
func groupOf(c Competition) string {
	switch {
	case top5Comps[c.ID]:
		return "top5"
	case uefaComps[c.ID] || c.Group == "uefa" || europeCountries[strings.ToLower(strings.TrimSpace(c.Country))]:
		return "europe"
	}
	return "world"
}

// consInputs are a fixture's consensus sources (nil = missing) with labels.
type consInputs struct {
	P       map[string]*P3
	Label   map[string]string
	Detail  map[string]string
	AsOf    map[string]*time.Time
	URL     map[string]string
	Why     map[string]string
	Over    *float64
	OverSrc string
	BTTS    *float64
	BTTSSrc string
	Fav     *P3 // the bookmakers' view (sharp else market) for the favourite baseline
	// v2.19.1 data gate (lock_gate.go): what the model knows, how many API
	// bookmakers priced the match, a Kenyan / OddsPapi price, typed-in prices.
	Model  *ModelData
	Books  int
	KEorOP bool
	Typed  bool
}

func newConsInputs() consInputs {
	return consInputs{P: map[string]*P3{}, Label: map[string]string{}, Detail: map[string]string{}, AsOf: map[string]*time.Time{}, URL: map[string]string{}, Why: map[string]string{}}
}

// marketSources reads the sharp book (Shin) and the median of every
// bookmaker (proportional) from a market view's 1X2 prices. Hand-typed
// prices never count.
func marketSources(mv MarketView, in *consInputs) {
	type triple struct {
		o       [3]float64
		book    string
		title   string
		fetched time.Time
	}
	books := map[string]*triple{}
	for _, so := range mv.Selections {
		if so.Market != Mkt1X2 {
			continue
		}
		idx := map[string]int{SelHome: 0, SelDraw: 1, SelAway: 2}[so.Selection]
		if so.Selection != SelHome && so.Selection != SelDraw && so.Selection != SelAway {
			continue
		}
		for _, pr := range so.Prices {
			if strings.HasPrefix(pr.Source, "manual") {
				continue
			}
			k := pr.Source + ":" + pr.Bookmaker
			t := books[k]
			if t == nil {
				t = &triple{book: pr.Bookmaker, title: pr.Title}
				books[k] = t
			}
			t.o[idx] = pr.Price
			if pr.FetchedAt.After(t.fetched) {
				t.fetched = pr.FetchedAt
			}
		}
	}
	keys := make([]string, 0, len(books))
	for k := range books {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var hs, ds, as []float64
	var latest time.Time
	for _, k := range keys {
		t := books[k]
		if t.o[0] <= 1 || t.o[1] <= 1 || t.o[2] <= 1 {
			continue
		}
		ps, _ := model.DeVig(t.o[:], model.DeVigProportional)
		if ps == nil {
			continue
		}
		hs, ds, as = append(hs, ps[0]), append(ds, ps[1]), append(as, ps[2])
		if t.fetched.After(latest) {
			latest = t.fetched
		}
	}
	// Sharp: the first sharp book in preference order.
	for _, pref := range SharpBooks {
		for _, k := range keys {
			t := books[k]
			if t.book != pref || t.o[0] <= 1 || t.o[1] <= 1 || t.o[2] <= 1 {
				continue
			}
			ps, m := model.DeVig(t.o[:], model.DeVigShin)
			if ps == nil {
				continue
			}
			p := P3{ps[0], ps[1], ps[2]}
			in.P["sharp"] = &p
			in.Label["sharp"] = sharpTitle(pref) + " (de-margined, Shin)"
			in.Detail["sharp"] = fmt.Sprintf("%s %.2f / %.2f / %.2f, margin %.1f%%", sharpTitle(pref), t.o[0], t.o[1], t.o[2], m*100)
			f := t.fetched
			in.AsOf["sharp"] = &f
			break
		}
		if in.P["sharp"] != nil {
			break
		}
	}
	if in.P["sharp"] == nil {
		in.Why["sharp"] = "no Pinnacle / exchange price"
	}
	if len(hs) > 0 {
		p := P3{median(hs), median(ds), median(as)}
		s := p[0] + p[1] + p[2]
		for k := range p {
			p[k] /= s
		}
		in.P["market"] = &p
		in.Label["market"] = fmt.Sprintf("Bookmakers' consensus (median of %d, de-margined)", len(hs))
		if len(hs) == 1 {
			in.Label["market"] = "1 bookmaker (de-margined)"
		}
		in.Detail["market"] = fmt.Sprintf("%d bookmaker(s)", len(hs))
		l := latest
		in.AsOf["market"] = &l
	} else {
		in.Why["market"] = "no bookmaker prices stored"
	}
	if in.P["sharp"] != nil {
		in.Fav = in.P["sharp"]
	} else {
		in.Fav = in.P["market"]
	}
	// Goal markets from the market's fair prices (sharp when available).
	for _, so := range mv.Selections {
		if so.FairP == nil {
			continue
		}
		if so.Market == MktOU && so.Line != nil && *so.Line == 2.5 && so.Selection == SelOver && in.Over == nil {
			v := *so.FairP
			in.Over, in.OverSrc = &v, "market"
		}
		if so.Market == MktBTTS && so.Selection == SelYes && in.BTTS == nil {
			v := *so.FairP
			in.BTTS, in.BTTSSrc = &v, "market"
		}
	}
}

// consensusInputs gathers every source of a fixture. pred is the model's
// prediction to use (nil = none); before limits stored external
// probabilities to those fetched before then (frozen verdicts).
func (s *Service) consensusInputs(f Fixture, mv MarketView, pred *Prediction, before *time.Time) consInputs {
	in := newConsInputs()
	marketSources(mv, &in)
	in.Books, in.KEorOP = marketEvidence(mv)
	s.typedMarket(f, before, &in) // v2.19.1: a price typed in by hand when nothing was fetched
	// v2.19.1: the model counts only when it knows the teams (lock_gate.go);
	// an unfitted default is not a probability.
	md := s.modelDataFor(f, pred)
	in.Model = &md
	if pred != nil && pred.PHome+pred.PDraw+pred.PAway > 0 && !md.Solid {
		in.Why["model"] = md.Why
	} else if pred != nil && pred.PHome+pred.PDraw+pred.PAway > 0 {
		p := P3{pred.PHome, pred.PDraw, pred.PAway}
		in.P["model"] = &p
		in.Label["model"] = consLabels["model"]
		t := pred.ComputedAt
		in.AsOf["model"] = &t
		in.Detail["model"] = pred.Kind + " prediction, " + pred.ComputedAt.Format("2006-01-02 15:04") + " UTC"
		if in.Over == nil {
			if v, ok := pred.Detail.Over["2.5"]; ok {
				vv := v
				in.Over, in.OverSrc = &vv, "model"
			}
		}
		if in.BTTS == nil && pred.Detail.BTTS != nil {
			v := *pred.Detail.BTTS
			in.BTTS, in.BTTSSrc = &v, "model"
		}
	} else if in.Why["model"] == "" {
		in.Why["model"] = "no model prediction"
	}
	if s.FreeOn(ProviderClubElo) {
		if ce := s.clubEloFor(f); ce.Available && ce.Probs != nil {
			p := P3{ce.Probs["home"], ce.Probs["draw"], ce.Probs["away"]}
			in.P["clubelo"] = &p
			in.Label["clubelo"] = "ClubElo"
			in.AsOf["clubelo"] = ce.FetchedAt
			in.URL["clubelo"] = ce.URL
			if ce.Home != nil && ce.Away != nil {
				in.Detail["clubelo"] = fmt.Sprintf("%s %.0f v %s %.0f (%s)", ce.Home.Club, ce.Home.Elo, ce.Away.Club, ce.Away.Elo, strings.ReplaceAll(ce.ProbsSource, "_", " "))
			}
		} else {
			in.Why["clubelo"] = "ClubElo has no rating for one of the teams"
		}
	} else {
		in.Why["clubelo"] = "switched off"
	}
	var sps []SourceProb
	s.DB.Where("fixture_id = ?", f.ID).Find(&sps)
	got := map[string]SourceProb{}
	for _, sp := range sps {
		if before != nil && !sp.FetchedAt.Before(*before) {
			continue
		}
		got[sp.Source] = sp
	}
	for _, key := range []string{ProviderPolymarket, ProviderAFPred} {
		if !s.FreeOn(key) {
			in.Why[key] = "switched off"
			continue
		}
		sp, ok := got[key]
		if !ok || sp.PHome+sp.PDraw+sp.PAway <= 0 {
			if key == ProviderPolymarket {
				in.Why[key] = "no usable market for this match (thin markets are ignored)"
			} else {
				in.Why[key] = "no prediction fetched for this match"
			}
			continue
		}
		p := P3{sp.PHome, sp.PDraw, sp.PAway}
		in.P[key] = &p
		in.Label[key] = consLabels[key]
		t := sp.FetchedAt
		in.AsOf[key] = &t
		in.URL[key] = sp.URL
		if key == ProviderPolymarket {
			in.Detail[key] = fmt.Sprintf("mid prices of %v markets", sp.Detail["n_markets"])
		} else if a, ok := sp.Detail["advice"].(string); ok {
			in.Detail[key] = a
		}
	}
	return in
}

// consWeightsFor returns the weights for a group (learned when they passed
// the holdout check) and where they came from.
func (s *Service) consWeightsFor(group string) (map[string]float64, string) {
	if w := cachedLearned(s, group); w != nil {
		return w, "learned"
	}
	return DefaultConsWeights, "default"
}

var learnedMu sync.Mutex
var learnedCache = struct {
	at time.Time
	m  map[string]map[string]float64
}{}

func cachedLearned(s *Service, group string) map[string]float64 {
	learnedMu.Lock()
	defer learnedMu.Unlock()
	if learnedCache.m == nil || time.Since(learnedCache.at) > 5*time.Minute {
		m := map[string]map[string]float64{}
		var rows []ConsensusWeightsRow
		s.DB.Where("in_use = 'learned'").Find(&rows)
		for _, r := range rows {
			m[r.Grp] = r.Weights
		}
		learnedCache.m, learnedCache.at = m, time.Now()
	}
	return learnedCache.m[group]
}

func resetLearnedCache() {
	learnedMu.Lock()
	learnedCache.m = nil
	learnedMu.Unlock()
}

// BlendConsensus applies the weights to the present sources: the market
// takes the sharp book's weight when there is no sharp price; missing
// sources renormalise. It returns the blend and the effective weights.
func BlendConsensus(ps map[string]*P3, w map[string]float64) (P3, map[string]float64) {
	eff := map[string]float64{}
	for _, k := range ConsKeys {
		if ps[k] != nil && w[k] > 0 {
			eff[k] = w[k]
		}
	}
	if ps["sharp"] == nil && ps["market"] != nil {
		eff["market"] = w["market"] + w["sharp"]
	}
	tot := 0.0
	for _, v := range eff {
		tot += v
	}
	var out P3
	if tot <= 0 {
		return out, map[string]float64{}
	}
	for k, v := range eff {
		eff[k] = v / tot
		for i := 0; i < 3; i++ {
			out[i] += eff[k] * ps[k][i]
		}
	}
	s := out[0] + out[1] + out[2]
	if s > 0 {
		for i := range out {
			out[i] /= s
		}
	}
	return out, eff
}

// verdictFromInputs builds a fixture's verdict from its inputs (nil when
// the data gate finds no solid source — lock_gate.go).
func (s *Service) verdictFromInputs(f Fixture, comp Competition, in consInputs, own map[string]*OwnRecord) *Verdict {
	v, _ := s.verdictGated(f, comp, in, own)
	return v
}

// verdictGated is verdictFromInputs with the data gate's answer (why there
// is no pick, or which solid sources back it).
func (s *Service) verdictGated(f Fixture, comp Competition, in consInputs, own map[string]*OwnRecord) (*Verdict, GateOut) {
	g := gateFor(in)
	if !g.OK {
		return nil, g
	}
	group := groupOf(comp)
	w, from := s.consWeightsFor(group)
	blend, eff := BlendConsensus(in.P, w)
	if len(eff) == 0 {
		g.OK, g.Reason = false, "No prices and too little match history for these teams"
		return nil, g
	}
	srcs := []ConsSource{}
	missing := []ConsMissing{}
	var asOf time.Time
	for _, k := range ConsKeys {
		if p := in.P[k]; p != nil && eff[k] > 0 {
			srcs = append(srcs, ConsSource{Key: k, Label: firstNonEmpty(in.Label[k], consLabels[k]), Probs: probsMap(*p), Weight: round4(eff[k]), AsOf: in.AsOf[k], URL: in.URL[k], Detail: in.Detail[k]})
			if in.AsOf[k] != nil && in.AsOf[k].After(asOf) {
				asOf = *in.AsOf[k]
			}
		} else if p == nil {
			why := in.Why[k]
			if k == "sharp" && in.P["market"] != nil {
				why += " (the bookmakers' consensus takes its weight)"
			}
			missing = append(missing, ConsMissing{Key: k, Label: consLabels[k], Why: why})
		}
	}
	if asOf.IsZero() {
		asOf = s.Now()
	}
	v := BuildVerdict(VerdictInput{Home: s.teamName(f.HomeID), Away: s.teamName(f.AwayID), Probs: blend, Sources: srcs, Missing: missing, WeightsFrom: from,
		Group: group, MarketUsed: in.P["sharp"] != nil || in.P["market"] != nil, Over: in.Over, OverSource: in.OverSrc, BTTS: in.BTTS, BTTSSource: in.BTTSSrc,
		Own: own, AsOf: asOf})
	capModelOnly(&v, g)
	if g.ModelOnly && in.P["market"] != nil {
		v.Tier.Note = "One bookmaker's price and the model only — less reliable"
	}
	if v.Tier.Capped && own != nil {
		v.Tier.Own = own["good"]
	}
	v.Data = &g
	return &v, g
}

// ---------------------------------------------------------------- own record

var ownMu sync.Mutex
var ownCache = struct {
	at time.Time
	m  map[string]*OwnRecord
}{}

// ownRecords is DASYIN's graded record per tier (≥ 30 graded to show).
func (s *Service) ownRecords() map[string]*OwnRecord {
	ownMu.Lock()
	defer ownMu.Unlock()
	if ownCache.m != nil && time.Since(ownCache.at) < 10*time.Minute {
		return ownCache.m
	}
	var rows []struct {
		Tier    string
		N       int
		Correct int
	}
	s.DB.Raw(`SELECT tier, COUNT(*) AS n, SUM(CASE WHEN status = 'correct' THEN 1 ELSE 0 END) AS correct FROM fb_verdicts
		WHERE status IN ('correct','wrong') GROUP BY tier`).Scan(&rows)
	m := map[string]*OwnRecord{}
	for _, r := range rows {
		if r.N >= 30 {
			m[r.Tier] = &OwnRecord{HitRate: round4(float64(r.Correct) / float64(r.N)), N: r.N}
		}
	}
	ownCache.m, ownCache.at = m, time.Now()
	return m
}

func resetOwnCache() {
	ownMu.Lock()
	ownCache.m = nil
	ownMu.Unlock()
}

// ---------------------------------------------------------------- learned weights

// ConsSample is one graded frozen verdict for fitting: each present
// source's probabilities and the outcome (0 home, 1 draw, 2 away).
type ConsSample struct {
	Probs   map[string]P3
	Outcome int
	Kickoff time.Time
}

// ConsFit is a fit's result.
type ConsFit struct {
	Weights        map[string]float64 `json:"weights"`
	InUse          string             `json:"in_use"`
	N              int                `json:"n"`
	HoldoutN       int                `json:"holdout_n"`
	LoglossDefault *float64           `json:"logloss_default"`
	LoglossLearned *float64           `json:"logloss_learned"`
	Sources        []string           `json:"sources"`
	Note           string             `json:"note"`
}

// MinFitMatches is the minimum number of graded matches to learn weights.
const MinFitMatches = 200

func blendLogLoss(samples []ConsSample, keys []string, w []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	tot := 0.0
	for _, smp := range samples {
		p := 0.0
		ws := 0.0
		for i, k := range keys {
			p += w[i] * smp.Probs[k][smp.Outcome]
			ws += w[i]
		}
		if ws > 0 {
			p /= ws
		}
		tot += -math.Log(math.Max(p, 1e-9))
	}
	return tot / float64(len(samples))
}

// fitSimplex minimises mean log loss + ridge·Σ(w − w0)² over the simplex
// (w ≥ 0, Σw = 1) by exponentiated gradient descent.
func fitSimplex(samples []ConsSample, keys []string, w0 []float64, ridge float64) []float64 {
	n := len(keys)
	w := append([]float64(nil), w0...)
	for i := range w {
		if w[i] <= 0 {
			w[i] = 1e-3
		}
	}
	normalizeVec(w)
	eta := 0.5
	for it := 0; it < 1500; it++ {
		g := make([]float64, n)
		for _, smp := range samples {
			p := 0.0
			for i, k := range keys {
				p += w[i] * smp.Probs[k][smp.Outcome]
			}
			p = math.Max(p, 1e-9)
			for i, k := range keys {
				g[i] -= smp.Probs[k][smp.Outcome] / p
			}
		}
		for i := range g {
			g[i] = g[i]/float64(len(samples)) + 2*ridge*(w[i]-w0[i])
		}
		for i := range w {
			w[i] *= math.Exp(-eta * g[i])
		}
		normalizeVec(w)
	}
	return w
}

func normalizeVec(w []float64) {
	s := 0.0
	for _, v := range w {
		s += v
	}
	if s <= 0 {
		return
	}
	for i := range w {
		w[i] /= s
	}
}

// FitConsensus learns a group's weights: the sources present in at least
// half the samples are fitted on the samples where all of them are present
// (oldest 80 %), and kept only when they beat the defaults on the newest
// 20 % (holdout); other sources keep their default share.
func FitConsensus(samples []ConsSample, defaults map[string]float64) ConsFit {
	fit := ConsFit{Weights: copyW(defaults), InUse: "default", N: len(samples), Sources: []string{}}
	cnt := map[string]int{}
	for _, smp := range samples {
		for k := range smp.Probs {
			cnt[k]++
		}
	}
	keys := []string{}
	for _, k := range ConsKeys {
		if cnt[k]*2 >= len(samples) && cnt[k] > 0 && defaults[k] > 0 {
			keys = append(keys, k)
		}
	}
	// Sharp and market overlap: when the sharp book is fitted the market
	// keeps its own weight; when it isn't, the market stands in for it.
	use := []ConsSample{}
	for _, smp := range samples {
		ok := true
		for _, k := range keys {
			if _, have := smp.Probs[k]; !have {
				ok = false
			}
		}
		if ok {
			use = append(use, smp)
		}
	}
	fit.N, fit.Sources = len(use), keys
	if len(keys) < 2 {
		fit.Note = "Kept the defaults: fewer than two sources are present in most graded matches."
		return fit
	}
	if len(use) < MinFitMatches {
		fit.Note = fmt.Sprintf("Kept the defaults: only %d graded matches with %s (%d needed).", len(use), strings.Join(keys, ", "), MinFitMatches)
		return fit
	}
	sort.SliceStable(use, func(i, j int) bool { return use[i].Kickoff.Before(use[j].Kickoff) })
	cut := len(use) * 8 / 10
	train, hold := use[:cut], use[cut:]
	w0 := make([]float64, len(keys))
	for i, k := range keys {
		w0[i] = defaults[k]
	}
	if _, ok := cnt["sharp"]; !ok || !containsS(keys, "sharp") {
		for i, k := range keys {
			if k == "market" {
				w0[i] += defaults["sharp"]
			}
		}
	}
	normalizeVec(w0)
	w := fitSimplex(train, keys, w0, 0.01)
	ld, ll := blendLogLoss(hold, keys, w0), blendLogLoss(hold, keys, w)
	fit.HoldoutN = len(hold)
	fit.LoglossDefault, fit.LoglossLearned = ptr(math.Round(ld*1e5)/1e5), ptr(math.Round(ll*1e5)/1e5)
	if ll >= ld {
		fit.Note = fmt.Sprintf("Kept the defaults: the learned weights did not beat them on the newest %d matches (log loss %.4f v %.4f).", len(hold), ll, ld)
		return fit
	}
	// Learned shares for the fitted sources, defaults for the rest.
	rest := 0.0
	for k, v := range defaults {
		if !containsS(keys, k) && !(k == "sharp" && !containsS(keys, "sharp") && containsS(keys, "market")) {
			rest += v
		}
	}
	out := map[string]float64{}
	for k, v := range defaults {
		out[k] = v
	}
	if !containsS(keys, "sharp") && containsS(keys, "market") {
		out["sharp"] = 0 // the market's learned share already includes the sharp book's
	}
	for i, k := range keys {
		out[k] = math.Round(w[i]*(1-rest)*1e4) / 1e4
	}
	fit.Weights, fit.InUse = out, "learned"
	fit.Note = fmt.Sprintf("Learned on %d graded matches; better than the defaults on the newest %d (log loss %.4f v %.4f).", len(train), len(hold), ll, ld)
	return fit
}

func copyW(m map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func containsS(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// consSamples loads a group's graded frozen verdicts.
func (s *Service) consSamples(group string) []ConsSample {
	var rows []VerdictRow
	s.DB.Where("grp = ? AND status IN ('correct','wrong')", group).Order("kickoff").Find(&rows)
	out := []ConsSample{}
	for _, r := range rows {
		o := map[string]int{"1": 0, "X": 1, "2": 2}[r.Outcome]
		if r.Outcome == "" {
			continue
		}
		smp := ConsSample{Probs: map[string]P3{}, Outcome: o, Kickoff: r.Kickoff}
		for _, src := range r.Verdict.Consensus.Sources {
			smp.Probs[src.Key] = P3{src.Probs["1"], src.Probs["X"], src.Probs["2"]}
		}
		out = append(out, smp)
	}
	return out
}

// FitConsensusWeights fits every group now and stores the results.
func (s *Service) FitConsensusWeights() []ConsensusWeightsRow {
	now := s.Now()
	out := []ConsensusWeightsRow{}
	for _, g := range consGroups {
		fit := FitConsensus(s.consSamples(g), DefaultConsWeights)
		row := ConsensusWeightsRow{Grp: g, Weights: fit.Weights, InUse: fit.InUse, N: fit.N, HoldoutN: fit.HoldoutN, LoglossDefault: fit.LoglossDefault,
			LoglossLearned: fit.LoglossLearned, Sources: fit.Sources, Note: fit.Note, FittedAt: now}
		s.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row)
		out = append(out, row)
	}
	s.DB.Model(&WinSettings{}).Where("id = 1").Update("last_fit_at", now)
	resetLearnedCache()
	return out
}

// FitConsensusDue refits weekly (scheduler pass).
func (s *Service) FitConsensusDue() bool {
	st := s.WinSettingsGet()
	if st.LastFitAt != nil && s.Now().Sub(*st.LastFitAt) < 7*24*time.Hour {
		return false
	}
	s.FitConsensusWeights()
	return true
}

// ConsensusWeightsView is GET /model/consensus-weights.
func (s *Service) ConsensusWeightsView() map[string]any {
	var rows []ConsensusWeightsRow
	s.DB.Find(&rows)
	by := map[string]ConsensusWeightsRow{}
	for _, r := range rows {
		by[r.Grp] = r
	}
	groups := []map[string]any{}
	for _, g := range consGroups {
		r, ok := by[g]
		item := map[string]any{"group": g, "label": groupLabels[g], "weights": DefaultConsWeights, "in_use": "default", "n": 0, "holdout_n": 0,
			"logloss_default": nil, "logloss_learned": nil, "sources": []string{}, "fitted_at": nil, "note": "Not fitted yet: the defaults are used."}
		if ok {
			item["weights"], item["in_use"], item["n"], item["holdout_n"] = r.Weights, r.InUse, r.N, r.HoldoutN
			item["logloss_default"], item["logloss_learned"], item["sources"], item["note"] = r.LoglossDefault, r.LoglossLearned, r.Sources, r.Note
			t := r.FittedAt
			item["fitted_at"] = &t
			if r.Sources == nil {
				item["sources"] = []string{}
			}
		}
		groups = append(groups, item)
	}
	return map[string]any{"groups": groups, "defaults": DefaultConsWeights,
		"note": fmt.Sprintf("Weights are learned weekly per group from DASYIN's graded frozen verdicts (at least %d matches with every fitted source), "+
			"pulled toward the defaults, and used only when they beat the defaults on the newest 20%% of matches. Market prices are never corrected.", MinFitMatches)}
}
