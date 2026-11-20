package model

import (
	"math"
	"testing"
)

// Reference values computed independently (Python/SciPy: Štrumbelj's
// fixed-point iteration for Shin, Brent's method for power), not with this
// package.
func TestDeVigReferenceValues(t *testing.T) {
	cases := []struct {
		odds              []float64
		margin            float64
		prop, shin, power []float64
	}{
		{[]float64{2.10, 3.40, 3.60}, 0.048085901027,
			[]float64{0.454342984410, 0.280623608018, 0.265033407572},
			[]float64{0.458665715961, 0.278737637133, 0.262596646906},
			[]float64{0.460174457860, 0.277979964076, 0.261845578063}},
		{[]float64{1.30, 5.50, 11.0}, 0.041958041958,
			[]float64{0.738255033557, 0.174496644295, 0.087248322148},
			[]float64{0.750905445034, 0.169397608148, 0.079696946818},
			[]float64{0.757191481316, 0.164105242835, 0.078703275849}},
		{[]float64{1.50, 2.60}, 0.051282051282,
			[]float64{0.634146341463, 0.365853658537},
			[]float64{0.641025641026, 0.358974358974}, // = additive for two outcomes
			[]float64{0.644649937534, 0.355350062466}},
		{[]float64{2.5, 3.2, 2.9}, 0.057327586207,
			[]float64{0.378312270689, 0.295556461476, 0.326131267835},
			[]float64{0.380220838107, 0.293956289212, 0.325822872681},
			[]float64{0.380781825376, 0.293565615461, 0.325652559163}},
	}
	for _, c := range cases {
		for _, m := range []struct {
			name string
			want []float64
		}{{DeVigProportional, c.prop}, {DeVigShin, c.shin}, {DeVigPower, c.power}} {
			got, margin := DeVig(c.odds, m.name)
			near(t, m.name+" margin", margin, c.margin, 1e-11)
			s := 0.0
			for i := range got {
				near(t, m.name, got[i], m.want[i], 1e-10)
				s += got[i]
			}
			near(t, m.name+" sum", s, 1, 1e-12)
		}
	}
	near(t, "shin z", ShinZ([]float64{2.10, 3.40, 3.60}), 0.024082836732, 1e-10)
	// Even two-way market: everyone agrees on 50/50.
	for _, m := range []string{DeVigProportional, DeVigShin, DeVigPower} {
		p, _ := DeVig([]float64{1.9, 1.9}, m)
		near(t, m+" even", p[0], 0.5, 1e-12)
	}
}

func TestDeVigEdgeCases(t *testing.T) {
	for _, odds := range [][]float64{nil, {2}, {1.0, 3}, {0.5, 2}, {math.NaN(), 2}, {math.Inf(1), 2}} {
		p, m := DeVig(odds, DeVigShin)
		if p != nil || m != 0 {
			t.Errorf("DeVig(%v) should be invalid", odds)
		}
	}
	// No margin: every method returns the implied probabilities.
	fair := []float64{2, 4, 4}
	for _, m := range []string{DeVigProportional, DeVigShin, DeVigPower, "unknown"} {
		p, mg := DeVig(fair, m)
		near(t, m+" fair margin", mg, 0, 1e-15)
		near(t, m+" fair p0", p[0], 0.5, 1e-9)
	}
	// Arbitrage (negative margin) is handled.
	p, mg := DeVig([]float64{2.2, 2.2}, DeVigShin)
	if mg >= 0 || math.Abs(p[0]-0.5) > 1e-12 {
		t.Errorf("negative margin: %v %v", p, mg)
	}
	p, _ = DeVig([]float64{2.1, 4.4, 4.5}, DeVigPower)
	near(t, "power under-round sum", p[0]+p[1]+p[2], 1, 1e-12)
	if _, _, ok := DeVig3([3]float64{2, 1, 3}, DeVigShin); ok {
		t.Errorf("DeVig3 should reject odds of 1")
	}
}

