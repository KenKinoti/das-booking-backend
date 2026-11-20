package football

import (
	"container/heap"
	"math"
	"sort"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/football/model"
)

// Pure jackpot mathematics (no DB, no HTTP). A jackpot ticket picks 1, X or
// 2 for each of N independent games; the chance of a ticket is the product
// of its games' probabilities. Covered games (doubles "1X", triples "1X2")
// multiply the number of combinations (and the cost) by 2 or 3.

// Outcome labels in order.
var jpLabels = [3]string{"1", "X", "2"}

// P3 is a 1X2 probability vector.
type P3 [3]float64

// DeMarginProportional turns decimal odds into probabilities by dividing
// the implied probabilities by their sum (the "proportional" / multiplicative
// method; model.DeVig). It also returns the margin (overround − 1).
func DeMarginProportional(o [3]float64) (P3, float64) {
	ps, m := model.DeVig([]float64{o[0], o[1], o[2]}, "proportional")
	if len(ps) != 3 {
		return P3{}, 0
	}
	return P3{ps[0], ps[1], ps[2]}, m
}

// BlendP3 is the linear blend of the available sources with weights
// renormalised over them (missing sources have weight 0). It returns the
// effective weights.
func BlendP3(ps []P3, w []float64) (P3, []float64) {
	var out P3
	eff := make([]float64, len(w))
	tot := 0.0
	for i := range ps {
		if i < len(w) && w[i] > 0 {
			tot += w[i]
		}
	}
	if tot <= 0 {
		return out, eff
	}
	for i := range ps {
		if i >= len(w) || w[i] <= 0 {
			continue
		}
		eff[i] = w[i] / tot
		for k := 0; k < 3; k++ {
			out[k] += eff[i] * ps[i][k]
		}
	}
	s := out[0] + out[1] + out[2]
	if s > 0 {
		for k := range out {
			out[k] /= s
		}
	}
	return out, eff
}

// Ticket is a single-pick ticket (an outcome index 0..2 per game).
type Ticket []int

// TicketProb is the probability that every pick is right.
func TicketProb(ps []P3, t Ticket) float64 {
	p := 1.0
	for i, k := range t {
		p *= ps[i][k]
	}
	return p
}

// MostLikely picks the most probable outcome of every game (ties: 1 before X before 2).
func MostLikely(ps []P3) Ticket {
	t := make(Ticket, len(ps))
	for i, p := range ps {
		best := 0
		for k := 1; k < 3; k++ {
			if p[k] > p[best]+1e-15 {
				best = k
			}
		}
		t[i] = best
	}
	return t
}

// ---------------------------------------------------------------- top-N tickets

type kbState struct {
	idx  []int // rank of the chosen outcome per game (0 = most likely)
	pos  int   // highest game index whose rank was raised
	logp float64
}

type kbHeap []kbState

func (h kbHeap) Len() int           { return len(h) }
func (h kbHeap) Less(i, j int) bool { return h[i].logp > h[j].logp }
func (h kbHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *kbHeap) Push(x any)        { *h = append(*h, x.(kbState)) }
func (h *kbHeap) Pop() any          { o := *h; n := len(o); x := o[n-1]; *h = o[:n-1]; return x }

// TopTickets returns the n most probable tickets, best first, by exact
// best-first search over the product space: each game's outcomes are sorted
// by probability; a state is a vector of ranks; children raise one rank at
// a position ≥ the parent's last raised position, so every ticket is
// generated exactly once and children are never more probable than their
// parent (Lawler-style k-best enumeration).
func TopTickets(ps []P3, n int) []Ticket {
	g := len(ps)
	if g == 0 || n <= 0 {
		return nil
	}
	order := make([][3]int, g)
	lp := make([][3]float64, g)
	for i, p := range ps {
		o := [3]int{0, 1, 2}
		sort.SliceStable(o[:], func(a, b int) bool { return p[o[a]] > p[o[b]] })
		order[i] = o
		for r := 0; r < 3; r++ {
			v := p[o[r]]
			if v <= 0 {
				lp[i][r] = math.Inf(-1)
			} else {
				lp[i][r] = math.Log(v)
			}
		}
	}
	root := kbState{idx: make([]int, g), pos: 0}
	for i := 0; i < g; i++ {
		root.logp += lp[i][0]
	}
	h := &kbHeap{root}
	out := []Ticket{}
	for h.Len() > 0 && len(out) < n {
		st := heap.Pop(h).(kbState)
		t := make(Ticket, g)
		for i := range t {
			t[i] = order[i][st.idx[i]]
		}
		out = append(out, t)
		for i := st.pos; i < g; i++ {
			if st.idx[i] >= 2 || math.IsInf(lp[i][st.idx[i]+1], -1) {
				continue
			}
			c := kbState{idx: append([]int(nil), st.idx...), pos: i}
			c.idx[i]++
			c.logp = st.logp - lp[i][st.idx[i]] + lp[i][c.idx[i]]
			heap.Push(h, c)
		}
	}
	return out
}

