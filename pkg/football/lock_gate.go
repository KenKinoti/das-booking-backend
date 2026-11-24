package football

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football/model"
)

// The data gate: a pick needs at least one solid source — a market price
// (a sharp book, or two or more bookmakers, or a Kenyan / OddsPapi price, or
// a price typed in by hand, or a liquid Polymarket market), ClubElo for both
// teams, API-Football's own prediction (when switched on), a fitted
// Dixon–Coles with both teams having ≥ 8 matches in the competition, or Elo
// with both teams having ≥ 15 rated matches. Otherwise: no pick, with a plain
// reason. A pick without a market price and without ClubElo is "model only"
// (never above Good).

// modelDataFrom says how much an Elo replay and a competition's Dixon–Coles
// fit know about two teams.
func modelDataFrom(e *model.Elo, dc dcEntry, home, away int64, hn, an string) ModelData {
	h, a := tid(home), tid(away)
	md := ModelData{EloHome: e.Games(h), EloAway: e.Games(a), DCFitted: dc.p != nil, DCMatches: dc.n}
	if dc.teams != nil {
		md.DCHome, md.DCAway = dc.teams[h], dc.teams[a]
	}
	inFit := false
	if dc.p != nil {
		_, okH := dc.p.Attack[h]
		_, okA := dc.p.Attack[a]
		inFit = okH && okA
	}
	md.DCOK = inFit && md.DCHome >= MinDCTeamMatches && md.DCAway >= MinDCTeamMatches
	md.EloOK = md.EloHome >= MinEloMatches && md.EloAway >= MinEloMatches
	md.Solid = md.DCOK || md.EloOK
	md.HasData = inFit || (md.EloHome >= MinEloShow && md.EloAway >= MinEloShow)
	if !md.Solid {
		parts := []string{fmt.Sprintf("%s has %s and %s %s (%d each needed)", hn, countWord(md.EloHome, "stored result"), an, countWord(md.EloAway, "stored result"), MinEloMatches)}
		switch {
		case dc.p == nil && dc.err != nil:
			parts = append(parts, "Dixon–Coles: "+dc.err.Error())
		case dc.p == nil:
			parts = append(parts, "Dixon–Coles: not fitted")
		default:
			parts = append(parts, fmt.Sprintf("Dixon–Coles: %d and %d matches in this competition (%d each needed)", md.DCHome, md.DCAway, MinDCTeamMatches))
		}
		md.Why = "Too little match history: " + strings.Join(parts, "; ") + "."
	}
	return md
}

func countWord(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	return fmt.Sprintf("%d %ss", n, w)
}

// crossCompElo are the Elo options of the replay over every stored result.
// The tuned NewTeamOffset (−50: a promoted team starts below its league's
// mean) assumes one league; replayed over hundreds of teams of many
// competitions first seen at different times, each newcomer dragged the mean
// down (≈ 50 · ln(N/4) points: ~1280 after a few hundred teams), so a top
// club first seen late showed 1280 while an unseen team showed 1500.
// v2.19.1: every team starts at 1500 (Elo stays zero-sum around it).
func crossCompElo() model.EloOptions {
	o := model.DefaultEloOptions()
	o.NewTeamOffset = 0
	return o
}

// modelDataNow computes a fixture's model data from the current Elo replay
// and the competition's fit (cached).
func (s *Service) modelDataNow(f Fixture) ModelData {
	return modelDataFrom(s.eloNow(), s.dcFor(f.CompetitionID, false), f.HomeID, f.AwayID, s.teamName(f.HomeID), s.teamName(f.AwayID))
}

// modelDataFor is the model data stored with a prediction (v2.19.1), else
// computed now (older predictions, seeded worlds).
func (s *Service) modelDataFor(f Fixture, pred *Prediction) ModelData {
	if pred != nil && pred.Detail.Data != nil {
		return *pred.Detail.Data
	}
	return s.modelDataNow(f)
}

