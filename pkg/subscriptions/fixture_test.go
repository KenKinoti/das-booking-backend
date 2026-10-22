package subscriptions

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/notify"
)

// DB tests: TEST_DATABASE_URL=postgres://…  (each test gets a fresh schema).
// The invoicing API runs for real (in-process gin engine with JWT auth), so
// invoices, emails and payments go through pkg/invoicing exactly as in the
// app; the Synergy Wholesale API is a fake SOAP server (fakeSynergy).

const testSecret = "test-secret-subscriptions-0123456789"

var schemaN int
var schemaMu sync.Mutex

func testDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	schemaMu.Lock()
	schemaN++
	schema := fmt.Sprintf("subs_test_%d_%d", os.Getpid(), schemaN)
	schemaMu.Unlock()
	boot, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	boot.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	boot.Exec(`CREATE SCHEMA ` + schema)
	if sqlDB, err := boot.DB(); err == nil {
		sqlDB.Close()
	}
	dsn = regexp.MustCompile(`[?&]search_path=[^&]*`).ReplaceAllString(dsn, "")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	all := append(append(Models(), invoicing.Models()...), &notify.Notification{}, &fxrates.ManualRate{}, &mail.SendStatus{}, &expenses.Renewal{})
	all = append(all, emailprefs.Models()...)
	if err := db.AutoMigrate(all...); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE organizations (id varchar(255) primary key, name text, email text, phone text, address_street text, address_suburb text, address_state text, address_postcode text,
			country_code text, currency text, timezone text, region_source text, address_country text, business_type text)`,
		`CREATE TABLE users (id varchar(255) primary key, email text, role text, first_name text, last_name text, organization_id varchar(255), is_active boolean default true, created_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE customers (id varchar(255) primary key, first_name text not null default '', last_name text not null default '', email text, phone text not null default '',
			address_street text, address_suburb text, address_state text, address_postcode text, address_country text, country_code text, currency text, notes text,
			company_name text, tax_number text, website text, payment_terms_days bigint, source text, external_id text, organization_id varchar(255) not null,
			is_active boolean default true, created_at timestamptz, updated_at timestamptz, deleted_at timestamptz)`,
		`CREATE TABLE services (id varchar(255) primary key, organization_id varchar(255) not null, name text not null, description text, category text not null default '',
			duration bigint not null default 0, price numeric(10,2) not null default 0, currency varchar(3) not null default '', price_includes_tax boolean not null default false,
			is_active boolean default true, requires_vehicle boolean default false, created_at timestamptz, updated_at timestamptz, deleted_at timestamptz)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

type sent struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (s *sent) list() []mail.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mail.Message{}, s.msgs...)
}

func (s *sent) reset() {
	s.mu.Lock()
	s.msgs = nil
	s.mu.Unlock()
}

// to lists the subjects sent to addr.
func (s *sent) to(addr string) []string {
	out := []string{}
	for _, m := range s.list() {
		for _, a := range append(append(append([]string{}, m.To...), m.CC...), m.BCC...) {
			if strings.EqualFold(strings.TrimSpace(a), addr) {
				out = append(out, m.Subject)
				break
			}
		}
	}
	return out
}

// body is the text of the first message to addr whose subject contains part.
func (s *sent) body(addr, part string) string {
	for _, m := range s.list() {
		if strings.Contains(m.Subject, part) {
			for _, a := range m.To {
				if strings.EqualFold(a, addr) {
					return m.Text + "\n" + m.HTML
				}
			}
		}
	}
	return ""
}

type fixture struct {
	t        *testing.T
	db       *gorm.DB
	s        *Service
	org      string
	admin    string
	admin2   string
	engine   *gin.Engine
	out      *sent
	now      time.Time
	loc      *time.Location
	customer string
}

const clientEmail = "jane@wanjiku.example"

func setup(t *testing.T, tz, cur, country string) *fixture {
	gin.SetMode(gin.TestMode)
	db := testDB(t)
	t.Setenv("SMTP_HOST", "smtp.test.invalid")
	t.Setenv("SMTP_FROM", "Mursa Billing <billing@mursa.example>")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, db: db, org: uuid.NewString(), admin: uuid.NewString(), admin2: uuid.NewString(), out: &sent{}, loc: loc}
	f.addOrg(f.org, tz, cur, country, f.admin, f.admin2)
	ms := mail.NewService(db)
	ms.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error {
		f.out.mu.Lock()
		f.out.msgs = append(f.out.msgs, m)
		f.out.mu.Unlock()
		return nil
	}
	inv := invoicing.NewHandler(db)
	inv.Mail = ms
	inv.PublicURL = "https://app.example"
	prevPay := invoicing.OnlinePay
	invoicing.OnlinePay = func(*gorm.DB, string) bool { return true }
	t.Cleanup(func() { invoicing.OnlinePay = prevPay })
	r := gin.New()
	api := r.Group("/api/v1")
	auth := api.Group("/")
	auth.Use(middleware.AuthRequired(&config.Config{JWTSecret: testSecret, Environment: "test"}), func(c *gin.Context) {
		if v, ok := c.Get("org_id"); ok && v != nil {
			c.Set("organization_id", fmt.Sprint(v))
		}
		c.Next()
	})
	inv.RegisterRoutes(auth.Group("/invoicing"))
	f.engine = r
	f.s = &Service{DB: db, FX: &fxrates.Service{DB: db}, Mail: ms, Now: func() time.Time { return f.now }, AppURL: "https://app.example", Secret: []byte(testSecret),
		Inv: &APIInvoicer{Engine: func() http.Handler { return r }, Secret: []byte(testSecret)}}
	(&Handler{S: f.s}).RegisterRoutes(auth)
	f.customer = f.addCustomer(f.org, "Wanjiku", "Traders", clientEmail, "+254 700 111222", "", "")
	f.at("2030-01-05")
	return f
}