// ---------------------------------------------------------------- Poisson-binomial

// PoissonBinomial returns P(exactly k successes) for independent trials
// with success probabilities qs (k = 0..len(qs)), by dynamic programming.
func PoissonBinomial(qs []float64) []float64 {
	d := []float64{1}
	for _, q := range qs {
		nd := make([]float64, len(d)+1)
		for k, v := range d {
			nd[k] += v * (1 - q)
			nd[k+1] += v * q
		}
		d = nd
	}
	return d
}

// AtLeast returns P(at least k) from an exact distribution.
func AtLeast(dist []float64, k int) float64 {
	s := 0.0
	for i := k; i < len(dist); i++ {
		if i >= 0 {
			s += dist[i]
		}
	}
	return math.Min(1, s)
}

// ---------------------------------------------------------------- covering (doubles / triples)

// CoverStep is one game to double or triple next.
type CoverStep struct {
	Game         int     `json:"game"` // 1-based
	Add          string  `json:"add"`
	Picks        string  `json:"picks"`
	Kind         string  `json:"kind"`
	Combinations int     `json:"combinations"`
	PAll         float64 `json:"p_all"`
	Gain         float64 `json:"gain"` // the game's covered probability ratio after / before
}

// CoverPriority is the greedy order in which to cover games: at each step
// the addition with the largest log(probability gain) per log(combination
// growth) — a double doubles the combinations, a triple adds 50% more —
// starting from the most likely ticket. sel returns the final selection.
func CoverPriority(ps []P3, steps int) ([]CoverStep, [][]int) {
	g := len(ps)
	sel := make([][]int, g)
	ml := MostLikely(ps)
	for i := range sel {
		sel[i] = []int{ml[i]}
	}
	out := []CoverStep{}
	for s := 0; s < steps; s++ {
		bestScore, bg, bk := -1.0, -1, -1
		for i := 0; i < g; i++ {
			if len(sel[i]) >= 3 {
				continue
			}
			cur := 0.0
			in := map[int]bool{}
			for _, k := range sel[i] {
				cur += ps[i][k]
				in[k] = true
			}
			nk, np := -1, -1.0
			for k := 0; k < 3; k++ {
				if !in[k] && ps[i][k] > np {
					nk, np = k, ps[i][k]
				}
			}
			if nk < 0 || cur <= 0 {
				continue
			}
			n := float64(len(sel[i]))
			score := math.Log((cur+np)/cur) / math.Log((n+1)/n)
			if score > bestScore+1e-12 {
				bestScore, bg, bk = score, i, nk
			}
		}
		if bg < 0 {
			break
		}
		before := 0.0
		for _, k := range sel[bg] {
			before += ps[bg][k]
		}
		sel[bg] = append(sel[bg], bk)
		sort.Ints(sel[bg])
		combos, pall := 1, 1.0
		for i := range sel {
			combos *= len(sel[i])
			c := 0.0
			for _, k := range sel[i] {
				c += ps[i][k]
			}
			pall *= c
		}
		kind := "double"
		if len(sel[bg]) == 3 {
			kind = "triple"
		}
		out = append(out, CoverStep{Game: bg + 1, Add: jpLabels[bk], Picks: pickString(sel[bg]), Kind: kind, Combinations: combos, PAll: pall,
			Gain: round4((before + ps[bg][bk]) / before)})
	}
	return out, sel
}

func pickString(ks []int) string {
	c := append([]int(nil), ks...)
	sort.Ints(c)
	b := strings.Builder{}
	for _, k := range c {
		b.WriteString(jpLabels[k])
	}
	return b.String()
}

