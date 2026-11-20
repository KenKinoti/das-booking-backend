package football

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football/model"
)

// PriceOut is one bookmaker's price.
type PriceOut struct {
	Bookmaker string     `json:"bookmaker"`
	Title     string     `json:"title"`
	Source    string     `json:"source"` // apifootball | oddsapi | manual: <bookmaker>
	Price     float64    `json:"price"`
	UpdatedAt *time.Time `json:"updated_at"`
	FetchedAt time.Time  `json:"fetched_at"`
}

// SelectionOut is a selection with every price and the market's view.
type SelectionOut struct {
	Market     string     `json:"market"`
	Line       *float64   `json:"line"`
	Selection  string     `json:"selection"`
	Label      string     `json:"label"`
	Prices     []PriceOut `json:"prices"`
	Best       *PriceOut  `json:"best"`
	ConsensusP *float64   `json:"consensus_p"` // median de-margined probability across bookmakers
	SharpP     *float64   `json:"sharp_p"`     // Pinnacle (or exchange) de-margined
	FairP      *float64   `json:"fair_p"`      // sharp when available, else consensus
	FairOdds   *float64   `json:"fair_odds"`
	FairSource string     `json:"fair_source"`
	Books      int        `json:"books"`
}

// BookOut summarises a bookmaker.
type BookOut struct {
	Key       string     `json:"key"`
	Title     string     `json:"title"`
	Source    string     `json:"source"`
	Margin1X2 *float64   `json:"margin_1x2"`
	UpdatedAt *time.Time `json:"updated_at"`
	FetchedAt time.Time  `json:"fetched_at"`
	Stale     bool       `json:"stale"`
	Sharp     bool       `json:"sharp"`
}

// MovePoint is a price at a time.
type MovePoint struct {
	At    time.Time `json:"at"`
	Price float64   `json:"price"`
}

// Movement is the line history of one selection at one bookmaker.
type Movement struct {
	Bookmaker string      `json:"bookmaker"`
	Source    string      `json:"source"`
	Market    string      `json:"market"`
	Line      *float64    `json:"line"`
	Selection string      `json:"selection"`
	Points    []MovePoint `json:"points"`
	Open      float64     `json:"open"`
	Last      float64     `json:"last"`
	Change    float64     `json:"change_pct"` // last / open − 1
}

// MarketView is everything about the odds of a fixture.
type MarketView struct {
	AsOf         *time.Time         `json:"as_of"`
	Books        int                `json:"books"`
	Bookmakers   []BookOut          `json:"bookmakers"`
	Selections   []SelectionOut     `json:"selections"`
	Consensus1X2 map[string]float64 `json:"consensus_1x2"`
	Sharp1X2     map[string]float64 `json:"sharp_1x2"`
	Fair1X2      map[string]float64 `json:"fair_1x2"`
	FairSource   string             `json:"fair_source"`
	DeVig        string             `json:"devig_method"`
	Movement     []Movement         `json:"movement"`
	Closing      bool               `json:"closing"` // prices are the last before kickoff
	Manual       []ManualOdd        `json:"manual"`
	Live         []PriceOut         `json:"live_prices,omitempty"`
	Note         string             `json:"note,omitempty"`
}

