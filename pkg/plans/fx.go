package plans

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/market"
)

// FX serves USD→X rates: live rates from pkg/market (ECB via Frankfurter,
// ExchangeRate-API for the rest) refreshed in the background, falling back to
// the super-admin editable plan_fx_rates table.
type FX struct {
	mu       sync.RWMutex
	live     map[string]float64
	liveAsOf time.Time
	fallback map[string]float64
	svc      *market.Service
	interval time.Duration
}

// NewFX builds the FX source. Live rates are disabled with MARKET_DISABLE=1
// or PLANS_LIVE_FX=0.
func NewFX() *FX {
	f := &FX{live: map[string]float64{}, fallback: map[string]float64{}, interval: 30 * time.Minute}
	if os.Getenv("MARKET_DISABLE") == "1" || os.Getenv("PLANS_LIVE_FX") == "0" {
		return f
	}
	fx := &market.FXProvider{
		FrankfurterURL: envOr("FRANKFURTER_URL", "https://api.frankfurter.app"),
		FallbackURL:    envOr("EXCHANGERATE_API_URL", "https://open.er-api.com/v6/latest"),
		Symbols:        append([]string{}, currency.Supported...),
		HistoryDays:    1,
		CacheTTL:       f.interval,
	}
	f.svc = market.NewService(fx)
	return f
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// Start refreshes live rates now and then periodically until ctx ends.
func (f *FX) Start(ctx context.Context) {
	if f.svc == nil {
		return
	}
	go func() {
		f.refresh(ctx)
		t := time.NewTicker(f.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				f.refresh(ctx)
			}
		}
	}()
}

func (f *FX) refresh(ctx context.Context) {
	ov := f.svc.Overview(ctx, "USD")
	got := map[string]float64{}
	for _, sec := range ov.Sections {
		if sec.Key != "fx" {
			continue
		}
		for _, q := range sec.Items {
			if q.Available && q.Price > 0 && currency.IsSupported(q.Symbol) {
				got[q.Symbol] = q.Price
			}
		}
	}
	if len(got) == 0 {
		log.Printf("plans: live FX unavailable, using fallback rates")
		return
	}
	f.mu.Lock()
	for k, v := range got {
		f.live[k] = v
	}
	f.liveAsOf = time.Now().UTC()
	f.mu.Unlock()
}

// SetFallback replaces the fallback table.
func (f *FX) SetFallback(rates []FXRate) {
	m := map[string]float64{}
	for _, r := range rates {
		if r.RatePerUSD > 0 {
			m[strings.ToUpper(r.Currency)] = r.RatePerUSD
		}
	}
	f.mu.Lock()
	f.fallback = m
	f.mu.Unlock()
}

// SetLive injects live rates (tests).
func (f *FX) SetLive(m map[string]float64) {
	f.mu.Lock()
	f.live = m
	f.liveAsOf = time.Now().UTC()
	f.mu.Unlock()
}

// Rate implements Rates.
func (f *FX) Rate(code string) (float64, string, bool) {
	code = strings.ToUpper(code)
	if code == "USD" {
		return 1, "fixed", true
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.live[code]; ok && r > 0 {
		return r, "live", true
	}
	if r, ok := f.fallback[code]; ok && r > 0 {
		return r, "fallback", true
	}
	return 0, "", false
}

// RateInfo describes one currency's conversion for the catalogue endpoints.
type RateInfo struct {
	Currency string  `json:"currency"`
	Rate     float64 `json:"rate"`
	Source   string  `json:"source"`
	Fallback float64 `json:"fallback_rate"`
	Live     float64 `json:"live_rate,omitempty"`
	Decimals int     `json:"decimals"`
}

// Table lists every supported currency.
func (f *FX) Table() ([]RateInfo, *time.Time) {
	out := []RateInfo{}
	for _, c := range currency.Supported {
		r, src, _ := f.Rate(c)
		f.mu.RLock()
		ri := RateInfo{Currency: c, Rate: r, Source: src, Fallback: f.fallback[c], Live: f.live[c], Decimals: currency.Decimals(c)}
		f.mu.RUnlock()
		if c == "USD" {
			ri.Fallback = 1
		}
		out = append(out, ri)
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.liveAsOf.IsZero() {
		return out, nil
	}
	t := f.liveAsOf
	return out, &t
}
