package hr

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"os"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestCalcLineHourly(t *testing.T) {
	r, err := CalcLine(LineInput{PayType: PayHourly, Rate: 32.5, Hours: 38, Frequency: FreqWeekly, DeductionRate: 20, Currency: "AUD"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Gross != 1235 || r.Deductions != 247 || r.Net != 988 {
		t.Fatalf("got %+v", r)
	}
}

func TestCalcLineSalaryPerFrequency(t *testing.T) {
	cases := []struct {
		freq  string
		gross float64
	}{
		{FreqWeekly, 1000},      // 52000 / 52
		{FreqFortnightly, 2000}, // 52000 / 26
		{FreqMonthly, 4333.33},  // 52000 / 12
	}
	for _, tc := range cases {
		r, err := CalcLine(LineInput{PayType: PaySalary, Rate: 52000, Frequency: tc.freq, DeductionRate: 0, Currency: "AUD"})
		if err != nil {
			t.Fatal(err)
		}
		if r.Gross != tc.gross || r.Net != tc.gross || r.Deductions != 0 {
			t.Errorf("%s: got %+v want gross %v", tc.freq, r, tc.gross)
		}
	}
}

func TestCalcLineRoundingKeepsBalance(t *testing.T) {
	// 7.333 h × 27.27 = 199.97091 → 199.97; 17.5% → 34.99475 → 34.99; net 164.98
	r, _ := CalcLine(LineInput{PayType: PayHourly, Rate: 27.27, Hours: 7.333, DeductionRate: 17.5, Currency: "USD"})
	if r.Gross != 199.97 || r.Deductions != 34.99 || r.Net != 164.98 {
		t.Fatalf("got %+v", r)
	}
	if RoundTo(r.Deductions+r.Net, "USD") != r.Gross {
		t.Fatal("gross != deductions + net")
	}
}

func TestCalcLineCurrencyDecimals(t *testing.T) {
	r, _ := CalcLine(LineInput{PayType: PaySalary, Rate: 1_000_000, Frequency: FreqMonthly, DeductionRate: 10, Currency: "JPY"})
	if r.Gross != 83333 || r.Deductions != 8333 || r.Net != 75000 {
		t.Fatalf("JPY got %+v", r)
	}
	r, _ = CalcLine(LineInput{PayType: PayHourly, Rate: 3.3333, Hours: 1, Currency: "KWD"})
	if r.Gross != 3.333 {
		t.Fatalf("KWD got %+v", r)
	}
}

func TestCalcLineErrors(t *testing.T) {
	bad := []LineInput{
		{PayType: "weird", Rate: 1, Currency: "USD"},
		{PayType: PaySalary, Rate: 1, Frequency: "daily", Currency: "USD"},
		{PayType: PayHourly, Rate: -1, Hours: 1, Currency: "USD"},
		{PayType: PayHourly, Rate: 1, Hours: 1, DeductionRate: 101, Currency: "USD"},
	}
	for i, in := range bad {
		if _, err := CalcLine(in); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestPeriodEndAndLeaveDays(t *testing.T) {
	d := func(s string) time.Time { v, _ := time.Parse(dateLayout, s); return v }
	if got := PeriodEnd(d("2026-09-07"), FreqWeekly).Format(dateLayout); got != "2026-09-13" {
		t.Error("weekly", got)
	}
	if got := PeriodEnd(d("2026-09-07"), FreqFortnightly).Format(dateLayout); got != "2026-09-20" {
		t.Error("fortnightly", got)
	}
	if got := PeriodEnd(d("2026-02-01"), FreqMonthly).Format(dateLayout); got != "2026-02-28" {
		t.Error("monthly", got)
	}
	// Fri 2026-09-25 .. Tue 2026-09-29 = Fri, Mon, Tue
	if got := LeaveDays(d("2026-09-25"), d("2026-09-29")); got != 3 {
		t.Error("leave days", got)
	}
}

func TestPayRunCSV(t *testing.T) {
	r := &PayRun{Number: "PR-0001", PeriodStart: "2026-09-07", PeriodEnd: "2026-09-13", PayDate: "2026-09-14", Currency: "AUD",
		TotalGross: 1235, TotalDeductions: 247, TotalNet: 988,
		Lines: []PayRunLine{{EmployeeName: "=cmd", PayType: PayHourly, Hours: 38, Rate: 32.5, Gross: 1235, Deductions: 247, Net: 988}}}
	out := string(PayRunCSV(r))
	if !strings.Contains(out, "'=cmd") || !strings.Contains(out, "TOTAL") || !strings.Contains(out, "1235.00,247.00,988.00") {
		t.Fatal(out)
	}
}

// --- end-to-end through the HTTP handlers on a real Postgres database ---

// testDB connects to TEST_DATABASE_URL (skipping when unset) and empties the HR tables.
func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skip("no database:", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"hr_pay_run_lines", "hr_pay_runs", "hr_timesheets", "hr_leave_requests", "hr_employees", "hr_departments"} {
		db.Exec("DELETE FROM " + tbl)
	}
	return db
}

func testServer(t *testing.T) (*gin.Engine, *gorm.DB) {
	gin.SetMode(gin.TestMode)
	db := testDB(t)
	h := NewHandler(db)
	h.Now = func() time.Time { return time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC) }
	r := gin.New()
	g := r.Group("/hr", func(c *gin.Context) {
		c.Set("org_id", "org1")
		c.Set("user_id", "u1")
		c.Set("user_role", "admin")
	})
	h.RegisterRoutes(g)
	return r, db
}

func call(t *testing.T, r *gin.Engine, method, path string, body interface{}) (int, map[string]interface{}) {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func dataOf(m map[string]interface{}) map[string]interface{} {
	d, _ := m["data"].(map[string]interface{})
	return d
}

func TestPayRunFlow(t *testing.T) {
	r, _ := testServer(t)
	code, res := call(t, r, "POST", "/hr/employees", map[string]interface{}{"first_name": "Ann", "last_name": "Hourly", "pay_type": "hourly", "pay_rate": 30, "currency": "AUD"})
	if code != 201 {
		t.Fatal(code, res)
	}
	ann := dataOf(res)["id"].(string)
	code, _ = call(t, r, "POST", "/hr/employees", map[string]interface{}{"first_name": "Sal", "pay_type": "salary", "pay_rate": 52000, "currency": "AUD"})
	if code != 201 {
		t.Fatal(code)
	}
	// Other-currency employee must not be included.
	call(t, r, "POST", "/hr/employees", map[string]interface{}{"first_name": "Usd", "pay_type": "salary", "pay_rate": 52000, "currency": "USD"})

	for _, d := range []string{"2026-09-14", "2026-09-15"} {
		code, res = call(t, r, "POST", "/hr/timesheets", map[string]interface{}{"employee_id": ann, "date": d, "hours": 8})
		if code != 201 {
			t.Fatal(code, res)
		}
		id := dataOf(res)["id"].(string)
		if code, _ := call(t, r, "POST", "/hr/timesheets/"+id+"/approve", nil); code != 200 {
			t.Fatal("approve", code)
		}
	}
	// Unapproved hours are not paid.
	call(t, r, "POST", "/hr/timesheets", map[string]interface{}{"employee_id": ann, "date": "2026-09-16", "hours": 5})

	code, res = call(t, r, "POST", "/hr/payruns", map[string]interface{}{"frequency": "weekly", "period_start": "2026-09-14", "currency": "AUD", "deduction_rate": 10})
	if code != 201 {
		t.Fatal(code, res)
	}
	run := dataOf(res)["pay_run"].(map[string]interface{})
	// Ann: 16h × 30 = 480; Sal: 52000/52 = 1000 → gross 1480, ded 148, net 1332
	if run["total_gross"].(float64) != 1480 || run["total_deductions"].(float64) != 148 || run["total_net"].(float64) != 1332 {
		t.Fatalf("totals %+v", run)
	}
	if run["employee_count"].(float64) != 2 || run["period_end"] != "2026-09-20" {
		t.Fatalf("run %+v", run)
	}
	id := run["id"].(string)

	// Salaried staff can't be paid twice in the same period.
	code, res = call(t, r, "POST", "/hr/payruns/preview", map[string]interface{}{"frequency": "weekly", "period_start": "2026-09-14", "currency": "AUD"})
	if code != 200 || len(dataOf(res)["skipped"].([]interface{})) != 1 {
		t.Fatalf("preview %d %+v", code, res)
	}

	code, res = call(t, r, "POST", "/hr/payruns/"+id+"/paid", nil)
	if code != 200 || dataOf(res)["status"] != "paid" {
		t.Fatal(code, res)
	}
	// Paid hours are locked.
	code, res = call(t, r, "GET", "/hr/timesheets?status=approved", nil)
	for _, ts := range dataOf(res)["timesheets"].([]interface{}) {
		if ts.(map[string]interface{})["pay_run_id"] != id {
			t.Fatal("timesheet not linked to pay run")
		}
	}
	if code, _ := call(t, r, "DELETE", "/hr/payruns/"+id, nil); code != 409 {
		t.Fatal("paid run deleted", code)
	}
	req := httptest.NewRequest("GET", "/hr/payruns/"+id+"/export", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "1480.00,148.00,1332.00") {
		t.Fatal("csv", w.Body.String())
	}
}

func TestLeaveValidationAndApproval(t *testing.T) {
	r, _ := testServer(t)
	_, res := call(t, r, "POST", "/hr/employees", map[string]interface{}{"first_name": "Lee", "pay_type": "hourly", "pay_rate": 25})
	emp := dataOf(res)["id"].(string)
	code, _ := call(t, r, "POST", "/hr/leave", map[string]interface{}{"employee_id": emp, "type": "annual", "start_date": "2026-10-10", "end_date": "2026-10-01"})
	if code != 400 {
		t.Fatal("end before start accepted", code)
	}
	code, res = call(t, r, "POST", "/hr/leave", map[string]interface{}{"employee_id": emp, "type": "annual", "start_date": "2026-09-21", "end_date": "2026-09-27"})
	if code != 201 || dataOf(res)["days"].(float64) != 5 {
		t.Fatal(code, res)
	}
	lid := dataOf(res)["id"].(string)
	code, _ = call(t, r, "POST", "/hr/leave", map[string]interface{}{"employee_id": emp, "type": "sick", "start_date": "2026-09-23", "end_date": "2026-09-23"})
	if code != 409 {
		t.Fatal("overlap accepted", code)
	}
	if code, _ := call(t, r, "POST", "/hr/leave/"+lid+"/approve", nil); code != 200 {
		t.Fatal(code)
	}
	_, res = call(t, r, "GET", "/hr/summary", nil)
	if dataOf(res)["on_leave_today"].(float64) != 1 {
		t.Fatalf("summary %+v", res)
	}
}

func TestStaffCannotApprove(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testDB(t)
	r := gin.New()
	NewHandler(db).RegisterRoutes(r.Group("/hr", func(c *gin.Context) { c.Set("org_id", "o"); c.Set("user_role", "care_worker") }))
	req := httptest.NewRequest("POST", "/hr/employees", strings.NewReader(`{"first_name":"x","pay_rate":1}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
}