// marketView builds the market of a fixture from stored prices (before:
// only prices fetched before that time — the closing line; org: include the
// prices that organisation typed in by hand).
//
// Only the latest price of each line is loaded unless history is set (the
// match page's line movement).
func (s *Service) marketView(f Fixture, before *time.Time, org string, history ...bool) MarketView {
	mv := MarketView{Bookmakers: []BookOut{}, Selections: []SelectionOut{}, Movement: []Movement{}, Manual: []ManualOdd{},
		DeVig: "power method for 1X2, proportional for two-way markets", Closing: before != nil}
	var rows []Odd
	full := len(history) > 0 && history[0]
	if full {
		q := s.DB.Where("fixture_id = ?", f.ID)
		if before != nil {
			q = q.Where("fetched_at < ?", *before)
		}
		q.Order("fetched_at, id").Find(&rows)
	} else {
		cut := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		if before != nil {
			cut = *before
		}
		s.DB.Raw(`SELECT DISTINCT ON (source, bookmaker, market, selection, COALESCE(line, -999), live) * FROM fb_odds
			WHERE fixture_id = ? AND fetched_at < ? ORDER BY source, bookmaker, market, selection, COALESCE(line, -999), live, fetched_at DESC, id DESC`, f.ID, cut).Scan(&rows)
	}
	pre := []Odd{}
	for _, r := range rows {
		if r.Live {
			mv.Live = append(mv.Live, PriceOut{Bookmaker: r.Bookmaker, Title: r.BookmakerTitle, Source: r.Source, Price: r.Price, UpdatedAt: r.ProviderUpdatedAt, FetchedAt: r.FetchedAt})
			continue
		}
		pre = append(pre, r)
	}
	latest := latestOdds(pre, before)
	if org != "" {
		var man []ManualOdd
		s.DB.Where("organization_id = ? AND fixture_id = ?", org, f.ID).Order("seen_at").Find(&man)
		mv.Manual = man
		for _, m := range man {
			if before != nil && !m.SeenAt.Before(*before) {
				continue
			}
			book := "manual:" + bookmakerSlug(m.Bookmaker)
			if latest[book] == nil {
				latest[book] = map[selKey]Odd{}
			}
			k := selKey{m.Market, lineKey(m.Line), m.Selection}
			o := Odd{FixtureID: f.ID, Source: "manual: " + m.Bookmaker, Bookmaker: bookmakerSlug(m.Bookmaker), BookmakerTitle: m.Bookmaker,
				Market: m.Market, Selection: m.Selection, Line: m.Line, Price: m.Price, FetchedAt: m.SeenAt, CheckedAt: m.SeenAt}
			if cur, ok := latest[book][k]; !ok || !m.SeenAt.Before(cur.FetchedAt) {
				latest[book][k] = o
			}
		}
	}
	now := s.Now()
	// Per bookmaker: groups and their de-margined probabilities.
	type fairRow struct {
		book  string
		sharp bool
		probs map[string]float64
	}
	fair := map[groupKey][]fairRow{}
	books := []string{}
	for b := range latest {
		books = append(books, b)
	}
	sort.Strings(books)
	isSharp := func(slug string) bool {
		for _, x := range SharpBooks {
			if x == slug {
				return true
			}
		}
		return false
	}
	for _, b := range books {
		sels := latest[b]
		var any Odd
		groups := map[groupKey]map[string]float64{}
		var upd *time.Time
		var fetched time.Time
		for k, o := range sels {
			any = o
			g := fairGroupKey(k)
			if groups[g] == nil {
				groups[g] = map[string]float64{}
			}
			groups[g][k.Sel] = o.Price
			if o.ProviderUpdatedAt != nil && (upd == nil || o.ProviderUpdatedAt.After(*upd)) {
				upd = o.ProviderUpdatedAt
			}
			last := o.CheckedAt
			if last.IsZero() {
				last = o.FetchedAt
			}
			if last.After(fetched) {
				fetched = last
			}
		}
		manual := strings.HasPrefix(b, "manual:")
		bo := BookOut{Key: any.Bookmaker, Title: any.BookmakerTitle, Source: any.Source, UpdatedAt: upd, FetchedAt: fetched,
			Sharp: !manual && isSharp(any.Bookmaker), Stale: before == nil && now.Sub(fetched) > 6*time.Hour && f.Kickoff.After(now)}
		for g, prices := range groups {
			p, margin, ok := fairGroup(g.Market, prices)
			if !ok {
				continue
			}
			if g.Market == Mkt1X2 {
				m := margin
				bo.Margin1X2 = &m
			}
			if manual {
				continue // hand-typed prices are compared, not used for the market's view
			}
			fair[g] = append(fair[g], fairRow{book: b, sharp: isSharp(any.Bookmaker), probs: p})
		}
		mv.Bookmakers = append(mv.Bookmakers, bo)
		if mv.AsOf == nil || fetched.After(*mv.AsOf) {
			t := fetched
			mv.AsOf = &t
		}
	}
	mv.Books = len(mv.Bookmakers)
	sort.Slice(mv.Bookmakers, func(i, j int) bool {
		if mv.Bookmakers[i].Sharp != mv.Bookmakers[j].Sharp {
			return mv.Bookmakers[i].Sharp
		}
		return mv.Bookmakers[i].Title < mv.Bookmakers[j].Title
	})
	// Consensus and sharp per group.
	type gv struct {
		cons   map[string]float64
		sharp  map[string]float64
		sharpB string
		n      int
	}
	gviews := map[groupKey]gv{}
	for g, rs := range fair {
		v := gv{cons: map[string]float64{}, n: len(rs)}
		for _, sel := range MarketSelections[g.Market] {
			xs := []float64{}
			for _, r := range rs {
				xs = append(xs, r.probs[sel])
			}
			v.cons[sel] = median(xs)
		}
		normalize(v.cons)
		for _, pref := range SharpBooks {
			for _, r := range rs {
				if r.sharp && strings.HasSuffix(r.book, ":"+pref) {
					v.sharp, v.sharpB = r.probs, pref
					break
				}
			}
			if v.sharp != nil {
				break
			}
		}
		gviews[g] = v
	}
	if v, ok := gviews[groupKey{Mkt1X2, ""}]; ok {
		mv.Consensus1X2 = v.cons
		mv.Fair1X2, mv.FairSource = v.cons, consensusLabel(v.n)
		if v.sharp != nil {
			mv.Sharp1X2, mv.Fair1X2, mv.FairSource = v.sharp, v.sharp, sharpTitle(v.sharpB)+" (de-margined)"
		}
	}
	// Derived double chance / draw no bet from the 1X2 view.
	derive := func(m, sel string, p map[string]float64) (float64, bool) {
		if p == nil {
			return 0, false
		}
		h, d, a := p[SelHome], p[SelDraw], p[SelAway]
		switch m + ":" + sel {
		case "dc:1x":
			return h + d, true
		case "dc:12":
			return h + a, true
		case "dc:x2":
			return d + a, true
		case "dnb:home":
			if h+a > 0 {
				return h / (h + a), true
			}
		case "dnb:away":
			if h+a > 0 {
				return a / (h + a), true
			}
		}
		return 0, false
	}
	// Selections: every priced selection, prices best first.
	all := map[selKey][]PriceOut{}
	lines := map[selKey]*float64{}
	for _, b := range books {
		for k, o := range latest[b] {
			all[k] = append(all[k], PriceOut{Bookmaker: o.Bookmaker, Title: o.BookmakerTitle, Source: o.Source, Price: o.Price, UpdatedAt: o.ProviderUpdatedAt, FetchedAt: o.FetchedAt})
			lines[k] = o.Line
		}
	}
	h, a := s.teamName(f.HomeID), s.teamName(f.AwayID)
	for k, ps := range all {
		sort.Slice(ps, func(i, j int) bool { return ps[i].Price > ps[j].Price })
		so := SelectionOut{Market: k.Market, Line: lines[k], Selection: k.Sel, Label: SelectionLabel(k.Market, k.Sel, lines[k], h, a), Prices: ps, Books: len(ps)}
		// Best price: the highest, including the organisation's own
		// hand-typed prices (labelled "manual: <bookmaker>").
		if len(ps) > 0 {
			b := ps[0]
			so.Best = &b
		}
		g := fairGroupKey(k)
		if v, ok := gviews[g]; ok {
			so.ConsensusP = ptr(v.cons[k.Sel])
			if v.sharp != nil {
				so.SharpP = ptr(v.sharp[k.Sel])
				so.FairP, so.FairSource = so.SharpP, sharpTitle(v.sharpB)+" (de-margined)"
			} else {
				so.FairP, so.FairSource = so.ConsensusP, consensusLabel(v.n)
			}
		} else if k.Market == MktDC || k.Market == MktDNB {
			if p, ok := derive(k.Market, k.Sel, mv.Consensus1X2); ok {
				so.ConsensusP = ptr(p)
			}
			if p, ok := derive(k.Market, k.Sel, mv.Sharp1X2); ok {
				so.SharpP = ptr(p)
			}
			if p, ok := derive(k.Market, k.Sel, mv.Fair1X2); ok {
				so.FairP, so.FairSource = ptr(p), "derived from "+mv.FairSource+" 1X2"
			}
		}
		if so.FairP != nil && *so.FairP > 0 {
			so.FairOdds = ptr(math.Round(1 / *so.FairP * 1000) / 1000)
		}
		mv.Selections = append(mv.Selections, so)
	}
	order := map[string]int{Mkt1X2: 0, MktDC: 1, MktDNB: 2, MktOU: 3, MktBTTS: 4, MktAH: 5}
	selOrder := map[string]int{SelHome: 0, SelDraw: 1, SelAway: 2, Sel1X: 0, Sel12: 1, SelX2: 2, SelOver: 0, SelUnder: 1, SelYes: 0, SelNo: 1}
	sort.Slice(mv.Selections, func(i, j int) bool {
		x, y := mv.Selections[i], mv.Selections[j]
		if order[x.Market] != order[y.Market] {
			return order[x.Market] < order[y.Market]
		}
		lx, ly := 0.0, 0.0
		if x.Line != nil {
			lx = *x.Line
		}
		if y.Line != nil {
			ly = *y.Line
		}
		if lx != ly {
			return lx < ly
		}
		return selOrder[x.Selection] < selOrder[y.Selection]
	})
	// Line movement: 1X2 and over/under 2.5 at the sharp book (or the
	// bookmaker with the most history).
	if full {
		mv.Movement = movements(pre, before)
	}
	if len(mv.Bookmakers) == 0 {
		mv.Note = "No prices stored for this match yet."
	}
	return mv
}

