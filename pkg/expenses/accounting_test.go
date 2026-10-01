package expenses

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
)

func newOrgIn(t *testing.T, db *gorm.DB, country, cur, tz string) string {
	org := uuid.NewString()
	db.Exec(`INSERT INTO organizations (id, name, country_code, currency, timezone, region_source) VALUES (?, 'Kenya Co', ?, ?, ?, 'user')`, org, country, cur, tz)
	return org
}

func synItems(db *gorm.DB, org, source, status string) []Item {
	var l []Item
	q := db.Where("organization_id = ? AND source = ?", org, source)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	q.Order("invoice_date").Find(&l)
	return l
}

func sumBills(t *testing.T, s *Service, org string, items []Item) float64 {
	t.Helper()
	var tot float64
	for _, it := range items {
		b, err := s.Fin.GetBill(org, it.BillID)
		if err != nil {
			t.Fatalf("bill of %s: %v", it.InvoiceNumber, err)
		}
		if b.Status != "paid" {
			t.Fatalf("bill %s not paid: %s", b.BillNumber, b.Status)
		}
		tot += b.Total
	}
	return r2(tot)
}

// Kenyan business, KES home: both accounting modes, never both, idempotent,
// GST not claimable, conversions at each month's rate.
func TestSynergyAccountingModesKES(t *testing.T) {
	db := testDB(t)
	NotifyAfterUpload = false
	org := newOrgIn(t, db, "KE", "KES", "Africa/Nairobi")
	s := NewService(db, &fxrates.Service{DB: db})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	// Rates the user set for August and September (KES per AUD).
	for m, r := range map[string]float64{"2026-08": 84.0, "2026-09": 85.5} {
		row := FxMonth{ID: uuid.NewString(), OrganizationID: org, FromCurrency: "AUD", ToCurrency: "KES", Month: m, Rate: r, Source: fxrates.SourceManual, Manual: true, AsOf: &now}
		db.Create(&row)
	}

	uploadFixture(t, s, org, fxAug, nil)
	sep := uploadFixture(t, s, org, fxSep, nil)
	if sep.Accounting == nil || sep.Accounting.Mode != ModeCharges {
		t.Fatalf("upload did not apply the accounting: %+v", sep.Accounting)
	}
	// Charges (default): 4 approved month × category expenses, AUD 237.98.
	st := synItems(db, org, synStatementSource, StatusApproved)
	if len(st) != 4 {
		t.Fatalf("statement expenses %d", len(st))
	}
	var aud, kes float64
	for _, it := range st {
		aud += it.Total
		kes += it.HomeTotal
		if it.Tax != 0 || !strings.Contains(it.TaxNote, "not claimable") {
			t.Fatalf("GST should not be claimed by a Kenyan business: %+v", it)
		}
		want := map[string]float64{"2026-08": 84.0, "2026-09": 85.5}[monthOf(*it.InvoiceDate)]
		if !near(it.FxRate, want) || it.FxBasis != FxBasisManual || it.HomeCurrency != "KES" || !strings.Contains(it.FxNote, "1 AUD = ") {
			t.Fatalf("rate of the month: %s %.4f %s %q", it.ExternalID, it.FxRate, it.FxBasis, it.FxNote)
		}
		if d := it.HomeTotal - fxrates.Convert(it.Total, want, "KES"); d > 0.1 || d < -0.1 { // bill lines round one by one
			t.Fatalf("home total %s %.2f", it.ExternalID, it.HomeTotal)
		}
	}
	if !near(aud, 237.98) || !near(sumBills(t, s, org, st), r2(kes)) {
		t.Fatalf("charges total AUD %.2f, KES %.2f", aud, kes)
	}
	// Idempotent: applying again changes nothing.
	res, err := s.ApplySynergyAccounting(org, "u1")
	if err != nil || res.Created != 0 || res.Updated != 0 || res.Removed != 0 || res.Unchanged != 4 || !near(res.TotalAUD, 237.98) {
		t.Fatalf("re-apply %+v %v", res, err)
	}
	uploadFixture(t, s, org, fxSep, nil)
	if n := len(synItems(db, org, synStatementSource, "")); n != 4 {
		t.Fatalf("re-upload duplicated: %d", n)
	}

	// Top-ups: the three refills (A$222.71, all September) — charges removed.
	mode := ModeTopups
	res, err = s.SaveAccounting(org, "u1", AccountingInput{Mode: &mode})
	if err != nil || len(res.Errors) != 0 || res.Created != 3 || res.Approved != 3 || res.Removed != 4 {
		t.Fatalf("topups %+v %v", res, err)
	}
	if n := len(synItems(db, org, synStatementSource, "")); n != 0 {
		t.Fatalf("charges still counted: %d", n)
	}
	tops := synItems(db, org, synTopupSource, StatusApproved)
	var ta float64
	for _, it := range tops {
		ta += it.Total
		if it.Category != CatPrepaid || !near(it.FxRate, 85.5) || it.InvoiceDate.Format("2006-01") != "2026-09" {
			t.Fatalf("top-up %+v", it)
		}
	}
	if len(tops) != 3 || !near(ta, 222.71) || !near(sumBills(t, s, org, tops), fxrates.Convert(222.71, 85.5, "KES")) {
		t.Fatalf("top-ups %d %.2f", len(tops), ta)
	}
	if res, _ = s.ApplySynergyAccounting(org, "u1"); res.Created != 0 || res.Unchanged != 3 {
		t.Fatalf("top-ups re-apply %+v", res)
	}
	// Reconciliation: top-ups − charges = balance change, per month and in total.
	v := s.Accounting(org)
	if v.ReconTotal == nil || !v.ReconTotal.OK || !near(v.ReconTotal.Topups, 222.71) || !near(v.ReconTotal.Charges, 237.98) || !near(v.ReconTotal.Closing, 10.47) {
		t.Fatalf("reconciliation %+v", v.ReconTotal)
	}
	for _, r := range v.Reconciliation {
		if !r.OK {
			t.Fatalf("month %s does not reconcile: %+v", r.Month, r)
		}
	}
	if len(v.Counted) != 1 || v.Counted[0].Month != "2026-09" || !near(v.CountedAUD, 222.71) {
		t.Fatalf("counted %+v", v.Counted)
	}

	// Back to charges: the top-up expenses go, the charges come back.
	mode = ModeCharges
	if res, err = s.SaveAccounting(org, "u1", AccountingInput{Mode: &mode}); err != nil || res.Created != 4 || res.Removed != 3 {
		t.Fatalf("back to charges %+v %v", res, err)
	}
	if len(synItems(db, org, synTopupSource, "")) != 0 || len(synItems(db, org, synStatementSource, StatusApproved)) != 4 {
		t.Fatal("never both")
	}

	// A new manual rate for August re-converts August only.
	if _, err := s.SetMonthRate(org, "u1", MonthRateInput{From: "AUD", To: "KES", Month: "2026-08", Rate: 90}); err != nil {
		t.Fatal(err)
	}
	for _, it := range synItems(db, org, synStatementSource, StatusApproved) {
		want := map[string]float64{"2026-08": 90, "2026-09": 85.5}[monthOf(*it.InvoiceDate)]
		if !near(it.FxRate, want) {
			t.Fatalf("after rate change %s %.2f", it.ExternalID, it.FxRate)
		}
	}

	// Auto-approve off: new expenses wait in the inbox.
	off := false
	mode = ModeTopups
	if res, err = s.SaveAccounting(org, "u1", AccountingInput{Mode: &mode, AutoApprove: &off}); err != nil || res.Approved != 0 || res.Created != 3 {
		t.Fatalf("auto-approve off %+v %v", res, err)
	}
	if n := len(synItems(db, org, synTopupSource, StatusPending)); n != 3 {
		t.Fatalf("pending top-ups %d", n)
	}
}

