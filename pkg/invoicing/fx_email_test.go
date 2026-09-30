package invoicing

import (
	"bytes"
	"compress/zlib"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
)

// ---------------------------------------------------------------- helpers

// pdfText extracts the text drawn on a PDF produced by RenderPDF (zlib
// streams, UTF-16BE strings).
func pdfText(t *testing.T, data []byte) string {
	t.Helper()
	var out strings.Builder
	re := regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	str := regexp.MustCompile(`(?s)\(((?:\\.|[^\\)])*)\)\s*Tj`)
	for _, m := range re.FindAllSubmatch(data, -1) {
		r, err := zlib.NewReader(bytes.NewReader(m[1]))
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(r)
		for _, s := range str.FindAllSubmatch(raw, -1) {
			b := s[1]
			var un []byte
			for i := 0; i < len(b); i++ {
				if b[i] == '\\' && i+1 < len(b) {
					i++
					switch b[i] {
					case 'n':
						un = append(un, '\n')
					case 'r':
						un = append(un, '\r')
					default:
						un = append(un, b[i])
					}
					continue
				}
				un = append(un, b[i])
			}
			var rs []rune
			for i := 0; i+1 < len(un); i += 2 {
				rs = append(rs, rune(un[i])<<8|rune(un[i+1]))
			}
			out.WriteString(string(rs) + "\n")
		}
	}
	return out.String()
}

type staticLive map[string]float64

func (s staticLive) Rate(code string) (float64, string, bool) {
	r, ok := s[code]
	return r, "live", ok
}

type mailEnv struct {
	r    *gin.Engine
	org  string
	sent *[]mail.Message
	db   *gorm.DB
	h    *Handler
}

func setupMailEnv(t *testing.T) mailEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	t.Setenv("SMTP_HOST", "")
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.IntegrationSettings{}, &mail.SendStatus{}, &fxrates.ManualRate{}); err != nil {
		t.Fatal(err)
	}
	org := "test-org-" + uuid.NewString()
	t.Cleanup(func() { db.Unscoped().Where("organization_id = ?", org).Delete(&models.IntegrationSettings{}) })
	enc, _ := mail.EncryptSecret("pw")
	row := models.IntegrationSettings{ID: uuid.NewString(), OrganizationID: org, EmailSettings: models.EmailSettings{
		Enabled: true, SMTPHost: "smtp.x.test", SMTPPort: 587, SMTPSecurity: "tls", SMTPUsername: "u@x.test", SMTPPassword: enc, FromEmail: "shop@x.test"}}
	if err := db.Omit("Organization").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	svc := mail.NewService(db)
	sent := &[]mail.Message{}
	svc.Transport = func(_ context.Context, _ mail.Config, m mail.Message) error { *sent = append(*sent, m); return nil }
	// Base (home) currency in the test DB is the default, AUD. 1 USD = 1.5 AUD.
	FX = &fxrates.Service{DB: db, Live: staticLive{"AUD": 1.5, "KES": 130, "JPY": 150, "KWD": 0.3, "EUR": 0.9}, LiveAsOf: func() *time.Time { t := time.Now(); return &t }}
	t.Cleanup(func() { FX = nil })
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(db)
	h.Mail = svc
	h.PublicURL = "https://app.example.com"
	h.RegisterRoutes(r.Group("/api", func(c *gin.Context) { c.Set("org_id", c.GetHeader("X-Org")); c.Set("user_id", "u1"); c.Next() }))
	h.RegisterPublicRoutes(r.Group("/public"))
	call(t, r, org, "PUT", "/api/settings", map[string]interface{}{"business_name": "Acme Ltd", "tax_number": "123", "tax_number_label": "ABN",
		"invoice_prefix": "INV-", "next_invoice_number": 1001, "quote_prefix": "QUO-", "next_quote_number": 1001, "number_padding": 4,
		"default_due_days": 14, "quote_valid_days": 30, "tax_label": "GST"})
	return mailEnv{r: r, org: org, sent: sent, db: db, h: h}
}

func num(v interface{}) float64 { f, _ := v.(float64); return f }