func consensusLabel(n int) string {
	if n == 1 {
		return "1 bookmaker (de-margined)"
	}
	return "consensus of " + itoa(n) + " bookmakers (median, de-margined)"
}

func normalize(m map[string]float64) {
	t := 0.0
	for _, v := range m {
		t += v
	}
	if t <= 0 {
		return
	}
	for k := range m {
		m[k] /= t
	}
}

func movements(rows []Odd, before *time.Time) []Movement {
	type mk struct {
		src, book, market, line, sel string
	}
	series := map[mk][]Odd{}
	for _, r := range rows {
		if before != nil && !r.FetchedAt.Before(*before) {
			continue
		}
		l := lineKey(r.Line)
		if !(r.Market == Mkt1X2 || (r.Market == MktOU && l == "2.5")) {
			continue
		}
		k := mk{r.Source, r.Bookmaker, r.Market, l, r.Selection}
		series[k] = append(series[k], r)
	}
	// Pick the book: sharp first, else the longest history.
	bestBook, bestLen := "", -1
	counts := map[string]int{}
	for k, v := range series {
		counts[k.src+":"+k.book] += len(v)
	}
	for _, sb := range SharpBooks {
		for b, n := range counts {
			if strings.HasSuffix(b, ":"+sb) && (n > bestLen || n == bestLen && b < bestBook) {
				bestBook, bestLen = b, n
			}
		}
		if bestBook != "" {
			break
		}
	}
	if bestBook == "" {
		for b, n := range counts {
			if n > bestLen || (n == bestLen && b < bestBook) {
				bestBook, bestLen = b, n
			}
		}
	}
	out := []Movement{}
	for k, v := range series {
		if k.src+":"+k.book != bestBook {
			continue
		}
		m := Movement{Bookmaker: k.book, Source: k.src, Market: k.market, Selection: k.sel, Points: []MovePoint{}}
		if len(v) > 0 {
			m.Line = v[0].Line
		}
		for _, r := range v {
			m.Points = append(m.Points, MovePoint{At: r.FetchedAt, Price: r.Price})
		}
		// The last price was still current when last checked.
		last := v[len(v)-1]
		if last.CheckedAt.After(last.FetchedAt) && (before == nil || last.CheckedAt.Before(*before)) {
			m.Points = append(m.Points, MovePoint{At: last.CheckedAt, Price: last.Price})
		}
		m.Open, m.Last = v[0].Price, last.Price
		if m.Open > 0 {
			m.Change = math.Round((m.Last/m.Open-1)*10000) / 10000
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Market != out[j].Market {
			return out[i].Market < out[j].Market
		}
		return out[i].Selection < out[j].Selection
	})
	return out
}