func (f *fixture) addOrg(org, tz, cur, country string, admins ...string) {
	f.db.Exec(`INSERT INTO organizations (id, name, email, country_code, currency, timezone, region_source) VALUES (?, 'Mursa Hosting', 'hello@mursa.example', ?, ?, ?, 'user')`, org, country, cur, tz)
	for i, a := range admins {
		f.db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id, created_at) VALUES (?, ?, 'admin', 'Ada', 'Owner', ?, now() + (? * interval '1 second'))`,
			a, fmt.Sprintf("owner%d.%s@mursa.example", i+1, org[:6]), org, i)
	}
	f.db.Exec(`INSERT INTO users (id, email, role, first_name, last_name, organization_id, created_at) VALUES (?, ?, 'care_worker', 'Sam', 'Staff', ?, now())`,
		uuid.NewString(), "staff."+org[:6]+"@mursa.example", org)
}

func (f *fixture) adminEmail(i int) string {
	return fmt.Sprintf("owner%d.%s@mursa.example", i, f.org[:6])
}

func (f *fixture) addCustomer(org, first, last, email, phone, cur, country string) string {
	id := uuid.NewString()
	if err := f.db.Exec(`INSERT INTO customers (id, first_name, last_name, email, phone, currency, country_code, organization_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, now(), now())`,
		id, first, last, email, phone, cur, country, org).Error; err != nil {
		f.t.Fatal(err)
	}
	return id
}

// service adds a catalogue service.
func (f *fixture) service(name string, price float64, cur string) string {
	id := uuid.NewString()
	if err := f.db.Exec(`INSERT INTO services (id, organization_id, name, category, duration, price, currency, is_active, created_at, updated_at) VALUES (?, ?, ?, 'Hosting & domains', 0, ?, ?, true, now(), now())`,
		id, f.org, name, price, cur).Error; err != nil {
		f.t.Fatal(err)
	}
	return id
}

// at sets "now" to 09:00 local on day.
func (f *fixture) at(day string) {
	t, err := time.ParseInLocation("2006-01-02 15:04", day+" 09:00", f.loc)
	if err != nil {
		f.t.Fatal(err)
	}
	f.now = t
}

func (f *fixture) day(off int, from string) string { return DayStr(d(from).AddDate(0, 0, off)) }

func (f *fixture) run() RunResult {
	f.t.Helper()
	r := f.s.RunOrg(context.Background(), f.org, RunOptions{})
	for _, e := range r.Errors {
		f.t.Logf("run error: %s", e)
	}
	return r
}

// hosting is a typical subscription: one priced hosting line on a site.
func hosting(start string, price float64, site, supplier string) SubInput {
	return SubInput{Period: PeriodMonthly, StartDate: start, Lines: []LineInput{{Description: "Starter hosting", Kind: KindHosting, UnitPrice: &price}},
		SiteDomain: site, Supplier: supplier, SupplierPlan: "Micro Env"}
}

func (f *fixture) subscribe(in SubInput) *Subscription {
	f.t.Helper()
	if in.CustomerID == "" {
		in.CustomerID = f.customer
	}
	s, err := f.s.Create(f.org, f.admin, in)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) periods(sub string) []BillingPeriod {
	var out []BillingPeriod
	f.db.Where("subscription_id = ?", sub).Order("period_start").Find(&out)
	return out
}

func (f *fixture) sub(id string) Subscription {
	var s Subscription
	f.db.Where("id = ?", id).First(&s)
	return s
}

func (f *fixture) tasks(sub string) []Task {
	var out []Task
	f.db.Where("subscription_id = ?", sub).Order("created_at").Find(&out)
	return out
}

func (f *fixture) token(org, user, role string) string {
	c := jwt.MapClaims{"user_id": user, "org_id": org, "role": role, "email": "x@example.com", "exp": time.Now().Add(time.Hour).Unix()}
	s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(testSecret))
	return s
}

func (f *fixture) api(method, path, tok string, body any) (int, map[string]any) {
	f.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.engine.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// pay records a payment of the whole balance through the invoicing API.
func (f *fixture) pay(invoiceID string) {
	f.t.Helper()
	var bal float64
	f.db.Raw(`SELECT balance_due FROM billing_invoices WHERE id = ?`, invoiceID).Scan(&bal)
	code, out := f.api("POST", "/invoicing/invoices/"+invoiceID+"/payments", f.token(f.org, f.admin, "admin"), map[string]any{"amount": bal, "method": "mobile_money", "date": time.Now().Format("2006-01-02")})
	if code >= 300 {
		f.t.Fatalf("payment: %d %v", code, out)
	}
}

func hasSubject(list []string, part string) bool {
	for _, s := range list {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

func countSubject(list []string, part string) int {
	n := 0
	for _, s := range list {
		if strings.Contains(s, part) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- fake Synergy Wholesale

type fakeCall struct{ Cmd, Identifier, Reason string }

type fakeHost struct{ Hoid, Domain, Plan, Status string }

// fakeSynergy is a Synergy Wholesale SOAP endpoint (rpc/encoded, as the real
// one answers) with listHosting, hostingSuspendService and
// hostingUnsuspendService.
type fakeSynergy struct {
	mu      sync.Mutex
	hosting []fakeHost
	calls   []fakeCall
	failFor map[string]bool // identifier → the call fails
	srv     *httptest.Server
}

func newFakeSynergy(t *testing.T, hosts ...fakeHost) *fakeSynergy {
	fs := &fakeSynergy{hosting: hosts, failFor: map[string]bool{}}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.serve))
	t.Cleanup(fs.srv.Close)
	return fs
}

func soapReply(cmd, inner string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/" xmlns:ns1="urn:WholesaleSystem" ` +
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:SOAP-ENC="http://schemas.xmlsoap.org/soap/encoding/" ` +
		`SOAP-ENV:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><SOAP-ENV:Body><ns1:` + cmd + `Response><return xsi:type="ns1:` + cmd + `Response">` + inner +
		`</return></ns1:` + cmd + `Response></SOAP-ENV:Body></SOAP-ENV:Envelope>`
}

func xmlText(body, tag string) string {
	m := regexp.MustCompile(`<` + tag + `[^>]*>([^<]*)</` + tag + `>`).FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	var s string
	_ = xml.Unmarshal([]byte("<x>"+m[1]+"</x>"), &s)
	return s
}

func (fs *fakeSynergy) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	cmd := strings.TrimPrefix(strings.Trim(r.Header.Get("SOAPAction"), `"`), "urn:WholesaleSystem#")
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	if xmlText(body, "resellerID") != "RS-1001" || xmlText(body, "apiKey") != "sw-key" {
		_, _ = io.WriteString(w, soapReply(cmd, `<status>ERR_LOGIN_FAILED</status><errorMessage>Unable to login to wholesale system</errorMessage>`))
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	switch cmd {
	case "listHosting":
		var items strings.Builder
		for _, h := range fs.hosting {
			items.WriteString(`<item><hoid>` + h.Hoid + `</hoid><domain>` + h.Domain + `</domain><plan>` + h.Plan + `</plan><serviceStatus>` + h.Status + `</serviceStatus></item>`)
		}
		_, _ = io.WriteString(w, soapReply(cmd, fmt.Sprintf(`<status>OK</status><hoidList SOAP-ENC:arrayType="ns1:bulkHostingInfoResponseSingleEntry[%d]">%s</hoidList>`, len(fs.hosting), items.String())))
	case "hostingSuspendService", "hostingUnsuspendService":
		id := xmlText(body, "identifier")
		fs.calls = append(fs.calls, fakeCall{Cmd: cmd, Identifier: id, Reason: xmlText(body, "reason")})
		if fs.failFor[id] {
			_, _ = io.WriteString(w, soapReply(cmd, `<status>ERR_HOSTING_ACTION_FAILED</status><errorMessage>Unable to change the service</errorMessage>`))
			return
		}
		for i := range fs.hosting {
			if fs.hosting[i].Hoid == id {
				fs.hosting[i].Status = map[string]string{"hostingSuspendService": "Suspended", "hostingUnsuspendService": "Active"}[cmd]
			}
		}
		_, _ = io.WriteString(w, soapReply(cmd, `<status>OK</status>`))
	default:
		_, _ = io.WriteString(w, soapReply(cmd, `<status>ERR_UNKNOWN_COMMAND</status><errorMessage>Unknown command</errorMessage>`))
	}
}

func (fs *fakeSynergy) callsOf(cmd string) []fakeCall {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := []fakeCall{}
	for _, c := range fs.calls {
		if c.Cmd == cmd {
			out = append(out, c)
		}
	}
	return out
}

// connect makes the fixture's organisation use the fake API.
func (f *fixture) connect(fs *fakeSynergy) *SynergyActions {
	a := &SynergyActions{Client: func(org string) (*expenses.SynergyClient, string) {
		if org != f.org {
			return nil, "the Synergy Wholesale API is not connected (Expenses → Sources)"
		}
		c := expenses.NewSynergyClient("RS-1001", "sw-key")
		c.URL = fs.srv.URL
		return c, ""
	}}
	f.s.Supplier = a
	return a
}