func TestEVAndKelly(t *testing.T) {
	near(t, "EV", EV(0.5, 2.2), 0.1, 1e-12)
	near(t, "EV negative", EV(0.4, 2.0), -0.2, 1e-12)
	// Full Kelly for p=0.5 at 2.2: (1.1-1)/1.2 = 1/12.
	near(t, "kelly full", Kelly(0.5, 2.2, 1, 0), 1.0/12, 1e-12)
	near(t, "kelly quarter", Kelly(0.5, 2.2, 0.25, 0), 1.0/48, 1e-12)
	near(t, "kelly capped", Kelly(0.6, 3.0, 1, 0.02), 0.02, 1e-12)
	near(t, "kelly cap not binding", Kelly(0.5, 2.2, 0.25, 0.05), 1.0/48, 1e-12)
	for _, c := range []struct{ p, o, f, cap float64 }{
		{0.5, 2.0, 1, 0},           // zero edge
		{0.4, 2.0, 0.25, 0.02},     // negative edge
		{0.5, 1.0, 1, 0},           // odds 1
		{0.5, 0.8, 1, 0},           // odds < 1
		{0, 3, 1, 0},               // p = 0
		{-0.2, 3, 1, 0},            // negative p
		{0.6, 2.5, 0, 0},           // zero fraction
		{0.6, 2.5, -1, 0},          // negative fraction
		{math.NaN(), 2.5, 0.25, 0}, // NaN p
		{0.6, math.NaN(), 0.25, 0},
		{0.6, math.Inf(1), 0.25, 0},
	} {
		if k := Kelly(c.p, c.o, c.f, c.cap); k != 0 {
			t.Errorf("Kelly(%v,%v,%v,%v) = %v, want 0", c.p, c.o, c.f, c.cap, k)
		}
	}
	// p = 1 (certain) at full Kelly would stake everything; fraction and
	// cap still apply; fraction > 1 is clamped.
	near(t, "kelly certain", Kelly(1, 2, 0.25, 0), 0.25, 1e-12)
	near(t, "kelly fraction clamp", Kelly(1.5, 2, 3, 0), 1, 1e-12)
	if k := Kelly(0.55, 2.1, DefaultKellyFraction, DefaultKellyCap); k <= 0 || k > DefaultKellyCap {
		t.Errorf("default kelly out of range: %v", k)
	}
}

func TestBlend(t *testing.T) {
	a := Probs{0.5, 0.3, 0.2}
	b := Probs{0.3, 0.3, 0.4}
	m := Blend([]Probs{a, b}, nil)
	near(t, "equal H", m.H, 0.4, 1e-12)
	near(t, "equal A", m.A, 0.3, 1e-12)
	w := Blend([]Probs{a, b}, []float64{3, 1})
	near(t, "weighted H", w.H, 0.45, 1e-12)
	// Unnormalised inputs and odd weights.
	u := Blend([]Probs{{1, 0.6, 0.4}, b}, []float64{1, -1})
	near(t, "neg weight ignored", u.H, 0.5, 1e-12)
	z := Blend([]Probs{a, b}, []float64{0, 0})
	near(t, "zero weights → equal", z.H, 0.4, 1e-12)
	n := Blend([]Probs{{math.NaN(), 0, 0}, b}, nil)
	near(t, "NaN skipped", n.H, 0.3, 1e-12)
	if !Blend([]Probs{a, b}, []float64{0.7}).Valid() {
		t.Errorf("short weights")
	}
	// Log-linear pooling.
	same := BlendLogLinear([]Probs{a, a}, []float64{0.2, 0.8})
	near(t, "loglin identical", same.H, a.H, 1e-12)
	ll := BlendLogLinear([]Probs{a, b}, nil)
	g := Probs{math.Sqrt(0.5 * 0.3), math.Sqrt(0.3 * 0.3), math.Sqrt(0.2 * 0.4)}.Normalize()
	near(t, "loglin geometric", ll.H, g.H, 1e-12)
	near(t, "loglin D", ll.D, g.D, 1e-12)
	if !ll.Valid() {
		t.Errorf("invalid pooled forecast")
	}
}