// ---------------------------------------------------------------- value

// ValueOut is the model's view of one selection against the market.
type ValueOut struct {
	Market       string   `json:"market"`
	Line         *float64 `json:"line"`
	Selection    string   `json:"selection"`
	Label        string   `json:"label"`
	ModelP       float64  `json:"model_p"`    // probability of winning (pushes removed)
	ModelPush    float64  `json:"model_push"` // probability of the stake coming back (half or full)
	FairOdds     float64  `json:"model_fair_odds"`
	MarketP      *float64 `json:"market_p"`
	MarketSource string   `json:"market_source"`
	Edge         *float64 `json:"edge"` // model p − market p
	BestPrice    *float64 `json:"best_price"`
	Bookmaker    string   `json:"bookmaker"`
	BookSource   string   `json:"bookmaker_source"`
	EV           *float64 `json:"ev"`             // expected profit per 1 staked at the best price
	Kelly        float64  `json:"kelly_fraction"` // fractional Kelly, capped
	KellyStake   *float64 `json:"kelly_stake"`
	Currency     string   `json:"currency,omitempty"`
	Value        bool     `json:"value"` // edge ≥ minimum and EV > 0
	Basis        string   `json:"basis"` // blend | dixon-coles
}

// modelWL returns the model's win / loss / push probabilities for a selection.
func modelWL(p Prediction, m, sel string, line *float64) (W, L, V float64, basis string, ok bool) {
	H, D, A := p.PHome, p.PDraw, p.PAway
	switch m {
	case Mkt1X2:
		switch sel {
		case SelHome:
			return H, D + A, 0, "blend", true
		case SelDraw:
			return D, H + A, 0, "blend", true
		case SelAway:
			return A, H + D, 0, "blend", true
		}
	case MktDC:
		switch sel {
		case Sel1X:
			return H + D, A, 0, "blend", true
		case Sel12:
			return H + A, D, 0, "blend", true
		case SelX2:
			return D + A, H, 0, "blend", true
		}
	case MktDNB:
		if sel == SelHome {
			return H, A, D, "blend", true
		}
		return A, H, D, "blend", true
	}
	mat := p.Detail.Full
	if mat == nil {
		mat = p.Detail.Matrix
	}
	if mat == nil {
		return 0, 0, 0, "", false
	}
	W, L, V = SelectionProb(mat, m, sel, line)
	return W, L, V, "dixon-coles", W+L > 0
}