// Australian business registered for GST: 1/11 claimable; switching the
// setting off re-books the full amount as the cost.
func TestSynergyGSTSetting(t *testing.T) {
	db := testDB(t)
	NotifyAfterUpload = false
	org := newOrgIn(t, db, "AU", "AUD", "Australia/Sydney")
	s := NewService(db, &fxrates.Service{DB: db})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	uploadFixture(t, s, org, fxSep, nil)
	if v := s.Accounting(org); !v.AUGSTRegistered || !v.AUGSTDefault {
		t.Fatalf("AU org should default to GST registered: %+v", v)
	}
	for _, it := range synItems(db, org, synStatementSource, StatusApproved) {
		if !near(it.Tax, r2(it.Total/11)) || it.FxRate != 1 {
			t.Fatalf("GST 1/11: %+v", it)
		}
		b, _ := s.Fin.GetBill(org, it.BillID)
		if !near(b.Total, it.Total) || b.TaxAmount < it.Tax-0.05 {
			t.Fatalf("bill %+v", b)
		}
	}
	no := false
	res, err := s.SaveAccounting(org, "u1", AccountingInput{AUGSTRegistered: &no})
	if err != nil || res.Updated != 2 || len(res.Errors) != 0 {
		t.Fatalf("GST off %+v %v", res, err)
	}
	if n := len(synItems(db, org, synStatementSource, StatusApproved)); n != 2 {
		t.Fatalf("re-approved %d", n)
	}
	for _, it := range synItems(db, org, synStatementSource, StatusApproved) {
		b, _ := s.Fin.GetBill(org, it.BillID)
		if it.Tax != 0 || b.TaxAmount != 0 || !near(b.Total, it.Total) {
			t.Fatalf("GST off: item %+v bill %+v", it, b)
		}
	}
}