// marketEvidence counts the API bookmakers with a complete 1X2 price and
// whether one of them is Kenyan or came through OddsPapi.
func marketEvidence(mv MarketView) (books int, keOrOP bool) {
	type tri struct {
		n      int
		source string
		book   string
	}
	seen := map[string]*tri{}
	for _, so := range mv.Selections {
		if so.Market != Mkt1X2 || (so.Selection != SelHome && so.Selection != SelDraw && so.Selection != SelAway) {
			continue
		}
		for _, pr := range so.Prices {
			if strings.HasPrefix(pr.Source, "manual") || pr.Price <= 1 {
				continue
			}
			k := pr.Source + ":" + pr.Bookmaker
			if seen[k] == nil {
				seen[k] = &tri{source: pr.Source, book: pr.Bookmaker}
			}
			seen[k].n++
		}
	}
	ke := map[string]bool{}
	for _, k := range KenyanBookmakers {
		ke[bookmakerSlug(k)] = true
	}
	for _, t := range seen {
		if t.n < 3 {
			continue
		}
		books++
		if t.source == ProviderOddsPapi || ke[bookmakerSlug(t.book)] || KenyanOP[strings.ToLower(t.book)] {
			keOrOP = true
		}
	}
	return books, keOrOP
}

// typedMarket uses prices typed in by hand (any organisation's Price check,
// e.g. a Betika price) as the market when no bookmaker price was fetched:
// the latest complete 1X2 per bookmaker, de-margined; implausible margins
// (a typo) are ignored.
func (s *Service) typedMarket(f Fixture, before *time.Time, in *consInputs) {
	if in.P["sharp"] != nil || in.P["market"] != nil {
		return
	}
	q := s.DB.Where("fixture_id = ? AND market = ?", f.ID, Mkt1X2)
	if before != nil {
		q = q.Where("seen_at < ?", *before)
	}
	var man []ManualOdd
	q.Order("seen_at, created_at").Find(&man)
	if len(man) == 0 {
		return
	}
	type tri struct {
		o     [3]float64
		title string
		at    time.Time
	}
	books := map[string]*tri{}
	for _, m := range man {
		idx, ok := map[string]int{SelHome: 0, SelDraw: 1, SelAway: 2}[m.Selection]
		if !ok || m.Price <= 1 {
			continue
		}
		k := bookmakerSlug(m.Bookmaker)
		t := books[k]
		if t == nil {
			t = &tri{title: m.Bookmaker}
			books[k] = t
		}
		t.o[idx] = m.Price
		if m.SeenAt.After(t.at) {
			t.at = m.SeenAt
		}
	}
	keys := make([]string, 0, len(books))
	for k := range books {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var hs, ds, as []float64
	titles := []string{}
	var latest time.Time
	for _, k := range keys {
		t := books[k]
		if t.o[0] <= 1 || t.o[1] <= 1 || t.o[2] <= 1 {
			continue
		}
		ps, margin := model.DeVig(t.o[:], model.DeVigProportional)
		if ps == nil || margin < -0.02 || margin > 0.30 {
			continue
		}
		hs, ds, as = append(hs, ps[0]), append(ds, ps[1]), append(as, ps[2])
		titles = append(titles, t.title)
		if t.at.After(latest) {
			latest = t.at
		}
	}
	if len(hs) == 0 {
		return
	}
	p := P3{median(hs), median(ds), median(as)}
	sum := p[0] + p[1] + p[2]
	for i := range p {
		p[i] /= sum
	}
	in.P["market"] = &p
	in.Label["market"] = "Typed-in price (" + strings.Join(titles, ", ") + ", de-margined)"
	in.Detail["market"] = fmt.Sprintf("typed in by someone using DASYIN (Price check), latest %s UTC", latest.Format("2006-01-02 15:04"))
	in.AsOf["market"] = &latest
	in.Typed = true
	delete(in.Why, "market")
	if in.Fav == nil {
		in.Fav = &p
	}
}

// gateFor applies the data gate to a fixture's consensus inputs.
func gateFor(in consInputs) GateOut {
	g := GateOut{Solid: []string{}, Checks: []GateCheck{}}
	add := func(key, label string, ok bool, why string) {
		g.Checks = append(g.Checks, GateCheck{Key: key, Label: label, OK: ok, Why: why})
		if ok {
			g.Solid = append(g.Solid, label)
		}
	}
	// Market price.
	mLabel, mOK, mWhy := "Market price", false, "no bookmaker prices stored"
	switch {
	case in.P["sharp"] != nil:
		mLabel, mOK = firstNonEmpty(in.Label["sharp"], "Sharp bookmaker"), true
	case in.P["market"] != nil && in.Typed:
		mLabel, mOK = firstNonEmpty(in.Label["market"], "Typed-in price"), true
	case in.P["market"] != nil && in.Books >= 2:
		mLabel, mOK = fmt.Sprintf("Prices from %d bookmakers", in.Books), true
	case in.P["market"] != nil && in.KEorOP:
		mLabel, mOK = "Kenyan bookmaker price", true
	case in.P["polymarket"] != nil:
		mLabel, mOK = "Polymarket prices", true
	case in.P["market"] != nil:
		mWhy = "only one bookmaker's price (two or more, a sharp or a Kenyan price needed)"
	}
	add("market", mLabel, mOK, mWhy)
	ceWhy := in.Why["clubelo"]
	add("clubelo", "ClubElo ratings for both teams", in.P["clubelo"] != nil, ceWhy)
	add("apifootball_pred", "API-Football's prediction", in.P["apifootball_pred"] != nil, in.Why["apifootball_pred"])
	md := in.Model
	if md == nil {
		md = &ModelData{Why: "no model prediction"}
	}
	dcWhy, eloWhy := "", ""
	if !md.DCOK {
		switch {
		case !md.DCFitted:
			dcWhy = "not fitted for this competition"
		default:
			dcWhy = fmt.Sprintf("%d and %d matches in this competition (%d each needed)", md.DCHome, md.DCAway, MinDCTeamMatches)
		}
	}
	if !md.EloOK {
		eloWhy = fmt.Sprintf("%d and %d rated matches (%d each needed)", md.EloHome, md.EloAway, MinEloMatches)
	}
	add("model_dc", "Dixon–Coles fitted on this competition", md.DCOK, dcWhy)
	add("model_elo", "Elo with enough matches for both teams", md.EloOK, eloWhy)
	g.OK = len(g.Solid) > 0
	g.ModelOnly = g.OK && !mOK && in.P["clubelo"] == nil
	if !g.OK {
		if in.P["market"] != nil {
			g.Reason = "Only one bookmaker's price and too little match history for these teams"
		} else {
			g.Reason = "No prices and too little match history for these teams"
		}
	}
	return g
}

// ModelOnlyNote is the tier note of a pick made without a market price and
// without ClubElo.
const ModelOnlyNote = "Model only — less reliable"

// capModelOnly applies the gate's cap: a model-only pick is never Strong.
func capModelOnly(v *Verdict, g GateOut) {
	if !g.ModelOnly {
		return
	}
	if tierRank(v.Tier.Key) < tierRank("good") {
		ht := History.Tier("good")
		v.Tier.Key, v.Tier.Label, v.Tier.Range = "good", ht.Label, ht.Range
		v.Tier.History = VerdictHistory{HitRate: ht.FavouriteWon, N: ht.N, DCHitRate: ht.DoubleChanceWon, PODHitRate: ht.PickOrDrawWon, DrawRate: ht.DrawRate,
			Share: ht.Share, Source: historySourceLine}
		v.Tier.Capped = true
		if v.Safer.Selection != "" {
			v.Safer.HistoryHitRate = ht.DoubleChanceWon
		}
	}
	v.Tier.Note = ModelOnlyNote
}