// ---------------------------------------------------------------- amounts

// The amounts in the subject, text, HTML and PDF are the document's, in its
// own currency and decimals — with discounts, tax-inclusive prices, shipping
// and a deposit (regression: "for {{total}}" used to be the balance, and an
// unrounded shipping amount made the printed lines not add up).
func TestEmailAmountsAcrossCurrencies(t *testing.T) {
	e := setupMailEnv(t)
	cases := []struct {
		cur                              string
		totalStr, balanceStr, depositStr string
		wantDecimalsTotal                float64
	}{
		{cur: "AUD", totalStr: "A$3,313.99", balanceStr: "A$3,213.99", depositStr: "A$100.00", wantDecimalsTotal: 3313.99},
		{cur: "KES", totalStr: "KSh 3,313.99", balanceStr: "KSh 3,213.99", depositStr: "KSh 100.00", wantDecimalsTotal: 3313.99},
		{cur: "JPY", totalStr: "¥3,314", balanceStr: "¥3,214", depositStr: "¥100", wantDecimalsTotal: 3314},
		{cur: "KWD", totalStr: "KWD 3,313.996", balanceStr: "KWD 3,213.996", depositStr: "KWD 100.000", wantDecimalsTotal: 3313.996},
	}
	for _, tc := range cases {
		t.Run(tc.cur, func(t *testing.T) {
			*e.sent = nil
			code, res := call(t, e.r, e.org, "POST", "/api/invoices", map[string]interface{}{
				"client_name": "Client " + tc.cur, "client_email": "c@x.test", "currency": tc.cur, "prices_include_tax": true,
				"discount_type": "percent", "discount_value": 10, "shipping_amount": 12.345,
				"items": []map[string]interface{}{
					{"description": "Widget", "quantity": 3, "unit_price": 1234.567, "tax_rate": 16, "discount_percent": 5},
					{"description": "Service", "quantity": 1.5, "unit_price": 99.99, "tax_rate": 0},
				},
				"deposit": map[string]interface{}{"amount": 100, "date": "2026-09-30", "method": "cash"},
			})
			if code != 201 {
				t.Fatalf("create: %d %v", code, res)
			}
			inv := data(res)
			if num(inv["total"]) != tc.wantDecimalsTotal {
				t.Fatalf("total = %v, want %v", inv["total"], tc.wantDecimalsTotal)
			}
			// Shipping is stored in minor units, so the printed lines add up.
			if s := num(inv["shipping_amount"]); hasMoreDecimals(s, tc.cur) {
				t.Fatalf("shipping not rounded: %v", s)
			}
			id := inv["id"].(string)
			code, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email"})
			if code != 200 || len(*e.sent) != 1 {
				t.Fatalf("send: %d %v", code, res)
			}
			m := (*e.sent)[0]
			// Default message: "… for {{total}} …" is the document total.
			if !strings.Contains(m.Text, "for "+tc.totalStr+",") {
				t.Fatalf("text should quote the total %s:\n%s", tc.totalStr, m.Text)
			}
			for _, want := range []string{"Amount due: " + tc.balanceStr, "Part-paid — " + tc.depositStr + " received", "Balance due: " + tc.balanceStr, "Total " + tc.cur + ": " + tc.totalStr} {
				if !strings.Contains(m.Text, want) {
					t.Fatalf("text missing %q:\n%s", want, m.Text)
				}
			}
			if !strings.Contains(m.HTML, ">"+tc.balanceStr+"</div>") || strings.Contains(m.HTML, "{{") || strings.Contains(m.Text, "{{") {
				t.Fatalf("html amount due / placeholders wrong")
			}
			pdf := pdfText(t, m.Attachments[0].Data)
			for _, want := range []string{tc.totalStr, tc.balanceStr, tc.depositStr} {
				if !strings.Contains(pdf, want) {
					t.Fatalf("PDF missing %q:\n%s", want, pdf)
				}
			}
			// Every amount the email shows is in the PDF too.
			for _, a := range findAmounts(m.Text, tc.cur) {
				if a.Currency == tc.cur && !strings.Contains(pdf, a.Text) {
					t.Fatalf("email shows %q which the PDF doesn't", a.Text)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- FX

func TestExchangeRateLockedAndConverted(t *testing.T) {
	e := setupMailEnv(t)
	// A catalog price of A$150.00 on a USD invoice: rate AUD→USD = 1/1.5.
	orig := 150.0
	code, res := call(t, e.r, e.org, "POST", "/api/invoices", map[string]interface{}{
		"client_name": "US client", "client_email": "us@x.test", "currency": "USD",
		"items": []map[string]interface{}{{"description": "Consulting", "quantity": 2, "unit_price": 100, "original_unit_price": orig, "original_currency": "AUD"}},
	})
	if code != 201 {
		t.Fatalf("create: %d %v", code, res)
	}
	inv := data(res)
	id := inv["id"].(string)
	if inv["base_currency"] != "AUD" || inv["rate_source"] != "live" || num(inv["exchange_rate"]) != 0.66666667 {
		t.Fatalf("rate not locked: %v %v %v", inv["base_currency"], inv["rate_source"], inv["exchange_rate"])
	}
	items := inv["items"].([]interface{})
	it := items[0].(map[string]interface{})
	if num(it["unit_price"]) != 100 || num(it["original_unit_price"]) != 150 || it["original_currency"] != "AUD" {
		t.Fatalf("line not converted: %v", it)
	}
	if num(inv["total"]) != 200 || num(inv["total_base"]) != 300 {
		t.Fatalf("totals: %v base %v", inv["total"], inv["total_base"])
	}
	// The market moves; saving again keeps the locked rate.
	FX.Live = staticLive{"AUD": 1.6}
	body := map[string]interface{}{"client_name": "US client", "client_email": "us@x.test", "currency": "USD", "exchange_rate": 0.66666667, "rate_source": "live",
		"items": []map[string]interface{}{{"description": "Consulting", "quantity": 2, "unit_price": 100, "original_unit_price": orig, "original_currency": "AUD"}}}
	_, res = call(t, e.r, e.org, "PUT", "/api/invoices/"+id, body)
	if num(data(res)["exchange_rate"]) != 0.66666667 || num(data(res)["total"]) != 200 {
		t.Fatalf("rate must stay locked: %v", data(res)["exchange_rate"])
	}
	// "Update rate" on the draft re-prices the converted line: 150 / 1.6 = 93.75.
	body["update_rate"] = true
	delete(body, "exchange_rate")
	_, res = call(t, e.r, e.org, "PUT", "/api/invoices/"+id, body)
	d := data(res)
	if num(d["exchange_rate"]) != 0.625 || num(d["total"]) != 187.5 || num(d["total_base"]) != 300 {
		t.Fatalf("update rate: rate %v total %v base %v", d["exchange_rate"], d["total"], d["total_base"])
	}
	// Once sent the rate can't change.
	call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email"})
	code, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/rate", map[string]interface{}{})
	if code != 409 {
		t.Fatalf("rate after send: %d %v", code, res)
	}

	// JPY (0 decimals) and KWD (3 decimals) conversions round to minor units.
	FX.Live = staticLive{"AUD": 1.5, "JPY": 150, "KWD": 0.3}
	for cur, want := range map[string]float64{"JPY": 1234, "KWD": 2.468} {
		_, res = call(t, e.r, e.org, "POST", "/api/invoices", map[string]interface{}{"client_name": "X", "currency": cur,
			"items": []map[string]interface{}{{"description": "Item", "quantity": 1, "unit_price": 0, "original_unit_price": 12.34, "original_currency": "AUD"}}})
		li := data(res)["items"].([]interface{})[0].(map[string]interface{})
		if num(li["unit_price"]) != want {
			t.Fatalf("%s conversion: %v, want %v", cur, li["unit_price"], want)
		}
	}
	// A manual override wins over live rates.
	if _, err := FX.SaveManual(e.org, "u1", "AUD", "EUR", 0.61, "agreed"); err != nil {
		t.Fatal(err)
	}
	_, res = call(t, e.r, e.org, "POST", "/api/invoices", map[string]interface{}{"client_name": "EU", "currency": "EUR",
		"items": []map[string]interface{}{{"description": "Item", "quantity": 1, "unit_price": 0, "original_unit_price": 100, "original_currency": "AUD"}}})
	if d := data(res); d["rate_source"] != "manual" || num(d["total"]) != 61 {
		t.Fatalf("manual rate: %v %v", d["rate_source"], d["total"])
	}
}

func TestQuoteConversionKeepsRate(t *testing.T) {
	e := setupMailEnv(t)
	_, res := call(t, e.r, e.org, "POST", "/api/invoices", map[string]interface{}{"doc_type": "quote", "client_name": "Q", "currency": "USD",
		"items": []map[string]interface{}{{"description": "Item", "quantity": 1, "unit_price": 0, "original_unit_price": 300, "original_currency": "AUD"}}})
	q := data(res)
	FX.Live = staticLive{"AUD": 2}
	_, res = call(t, e.r, e.org, "POST", "/api/invoices/"+q["id"].(string)+"/convert", nil)
	inv := data(res)
	if num(inv["exchange_rate"]) != num(q["exchange_rate"]) || num(inv["total"]) != num(q["total"]) || num(inv["total"]) != 200 {
		t.Fatalf("converted invoice changed amounts: %v→%v %v→%v", q["exchange_rate"], inv["exchange_rate"], q["total"], inv["total"])
	}
}

// ---------------------------------------------------------------- preflight

func TestPreflightBlocksAndPasses(t *testing.T) {
	e := setupMailEnv(t)
	_, res := call(t, e.r, e.org, "POST", "/api/invoices", map[string]interface{}{"client_name": "Acme", "client_email": "client@x.test", "currency": "AUD",
		"items": []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 1000, "tax_rate": 10}}})
	id := data(res)["id"].(string)
	path := "/api/invoices/" + id

	checks := func(res map[string]interface{}) map[string]string {
		out := map[string]string{}
		d := data(res)
		list, _ := d["checks"].([]interface{})
		if list == nil {
			if e, ok := res["error"].(map[string]interface{}); ok {
				list, _ = e["checks"].([]interface{})
			}
		}
		for _, c := range list {
			m := c.(map[string]interface{})
			out[m["id"].(string)] = m["severity"].(string)
		}
		return out
	}

	// A typed (stale) amount and an unknown placeholder are errors.
	bad := map[string]interface{}{"mode": "email", "subject": "Invoice {{number}}", "message": "Please pay A$999.00 by {{due}}.", "accept_warnings": false}
	code, res := call(t, e.r, e.org, "POST", path+"/preflight", bad)
	c := checks(res)
	if code != 200 || c["amounts"] != "error" || c["placeholders"] != "error" || c["totals"] != "ok" || c["fx"] != "ok" {
		t.Fatalf("preflight: %d %v", code, c)
	}
	prev := data(res)["preview"].(map[string]interface{})
	if !strings.HasPrefix(prev["subject"].(string), "Invoice INV-") || !strings.Contains(prev["html"].(string), "A$1,100.00") {
		t.Fatalf("preview: %v", prev["subject"])
	}
	code, res = call(t, e.r, e.org, "POST", path+"/send", bad)
	if code != 422 || res["error"].(map[string]interface{})["code"] != "PREFLIGHT_FAILED" || len(*e.sent) != 0 {
		t.Fatalf("send with errors: %d %v", code, res)
	}
	// Correct amounts pass; the missing-logo warning needs "Send anyway".
	good := map[string]interface{}{"mode": "email", "subject": "Invoice {{number}}", "message": "Please pay {{amount_due}} (A$1,100.00) by {{due_date}}.", "accept_warnings": false}
	code, res = call(t, e.r, e.org, "POST", path+"/send", good)
	if code != 409 || res["error"].(map[string]interface{})["code"] != "PREFLIGHT_WARNINGS" || checks(res)["logo"] != "warning" {
		t.Fatalf("send with warnings: %d %v", code, res)
	}
	good["accept_warnings"] = true
	code, res = call(t, e.r, e.org, "POST", path+"/send", good)
	if code != 200 || len(*e.sent) != 1 || data(res)["message_id"] == "" {
		t.Fatalf("send anyway: %d %v", code, res)
	}
	if m := (*e.sent)[0]; !strings.Contains(m.Text, "Please pay A$1,100.00 (A$1,100.00)") || m.MessageID == "" {
		t.Fatalf("sent text: %s", m.Text)
	}
	// Sending again within 10 minutes warns.
	_, res = call(t, e.r, e.org, "POST", path+"/preflight", good)
	if checks(res)["duplicate"] != "warning" {
		t.Fatalf("duplicate: %v", checks(res))
	}
	// The log has the blocked and the sent attempt, with checks and amounts.
	_, res = call(t, e.r, e.org, "GET", path+"/email-log", nil)
	log := data(res)["log"].([]interface{})
	if len(log) != 3 {
		t.Fatalf("log entries: %d", len(log))
	}
	last := log[0].(map[string]interface{})
	if last["status"] != "sent" || last["message_id"] == "" || last["amounts"].(map[string]interface{})["Amount due"] != "A$1,100.00" {
		t.Fatalf("log: %v", last)
	}
	// A reminder for a paid invoice is refused.
	call(t, e.r, e.org, "POST", path+"/payments", map[string]interface{}{"amount": 1100, "date": "2026-09-30", "method": "cash"})
	code, _ = call(t, e.r, e.org, "POST", path+"/preflight", map[string]interface{}{"kind": "reminder"})
	if code != 409 {
		t.Fatalf("reminder on a paid invoice: %d", code)
	}
}

func TestMissingRateBlocksSend(t *testing.T) {
	e := setupMailEnv(t)
	FX.Live = staticLive{"AUD": 1.5} // no NGN, and the reference table is bypassed below
	_, res := call(t, e.r, e.org, "POST", "/api/invoices", map[string]interface{}{"client_name": "N", "client_email": "n@x.test", "currency": "USD",
		"items": []map[string]interface{}{{"description": "Work", "quantity": 1, "unit_price": 10}}})
	id := data(res)["id"].(string)
	// Simulate a legacy document without a rate.
	e.db.Model(&Invoice{}).Where("id = ?", id).Updates(map[string]interface{}{"exchange_rate": 0, "rate_source": "missing"})
	code, res := call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email"})
	if code != 422 || !strings.Contains(res["error"].(map[string]interface{})["message"].(string), "No exchange rate") {
		t.Fatalf("missing rate: %d %v", code, res)
	}
	code, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/mark-sent", nil)
	if code != 422 {
		t.Fatalf("mark sent without rate: %d %v", code, res)
	}
	code, res = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/rate", map[string]interface{}{"exchange_rate": 0.65})
	if code != 200 || data(res)["rate_source"] != "manual" {
		t.Fatalf("lock rate: %d %v", code, res)
	}
	code, _ = call(t, e.r, e.org, "POST", "/api/invoices/"+id+"/send", map[string]interface{}{"mode": "email"})
	if code != 200 {
		t.Fatalf("send after locking: %d", code)
	}
}

// ---------------------------------------------------------------- template

func TestPlaceholders(t *testing.T) {
	out, missing := fillTemplate("Hi {{client}}, {{ total }} {{nope}}", map[string]string{"client": "Sam", "total": "$5"})
	if out != "Hi Sam, $5 {{nope}}" || len(missing) != 1 || missing[0] != "nope" {
		t.Fatalf("%q %v", out, missing)
	}
	got := findAmounts("Pay KSh 1,500.00 or $20 — 3,000 KES, USD 7.5, BUSD 9", "AUD")
	want := []string{"KSh 1,500.00/KES", "$20/AUD", "USD 7.5/USD", "3,000 KES/KES"}
	if len(got) != len(want) {
		t.Fatalf("amounts: %+v", got)
	}
	for i, g := range got {
		if g.Text+"/"+g.Currency != want[i] {
			t.Fatalf("amount %d: %s/%s want %s", i, g.Text, g.Currency, want[i])
		}
	}
}
