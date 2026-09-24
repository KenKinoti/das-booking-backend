package analytics

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestResolvePeriods(t *testing.T) {
	today := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	cur, prev, err := resolvePeriods(today, "", "", "30d", "previous")
	if err != nil || cur.Days() != 30 || prev == nil || prev.Days() != 30 || !prev.To.Equal(cur.From.AddDate(0, 0, -1)) {
		t.Fatalf("30d: %v %v %v", cur, prev, err)
	}
	cur, prev, _ = resolvePeriods(today, "2026-01-01", "2026-01-31", "", "year")
	if cur.Days() != 31 || prev.From.Year() != 2025 {
		t.Fatalf("yoy: %v %v", cur, prev)
	}
	if _, p, _ := resolvePeriods(today, "", "", "7d", "none"); p != nil {
		t.Fatal("compare=none should have no previous period")
	}
	if _, _, err := resolvePeriods(today, "2026-02-01", "2026-01-01", "", ""); err == nil {
		t.Fatal("expected error for inverted range")
	}
	cur, _, _ = resolvePeriods(today, "", "", "12m", "none")
	if cur.From != time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC) || pickInterval("auto", cur.Days()) != "month" || len(buckets(cur, "month")) != 12 {
		t.Fatalf("12m: %v", cur)
	}
	if b := bucketStart(time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), "week"); b.Weekday() != time.Monday {
		t.Fatalf("week bucket should start Monday: %v", b)
	}
}

func TestDelta(t *testing.T) {
	if d := delta(150, 100); d == nil || *d != 50 {
		t.Fatal("delta 50%")
	}
	if delta(10, 0) != nil {
		t.Fatal("delta from zero should be nil")
	}
	if d := delta(0, 0); d == nil || *d != 0 {
		t.Fatal("0 vs 0 should be 0")
	}
}

// TestOverviewEmptyOrg runs against Postgres when TEST_DATABASE_URL is set
// (the ERP tables must already exist, e.g. a migrated das_* database with
// ?search_path=erp). An organisation with no data must get zeros, not errors.
func TestOverviewEmptyOrg(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/a", func(c *gin.Context) { c.Set("org_id", "no-such-org"); c.Next() })
	NewHandler(db).RegisterRoutes(g)
	for _, q := range []string{"", "?range=12m", "?range=7d&compare=none", "?from=2026-01-01&to=2026-03-31&interval=week&tz=Australia/Sydney"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/a/overview"+q, nil))
		if w.Code != 200 {
			t.Fatalf("%s: code %d %s", q, w.Code, w.Body.String())
		}
		var res struct {
			Data struct {
				Currency string                     `json:"currency"`
				Kpis     map[string]KPI             `json:"kpis"`
				Series   []SeriesPoint              `json:"series"`
				Top      []topCustomer              `json:"top_customers"`
				Activity []activity                 `json:"activity"`
				Cash     map[string]json.RawMessage `json:"cashflow"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Data.Currency != "AUD" || res.Data.Kpis["revenue"].Value != 0 || len(res.Data.Series) == 0 || res.Data.Top == nil || res.Data.Activity == nil {
			t.Fatalf("%s: unexpected %+v", q, res.Data)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/a/overview?from=2026-05-01&to=2026-01-01", nil))
	if w.Code != 400 {
		t.Fatalf("inverted range should be 400, got %d", w.Code)
	}
}
