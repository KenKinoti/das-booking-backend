package football

import (
	"math"
	"testing"
)

// Asian handicap sides carry their own perspective's line: home −0.5 and away
// +0.5 are one market, so they must be de-margined together (agent fbui).
func TestFairGroupKeyPairsHandicapSides(t *testing.T) {
	cases := []struct {
		k    selKey
		want groupKey
	}{
		{selKey{MktAH, "-0.5", SelHome}, groupKey{MktAH, "-0.5"}},
		{selKey{MktAH, "0.5", SelAway}, groupKey{MktAH, "-0.5"}},
		{selKey{MktAH, "-0.25", SelAway}, groupKey{MktAH, "0.25"}},
		{selKey{MktAH, "0", SelAway}, groupKey{MktAH, "0"}},
		{selKey{MktAH, "0", SelHome}, groupKey{MktAH, "0"}},
		{selKey{MktOU, "2.5", SelUnder}, groupKey{MktOU, "2.5"}},
		{selKey{Mkt1X2, "", SelAway}, groupKey{Mkt1X2, ""}},
	}
	for _, c := range cases {
		if got := fairGroupKey(c.k); got != c.want {
			t.Errorf("fairGroupKey(%+v) = %+v, want %+v", c.k, got, c.want)
		}
	}
	// Two complementary prices → proportional de-vig sums to 1 in one group.
	p, _, ok := fairGroup(MktAH, map[string]float64{SelHome: 2.57, SelAway: 1.6})
	if !ok || math.Abs(p[SelHome]+p[SelAway]-1) > 1e-12 || p[SelHome] >= p[SelAway] {
		t.Fatalf("fairGroup AH: %v %v", p, ok)
	}
}

func TestMeanSE(t *testing.T) {
	m, se := meanSE([]float64{1, 2, 3, 4})
	// sd = sqrt(((1.5²+0.5²)*2)/3) = 1.2910; se = sd/2
	if math.Abs(m-2.5) > 1e-12 || math.Abs(se-1.2909944487358056/2) > 1e-12 {
		t.Fatalf("meanSE = %v %v", m, se)
	}
	if m, se := meanSE([]float64{0.2}); m != 0.2 || se != 0 {
		t.Fatalf("one value: %v %v", m, se)
	}
	if m, se := meanSE(nil); m != 0 || se != 0 {
		t.Fatalf("empty: %v %v", m, se)
	}
}