// Statements uploaded before the upgrade get their expenses on first load,
// with a one-time notice; month rates that were never recorded are labelled.
func TestSynergyBackfillNotice(t *testing.T) {
	db := testDB(t)
	NotifyAfterUpload = false
	org := newOrgIn(t, db, "KE", "KES", "Africa/Nairobi")
	s := NewService(db, &fxrates.Service{DB: db})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	uploadFixture(t, s, org, fxAug, nil)
	uploadFixture(t, s, org, fxSep, nil)
	// Simulate the pre-upgrade state: statements stored, no expenses, no settings.
	for _, it := range synItems(db, org, synStatementSource, "") {
		if _, err := s.Unapprove(org, "u1", it.ID); err != nil {
			t.Fatal(err)
		}
	}
	db.Where("organization_id = ? AND source = ?", org, synStatementSource).Delete(&Item{})
	db.Where("organization_id = ?", org).Delete(&AccountingPrefs{})
	db.Where("organization_id = ?", org).Delete(&FxMonth{})
	var bills int64
	db.Model(&finance.Bill{}).Where("organization_id = ?", org).Count(&bills)
	if bills != 0 {
		t.Fatalf("bills left %d", bills)
	}

	s.EnsureSynergyBackfill(org)
	st := synItems(db, org, synStatementSource, StatusApproved)
	if len(st) != 4 {
		t.Fatalf("backfill created %d", len(st))
	}
	v := s.Accounting(org)
	if !strings.HasPrefix(v.Notice, "Synergy expenses for Aug–Sep 2026 added: A$237.98 (KES ") || !strings.Contains(v.Notice, "approved") {
		t.Fatalf("notice %q", v.Notice)
	}
	for _, it := range st {
		if it.FxBasis != FxBasisBackfilled || !strings.Contains(it.FxNote, "none was recorded in") {
			t.Fatalf("backfilled rate should be labelled: %s %q", it.FxBasis, it.FxNote)
		}
	}
	// Once only; the notice can be dismissed.
	s.EnsureSynergyBackfill(org)
	if n := len(synItems(db, org, synStatementSource, "")); n != 4 {
		t.Fatalf("second backfill: %d", n)
	}
	s.DismissAccountingNotice(org)
	if s.Accounting(org).Notice != "" {
		t.Fatal("notice not dismissed")
	}
	// The current month's rate is recorded as the month goes and stays once it is over.
	r1 := s.RateFor(org, "AUD", "KES", now)
	if r1.Basis != FxBasisMonth || r1.Month != "2026-10" {
		t.Fatalf("current month rate %+v", r1)
	}
	later := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return later }
	if r2 := s.RateFor(org, "AUD", "KES", now); r2.Basis != FxBasisMonth || !near(r2.Rate, r1.Rate) || !strings.Contains(r2.Label, "Oct 2026 rate") {
		t.Fatalf("closed month rate %+v", r2)
	}
}

