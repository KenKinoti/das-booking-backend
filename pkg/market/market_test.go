package market

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeUpstream emulates Frankfurter, ExchangeRate-API, CoinGecko, gold-api
// and Alpha Vantage. Set down=true to make every request fail.
type fakeUpstream struct {
	srv   *httptest.Server
	calls map[string]*int64
	down  atomic.Bool
}

func newFake(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{calls: map[string]*int64{}}
	for _, k := range []string{"frank_latest", "frank_hist", "er", "gecko", "gold", "av"} {
		var n int64
		f.calls[k] = &n
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "upstream down", http.StatusBadGateway)
			return
		}
		p := r.URL.Path
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case p == "/frank/latest":
			atomic.AddInt64(f.calls["frank_latest"], 1)
			if q.Get("from") == "" {
				http.Error(w, `{"message":"missing from"}`, 422)
				return
			}
			rates := map[string]float64{}
			all := map[string]float64{"USD": 0.66, "EUR": 0.6, "GBP": 0.5, "NZD": 1.1, "JPY": 98.5, "CNY": 4.7, "AUD": 1.52}
			for _, s := range strings.Split(q.Get("to"), ",") {
				if s == "KES" {
					http.Error(w, `{"message":"not found"}`, 404)
					return
				}
				rates[s] = all[s]
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"amount": 1, "base": q.Get("from"), "date": "2026-09-23", "rates": rates})
		case strings.HasPrefix(p, "/frank/") && strings.Contains(p, ".."):
			atomic.AddInt64(f.calls["frank_hist"], 1)
			rates := map[string]map[string]float64{}
			for i, d := range []string{"2026-08-25", "2026-09-01", "2026-09-22"} {
				m := map[string]float64{}
				for _, s := range strings.Split(q.Get("to"), ",") {
					m[s] = 0.6 + float64(i)*0.02
				}
				rates[d] = m
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"base": q.Get("from"), "rates": rates})
		case strings.HasPrefix(p, "/er/"):
			atomic.AddInt64(f.calls["er"], 1)
			json.NewEncoder(w).Encode(map[string]interface{}{"result": "success", "base_code": strings.TrimPrefix(p, "/er/"),
				"time_last_update_unix": 1790208000, "rates": map[string]float64{"KES": 85.2, "USD": 0.66, "EUR": 0.6, "AUD": 1}})
		case p == "/gecko/coins/markets":
			atomic.AddInt64(f.calls["gecko"], 1)
			if q.Get("vs_currency") == "kes" {
				http.Error(w, `{"error":"invalid vs_currency"}`, 400)
				return
			}
			w.Write([]byte(`[{"id":"bitcoin","symbol":"btc","name":"Bitcoin","current_price":150000,"price_change_percentage_24h_in_currency":1.234,
				"price_change_percentage_7d_in_currency":-2.5,"last_updated":"2026-09-24T01:00:00Z","sparkline_in_7d":{"price":[1,2,3,4]}},
				{"id":"ethereum","symbol":"eth","name":"Ethereum","current_price":6000,"price_change_percentage_24h_in_currency":-0.5,"last_updated":"2026-09-24T01:00:00Z"}]`))
		case strings.HasPrefix(p, "/gold/price/"):
			atomic.AddInt64(f.calls["gold"], 1)
			sym := strings.TrimPrefix(p, "/gold/price/")
			price := map[string]float64{"XAU": 2640, "XAG": 31}[sym]
			json.NewEncoder(w).Encode(map[string]interface{}{"name": sym, "price": price, "symbol": sym, "updatedAt": "2026-09-24T01:02:03Z"})
		case p == "/av":
			atomic.AddInt64(f.calls["av"], 1)
			w.Write([]byte(`{"Global Quote":{"01. symbol":"SPY","05. price":"612.3400","07. latest trading day":"2026-09-23","10. change percent":"0.4512%"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) service(avKey string) (*Service, *time.Time) {
	now := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)
	clock := &now
	fx := &FXProvider{FrankfurterURL: f.srv.URL + "/frank", FallbackURL: f.srv.URL + "/er", Symbols: []string{"USD", "EUR", "GBP", "KES", "NZD", "JPY", "CNY"},
		HistoryDays: 30, CacheTTL: 15 * time.Minute, Now: func() time.Time { return *clock }}
	crypto := &CryptoProvider{BaseURL: f.srv.URL + "/gecko", IDs: []string{"bitcoin", "ethereum"}, CacheTTL: 15 * time.Minute}
	metals := &MetalsProvider{BaseURL: f.srv.URL + "/gold", Symbols: []string{"XAU", "XAG"}, CacheTTL: 15 * time.Minute}
	av := &AlphaVantageProvider{BaseURL: f.srv.URL + "/av", APIKey: avKey, Symbols: []SymbolDef{{"SPY", "S&P 500"}}, CacheTTL: 2 * time.Hour}
	s := NewService(fx, crypto, metals, av)
	s.Client = f.srv.Client()
	s.Now = func() time.Time { return *clock }
	return s, clock
}

func section(o Overview, key string) *Section {
	for i := range o.Sections {
		if o.Sections[i].Key == key {
			return &o.Sections[i]
		}
	}
	return nil
}

func quote(s *Section, sym string) *Quote {
	for i := range s.Items {
		if s.Items[i].Symbol == sym {
			return &s.Items[i]
		}
	}
	return nil
}

func TestOverviewHappyPathAndCaching(t *testing.T) {
	f := newFake(t)
	s, clock := f.service("")
	o := s.Overview(context.Background(), "AUD")
	if o.Base != "AUD" || o.RefreshSecs != 900 {
		t.Fatalf("base/refresh: %+v", o)
	}
	fx := section(o, "fx")
	if fx == nil || !fx.Available || fx.Stale {
		t.Fatalf("fx: %+v", fx)
	}
	usd := quote(fx, "USD")
	if usd == nil || !usd.Available || usd.Price != 0.66 || usd.AsOf != "2026-09-23" || len(usd.Spark) != 4 || usd.Change30D == nil {
		t.Fatalf("usd: %+v", usd)
	}
	if !strings.Contains(usd.Source, "European Central Bank") {
		t.Fatalf("usd source: %s", usd.Source)
	}
	kes := quote(fx, "KES")
	if kes == nil || !kes.Available || kes.Price != 85.2 || !strings.Contains(kes.Source, "ExchangeRate-API") {
		t.Fatalf("kes should come from fallback: %+v", kes)
	}
	if quote(fx, "AUD") != nil {
		t.Fatal("base currency must not be listed against itself")
	}
	cr := section(o, "crypto")
	btc := quote(cr, "BTC")
	if btc == nil || btc.Price != 150000 || btc.Currency != "AUD" || btc.Change1D == nil || *btc.Change1D != 1.23 || len(btc.Spark) != 4 {
		t.Fatalf("btc: %+v", btc)
	}
	gold := quote(section(o, "metals"), "XAU")
	if gold == nil || !gold.Available || gold.Currency != "AUD" || gold.Price != 4000 { // 2640 USD / 0.66
		t.Fatalf("gold: %+v", gold)
	}
	if section(o, "indices") != nil || len(o.Disabled) != 1 || o.Disabled[0] != "indices" {
		t.Fatalf("indices should be skipped without API key: %+v", o.Disabled)
	}
	if len(o.Sources) < 4 {
		t.Fatalf("sources: %+v", o.Sources)
	}

	// Second call within TTL: served from cache
	before := atomic.LoadInt64(f.calls["frank_latest"]) + atomic.LoadInt64(f.calls["gecko"])
	s.Overview(context.Background(), "AUD")
	if after := atomic.LoadInt64(f.calls["frank_latest"]) + atomic.LoadInt64(f.calls["gecko"]); after != before {
		t.Fatalf("expected cache hit, calls %d -> %d", before, after)
	}
	// After TTL: refetched
	*clock = clock.Add(16 * time.Minute)
	s.Overview(context.Background(), "AUD")
	if atomic.LoadInt64(f.calls["frank_latest"]) != 2 {
		t.Fatalf("expected refetch after TTL, got %d", atomic.LoadInt64(f.calls["frank_latest"]))
	}
}

func TestStaleFallbackWhenSourceGoesDown(t *testing.T) {
	f := newFake(t)
	s, clock := f.service("")
	s.Overview(context.Background(), "AUD")
	f.down.Store(true)
	*clock = clock.Add(20 * time.Minute)
	o := s.Overview(context.Background(), "AUD")
	for _, sec := range o.Sections {
		if !sec.Available || !sec.Stale || sec.Error == "" || len(sec.Items) == 0 {
			t.Fatalf("section %s should be stale with last good values: %+v", sec.Key, sec)
		}
	}
	if q := quote(section(o, "fx"), "USD"); q.Price != 0.66 {
		t.Fatalf("stale value lost: %+v", q)
	}
	// Failed source is not retried before RetryAfter elapses
	n := atomic.LoadInt64(f.calls["frank_latest"])
	s.Overview(context.Background(), "AUD")
	if atomic.LoadInt64(f.calls["frank_latest"]) != n {
		t.Fatal("should not retry a failed source immediately")
	}
	// Recovery
	f.down.Store(false)
	*clock = clock.Add(2 * time.Minute)
	o = s.Overview(context.Background(), "AUD")
	if sec := section(o, "fx"); sec.Stale || !sec.Available {
		t.Fatalf("expected recovery: %+v", sec)
	}
}

func TestOfflineReturnsUnavailableSections(t *testing.T) {
	f := newFake(t)
	f.down.Store(true)
	s, _ := f.service("")
	o := s.Overview(context.Background(), "AUD")
	if len(o.Sections) != 3 {
		t.Fatalf("expected 3 sections, got %d", len(o.Sections))
	}
	for _, sec := range o.Sections {
		if sec.Available || sec.Error == "" || sec.Items == nil || len(sec.Sources) == 0 {
			t.Fatalf("section %s should be unavailable with an error and sources: %+v", sec.Key, sec)
		}
	}
}

func TestNonECBBaseAndCryptoConversion(t *testing.T) {
	f := newFake(t)
	s, _ := f.service("")
	o := s.Overview(context.Background(), "kes")
	if o.Base != "KES" {
		t.Fatalf("base %s", o.Base)
	}
	fx := section(o, "fx")
	if usd := quote(fx, "USD"); usd == nil || !usd.Available || !strings.Contains(usd.Source, "ExchangeRate-API") {
		t.Fatalf("non-ECB base should use fallback: %+v", usd)
	}
	if aud := quote(fx, "AUD"); aud == nil || !aud.Available {
		t.Fatalf("AUD should be listed for non-AUD bases: %+v", aud)
	}
	if atomic.LoadInt64(f.calls["frank_latest"]) != 0 {
		t.Fatal("frankfurter should not be called for a non-ECB base")
	}
	btc := quote(section(o, "crypto"), "BTC")
	if btc == nil || !btc.Available || btc.Currency != "KES" || btc.Note == "" {
		t.Fatalf("btc should be converted from USD: %+v", btc)
	}
}

func TestAlphaVantageEnabledWithKey(t *testing.T) {
	f := newFake(t)
	s, _ := f.service("demo")
	o := s.Overview(context.Background(), "AUD")
	idx := section(o, "indices")
	if idx == nil || !idx.Available {
		t.Fatalf("indices: %+v", idx)
	}
	spy := quote(idx, "SPY")
	if spy.Price != 612.34 || spy.Change1D == nil || *spy.Change1D != 0.45 || spy.AsOf != "2026-09-23" {
		t.Fatalf("spy: %+v", spy)
	}
}

func TestHandlerAlways200(t *testing.T) {
	f := newFake(t)
	f.down.Store(true)
	s, _ := f.service("")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{Svc: s, DefaultBase: func(string) string { return "NZD" }}
	g := r.Group("/m", func(c *gin.Context) { c.Set("org_id", "o1") })
	h.RegisterRoutes(g)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/m/overview", nil))
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	var res struct {
		Data Overview `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Data.Base != "NZD" || len(res.Data.Sections) == 0 || res.Data.Sections[0].Available {
		t.Fatalf("unexpected: %+v", res.Data)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/m/overview?base=usd", nil))
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Data.Base != "USD" {
		t.Fatalf("base override: %s", res.Data.Base)
	}
}