// valueRows compares the model with the market for every priced selection.
func valueRows(p Prediction, mv MarketView, minEdge, fraction, capF, bankroll float64, currency string) []ValueOut {
	out := []ValueOut{}
	for _, so := range mv.Selections {
		W, L, V, basis, ok := modelWL(p, so.Market, so.Selection, so.Line)
		if !ok {
			continue
		}
		pe := EffP(W, L)
		v := ValueOut{Market: so.Market, Line: so.Line, Selection: so.Selection, Label: so.Label, ModelP: round4(pe), ModelPush: round4(V),
			MarketP: so.FairP, MarketSource: so.FairSource, Basis: basis}
		if pe > 0 {
			v.FairOdds = math.Round(1/pe*1000) / 1000
		}
		if so.FairP != nil {
			e := round4(pe - *so.FairP)
			v.Edge = &e
		}
		if so.Best != nil {
			b := so.Best.Price
			v.BestPrice, v.Bookmaker, v.BookSource = &b, so.Best.Title, so.Best.Source
			ev := round4(EVOf(W, L, b))
			v.EV = &ev
			v.Kelly = round4(model.Kelly(pe, b, fraction, capF))
			if bankroll > 0 {
				st := math.Floor(v.Kelly*bankroll*100) / 100
				v.KellyStake, v.Currency = &st, currency
			}
			v.Value = v.Edge != nil && *v.Edge >= minEdge && ev > 0
			if !v.Value {
				v.Kelly, v.KellyStake = 0, nil
			}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ei, ej := -1.0, -1.0
		if out[i].EV != nil {
			ei = *out[i].EV
		}
		if out[j].EV != nil {
			ej = *out[j].EV
		}
		return ei > ej
	})
	return out
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

func itoa(n int) string { return strconv.Itoa(n) }

// fairGroupKey is the group a selection is de-margined in. Asian handicap
// sides carry their own perspective's line (home −0.5 is the other side of
// away +0.5), so the away side's line is mirrored to the home perspective;
// otherwise no handicap line other than 0 would ever get a fair price.
func fairGroupKey(k selKey) groupKey {
	if k.Market == MktAH && k.Sel == SelAway && k.Line != "" {
		if v, err := strconv.ParseFloat(k.Line, 64); err == nil {
			m := -v + 0 // + 0 turns −0 into 0
			return groupKey{k.Market, lineKey(&m)}
		}
	}
	return groupKey{k.Market, k.Line}
}