// Gmail: Synergy statement and usage CSVs attached to emails go to the
// Synergy importer, and the statement expenses are created and approved.
func TestGmailRoutesSynergyFiles(t *testing.T) {
	db := testDB(t)
	NotifyAfterUpload = false
	fg := setupGoogle(t)
	day := func(s string) time.Time { d, _ := time.Parse("2006-01-02 15:04", s); return d }
	sy := "Synergy Wholesale <noreply@synergywholesale.com>"
	fg.msgs = map[string]*Message{
		"st-aug": gmailMsg("st-aug", sy, "Your monthly transaction statement for August", day("2026-09-01 02:00"), synergyStatementEmail,
			[3]string{"statement-August-2026.csv", "application/csv", "a-aug"}),
		"st-sep": gmailMsg("st-sep", sy, "Your monthly transaction statement for September", day("2026-10-01 02:00"), synergyStatementEmail,
			[3]string{"statement-September-2026.csv", "text/csv", "a-sep"}),
		"use": gmailMsg("use", sy, "Hosting account usage", day("2026-10-01 03:00"), "Your hosting usage report is attached.",
			[3]string{"hosting-account-usage-2026-10-01.csv", "text/csv", "a-use"}),
	}
	fg.atts = map[string][]byte{"a-aug": fixture(t, fxAug), "a-sep": fixture(t, fxSep), "a-use": fixture(t, fxUse10)}
	org := newOrgIn(t, db, "KE", "KES", "Africa/Nairobi")
	s := NewService(db, &fxrates.Service{DB: db})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := s.SaveGmail(org, GmailInput{Presets: []string{"google_workspace", "synergy"}}); err != nil {
		t.Fatal(err)
	}
	s.StartGmail(org, "u1", "x", "")
	if _, err := s.FinishGmail(ctx, s.connection(org, "gmail").State, "good-code", "x"); err != nil {
		t.Fatal(err)
	}
	if sc := s.connection(org, "schedule"); sc == nil || !sc.Enabled || !sc.Settings.AutoSync {
		t.Fatalf("connecting Gmail should turn the daily sync on: %+v", sc)
	}
	r := s.SyncGmail(ctx, org)
	if !r.OK || r.SynergyStatements != 2 || r.SynergyUsage != 1 {
		t.Fatalf("sync %+v", r)
	}
	var n int64
	db.Model(&SynergyTxn{}).Where("organization_id = ?", org).Count(&n)
	if n != 50 {
		t.Fatalf("statement lines %d", n)
	}
	db.Model(&SynergyUsage{}).Where("organization_id = ?", org).Count(&n)
	if n != 11 {
		t.Fatalf("usage rows %d", n)
	}
	if st := synItems(db, org, synStatementSource, StatusApproved); len(st) != 4 {
		t.Fatalf("statement expenses %d", len(st))
	}
	// Second sync: nothing new.
	r = s.SyncGmail(ctx, org)
	if !r.OK || r.SynergyStatements != 0 || len(synItems(db, org, synStatementSource, "")) != 4 {
		t.Fatalf("second sync %+v", r)
	}
	a := s.AutomationFor(org)
	if !a.DailySync || len(a.Sources) != 4 || a.Sources[1].Level != "automatic" || a.Sources[0].Level != "automatic" {
		t.Fatalf("automation %+v", a)
	}
}