// ParsePick turns "1", "X", "2", "1X", "x2", "12", "1X2" into outcome indexes.
func ParsePick(s string) ([]int, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return nil, false
	}
	seen := map[int]bool{}
	out := []int{}
	for _, r := range s {
		k := -1
		switch r {
		case '1':
			k = 0
		case 'X', '0':
			k = 1
		case '2':
			k = 2
		case ' ', '/', ',', '-':
			continue
		default:
			return nil, false
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Ints(out)
	return out, len(out) > 0
}

// CoveredProb is the probability that a covered ticket has every game right,
// and its number of combinations.
func CoveredProb(ps []P3, picks [][]int) (float64, int) {
	p, n := 1.0, 1
	for i, ks := range picks {
		c := 0.0
		for _, k := range ks {
			c += ps[i][k]
		}
		p *= c
		n *= len(ks)
	}
	return p, n
}

// ---------------------------------------------------------------- expected value and sharing

// PublicShares are the assumed shares of tickets picking each outcome:
// ∝ p^popularity (popularity > 1: favourites are over-picked).
func PublicShares(p P3, popularity float64) P3 {
	if popularity <= 0 {
		popularity = 1
	}
	var w P3
	s := 0.0
	for k := range p {
		w[k] = math.Pow(math.Max(p[k], 1e-12), popularity)
		s += w[k]
	}
	for k := range w {
		w[k] /= s
	}
	return w
}

// ShareFactor is E[1/(1+K)] for K ~ Poisson(λ) other winners: the expected
// share of the prize for one winning ticket ((1 − e^−λ)/λ; 1 when λ = 0).
func ShareFactor(lambda float64) float64 {
	if lambda < 1e-9 {
		return 1
	}
	return (1 - math.Exp(-lambda)) / lambda
}

// EVResult is the expected value of one ticket.
type EVResult struct {
	TicketPrice   float64   `json:"ticket_price"`
	Prize         float64   `json:"prize"`
	PJackpot      float64   `json:"p_jackpot"`
	TicketsSold   float64   `json:"tickets_sold"`
	CoWinners     float64   `json:"co_winners_expected"`
	ShareFactor   float64   `json:"share_factor"`
	EVJackpot     float64   `json:"ev_jackpot"`
	EVBonuses     float64   `json:"ev_bonuses"`
	EVTotal       float64   `json:"ev_total"`
	EVPerShilling float64   `json:"ev_per_shilling"`
	Bonuses       []EVBonus `json:"bonuses"`
	Explain       string    `json:"explain"`
}

// EVBonus is one bonus tier's contribution.
type EVBonus struct {
	Correct int     `json:"correct"`
	Amount  float64 `json:"amount"`
	P       float64 `json:"p"`
	EV      float64 `json:"ev"`
}

// TicketEV: Σ bonus_k · P(exactly k right) + P(all right) · prize · E[share]
// − price. Bonuses are not shared; the prize is shared with K ~ Poisson(λ)
// other winners, λ = tickets_sold × Π public share(pick) (mode "model") or
// the fixed co_winners.
func TicketEV(ps []P3, t Ticket, prize, price float64, bonuses []BonusTier, sh SharingOpts) EVResult {
	qs := make([]float64, len(t))
	for i, k := range t {
		qs[i] = ps[i][k]
	}
	dist := PoissonBinomial(qs)
	n := len(t)
	r := EVResult{TicketPrice: price, Prize: prize, PJackpot: dist[n], Bonuses: []EVBonus{}}
	lambda := 0.0
	if sh.Mode == "fixed" && sh.CoWinners != nil {
		lambda = math.Max(0, *sh.CoWinners)
	} else {
		pub := 1.0
		for i, k := range t {
			pub *= PublicShares(ps[i], sh.Popularity)[k]
		}
		lambda = math.Max(0, sh.TicketsSold) * pub
		r.TicketsSold = sh.TicketsSold
	}
	r.CoWinners = round4(lambda)
	r.ShareFactor = round4(ShareFactor(lambda))
	r.EVJackpot = round2(dist[n] * prize * ShareFactor(lambda))
	bs := append([]BonusTier(nil), bonuses...)
	sort.Slice(bs, func(i, j int) bool { return bs[i].Correct > bs[j].Correct })
	for _, b := range bs {
		if b.Correct < 0 || b.Correct > n || b.Amount <= 0 {
			continue
		}
		p := dist[b.Correct]
		r.Bonuses = append(r.Bonuses, EVBonus{Correct: b.Correct, Amount: b.Amount, P: p, EV: round2(p * b.Amount)})
		r.EVBonuses += p * b.Amount
	}
	r.EVBonuses = round2(r.EVBonuses)
	r.EVTotal = round2(r.EVJackpot + r.EVBonuses - price)
	if price > 0 {
		r.EVPerShilling = round4((r.EVJackpot + r.EVBonuses) / price)
	}
	return r
}

// ---------------------------------------------------------------- distributions

// ExpectedDraws is the expected number of draws and its distribution.
func ExpectedDraws(ps []P3) (float64, []float64) {
	qs := make([]float64, len(ps))
	e := 0.0
	for i, p := range ps {
		qs[i] = p[1]
		e += p[1]
	}
	return e, PoissonBinomial(qs)
}
