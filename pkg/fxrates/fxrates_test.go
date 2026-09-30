package fxrates

import (
	"testing"
	"time"
)

type live map[string]float64

func (l live) Rate(c string) (float64, string, bool) {
	r, ok := l[c]
	src := "live"
	if c == "KES" {
		src = "fallback" // plans.FX reports its table as "fallback"
	}
	return r, src, ok
}

func TestQuoteSources(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 12, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	s := &Service{Live: live{"AUD": 1.5, "EUR": 0.9, "KES": 130}, LiveAsOf: func() *time.Time { return &fresh }, Now: func() time.Time { return now }}

	if q := s.Quote("", "KES", "kes"); q.Source != SourceSame || q.Rate != 1 {
		t.Fatalf("same: %+v", q)
	}
	q := s.Quote("", "AUD", "EUR")
	if q.Source != SourceLive || q.Rate != 0.6 || q.Stale || q.AsOf == nil || !q.AsOf.Equal(fresh) {
		t.Fatalf("live: %+v", q)
	}
	if q := s.Quote("", "KES", "USD"); q.Source != SourceFallback || q.Rate != SigRound(1.0/130) || q.Warning == "" {
		t.Fatalf("fallback: %+v", q)
	}
	old := now.Add(-30 * time.Hour)
	s.LiveAsOf = func() *time.Time { return &old }
	if q := s.Quote("", "AUD", "EUR"); !q.Stale {
		t.Fatalf("stale: %+v", q)
	}
	// No live source at all: the built-in reference table.
	if q := (&Service{}).Quote("", "USD", "JPY"); q.Source != SourceFallback || q.Rate != 148 {
		t.Fatalf("reference: %+v", q)
	}
	if q := (&Service{}).Quote("", "USD", "XXX"); q.OK() {
		t.Fatalf("unsupported: %+v", q)
	}
}

func TestConvertRounding(t *testing.T) {
	for _, c := range []struct {
		amt, rate float64
		cur       string
		want      float64
	}{
		{1500, 0.0077, "USD", 11.55},
		{1500, 1.0 / 129.5, "USD", 11.58},
		{12.34, 100, "JPY", 1234},
		{12.345, 1, "JPY", 12},
		{10, 0.30712, "KWD", 3.071},
		{0.005, 1, "USD", 0.01},
	} {
		if got := Convert(c.amt, c.rate, c.cur); got != c.want {
			t.Errorf("Convert(%v, %v, %s) = %v, want %v", c.amt, c.rate, c.cur, got, c.want)
		}
	}
	if SigRound(1.0/3) != 0.33333333 {
		t.Fatal(SigRound(1.0 / 3))
	}
}