// Vendor rule: Google Workspace invoices within ±20% of the last approved
// one are approved automatically.
func TestAutoApproveRule(t *testing.T) {
	db := testDB(t)
	org := newOrgIn(t, db, "KE", "KES", "Africa/Nairobi")
	s := NewService(db, &fxrates.Service{DB: db})
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	gw := s.vendorByKey(org, "google_workspace")
	if !gw.AutoApprove || gw.AutoApprovePct != 20 {
		t.Fatalf("default rule %+v", gw)
	}
	mk := func(id string, d time.Time, total float64) *Item {
		it, _, err := s.Ingest(org, Incoming{Source: SourceGmail, ExternalID: id, VendorID: gw.ID, X: Extracted{Kind: KindInvoice, Total: total, Tax: 0, Currency: "EUR", Date: &d, InvoiceNumber: id}})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	a := mk("inv-1", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 36)
	if s.AutoApproveItems(org, []string{a.ID}) != 0 {
		t.Fatal("the first invoice must not be approved automatically")
	}
	if it, _ := s.Get(org, a.ID); it.Status != StatusPending || !strings.Contains(strings.Join(it.Warnings, " "), "first Google Workspace invoice") {
		t.Fatalf("first %+v", it)
	}
	if _, err := s.Approve(org, "u1", a.ID, ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	b := mk("inv-2", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 43.2) // +20%
	c := mk("inv-3", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 60)   // +39% vs 43.20
	if n := s.AutoApproveItems(org, []string{b.ID, c.ID}); n != 1 {
		t.Fatalf("auto-approved %d", n)
	}
	if it, _ := s.Get(org, b.ID); it.Status != StatusApproved || it.HomeCurrency != "KES" || it.FxRate <= 0 || !strings.Contains(it.Notes, "Approved automatically") {
		t.Fatalf("within rule %+v", it)
	}
	if it, _ := s.Get(org, c.ID); it.Status != StatusPending || !strings.Contains(strings.Join(it.Warnings, " "), "+39%") {
		t.Fatalf("outside rule %+v", it)
	}
	// EUR → KES at the invoice month's rate.
	if it, _ := s.Get(org, b.ID); it.FxBasis != FxBasisBackfilled || !strings.Contains(it.FxNote, "1 EUR = ") {
		t.Fatalf("EUR rate %+v", it)
	}
}

func TestGoogleInvoicePDFTotals(t *testing.T) {
	x, _, _ := extractDoc(context.Background(), googleInvoiceEmail+"\n"+PDFText(makePDF(googleInvoicePDFLines, true)), nil, "", Hints{Subject: "Google Workspace: Your invoice is available for example.com.au", Sender: "payments-noreply@google.com"})
	if !near(x.Total, 36) || x.Currency != "EUR" || !near(x.Tax, 6) || x.InvoiceNumber != "5671877634" {
		t.Fatalf("google pdf %+v", x)
	}
}

func TestPeriodMonthsAndRateLabel(t *testing.T) {
	for in, want := range map[string]int{"yearly": 12, "1 Year/s": 12, "6 months": 6, "monthly": 1, "": 1} {
		if got := periodMonths(in, "hosting"); got != want {
			t.Errorf("%q → %d, want %d", in, got, want)
		}
	}
	if periodMonths("", "domain") != 12 {
		t.Error("domains renew yearly")
	}
	d := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	l := rateLabel(RateInfo{From: "AUD", To: "KES", Rate: 85.4, Source: fxrates.SourceLive, Basis: FxBasisToday, AsOf: &d})
	if l != "1 AUD = 85.4 KES · live · today (1 Oct 2026)" {
		t.Fatalf("label %q", l)
	}
	if monthSpan([]string{"2026-09", "2026-08"}) != "Aug–Sep 2026" {
		t.Fatal(monthSpan([]string{"2026-09", "2026-08"}))
	}
}
