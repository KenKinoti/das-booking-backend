package pnl

import (
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"
)

const dayFmt = "2006-01-02"

func parseDay(s string) time.Time {
	t, err := time.Parse(dayFmt, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}

// loader runs the queries. A failing query (e.g. a module's table does not
// exist yet) is logged and treated as "no records", never as an error.
type loader struct {
	db  *gorm.DB
	org string
}

func (l loader) scan(dest interface{}, q string, args ...interface{}) {
	if err := l.db.Raw(q, args...).Scan(dest).Error; err != nil {
		log.Printf("pnl: query failed: %v", err)
	}
}

// Load reads every record that can fall inside [from, to] (calendar dates in
// loc). Timestamp windows get a day of slack either side; Compute does the
// exact date test in the user's time zone.
func Load(db *gorm.DB, org string, from, to time.Time, loc *time.Location) *Input {
	l := loader{db: db, org: org}
	in := &Input{Loc: loc}
	f, t := from.Format(dayFmt), to.Format(dayFmt)
	tsFrom := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -1)
	tsTo := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 2)

	// Invoices issued in the window, plus the invoices behind payments in it.
	type invRow struct {
		ID, Number, DocType, Status, Currency, IssueDate string
		Total, TaxTotal                                  float64
	}
	var invs []invRow
	l.scan(&invs, `SELECT id, number, doc_type, status, COALESCE(currency,'') AS currency, to_char(issue_date,'YYYY-MM-DD') AS issue_date, total, tax_total
		FROM billing_invoices
		WHERE organization_id = ? AND deleted_at IS NULL AND doc_type IN ('invoice','credit_note')
		  AND ((issue_date >= ?::date AND issue_date <= ?::date)
		    OR id IN (SELECT invoice_id FROM billing_payments WHERE organization_id = ? AND date >= ?::date AND date <= ?::date))`,
		org, f, t, org, f, t)
	for _, r := range invs {
		in.Invoices = append(in.Invoices, Invoice{ID: r.ID, Number: r.Number, DocType: r.DocType, Status: r.Status, Currency: r.Currency,
			IssueDate: parseDay(r.IssueDate), Total: r.Total, Tax: r.TaxTotal})
	}
	var pays []struct {
		InvoiceID, D string
		Amount       float64
	}
	l.scan(&pays, `SELECT invoice_id, to_char(date,'YYYY-MM-DD') AS d, amount FROM billing_payments
		WHERE organization_id = ? AND date >= ?::date AND date <= ?::date`, org, f, t)
	for _, r := range pays {
		in.InvoicePayments = append(in.InvoicePayments, InvoicePayment{InvoiceID: r.InvoiceID, Date: parseDay(r.D), Amount: r.Amount})
	}

	// Point of sale
	var pos []struct {
		TransactionNumber, Type, Status, Currency string
		CreatedAt                                 time.Time
		TotalAmount, TaxAmount                    float64
	}
	l.scan(&pos, `SELECT transaction_number, type, status, created_at, total_amount, tax_amount, COALESCE(currency,'') AS currency FROM pos_transactions
		WHERE organization_id = ? AND deleted_at IS NULL AND created_at >= ? AND created_at < ?`, org, tsFrom, tsTo)
	for _, r := range pos {
		in.POS = append(in.POS, POSTxn{Number: r.TransactionNumber, Type: r.Type, Status: r.Status, Currency: r.Currency, At: r.CreatedAt, Total: r.TotalAmount, Tax: r.TaxAmount})
	}

	// Online store orders
	var online []struct {
		Status, PaymentStatus, Currency string
		PlacedAt                        time.Time
		Total, Tax                      float64
	}
	l.scan(&online, `SELECT status, COALESCE(payment_status,'') AS payment_status, COALESCE(currency,'') AS currency, placed_at, total, tax FROM ecom_orders
		WHERE org_id = ? AND placed_at >= ? AND placed_at < ?`, org, tsFrom, tsTo)
	for _, r := range online {
		in.Online = append(in.Online, OnlineOrder{Status: r.Status, PaymentStatus: r.PaymentStatus, Currency: r.Currency, At: r.PlacedAt, Total: r.Total, Tax: r.Tax})
	}

	// Event tickets
	var regs []struct {
		PaymentStatus, Currency string
		At                      time.Time
		TotalAmount             float64
	}
	l.scan(&regs, `SELECT r.payment_status, COALESCE(e.currency,'') AS currency, COALESCE(r.registration_date, r.created_at) AS at, r.total_amount
		FROM event_registrations r JOIN events e ON e.id = r.event_id AND e.deleted_at IS NULL
		WHERE e.organization_id = ? AND r.deleted_at IS NULL AND COALESCE(r.registration_date, r.created_at) >= ? AND COALESCE(r.registration_date, r.created_at) < ?`,
		org, tsFrom, tsTo)
	for _, r := range regs {
		in.Events = append(in.Events, EventReg{PaymentStatus: r.PaymentStatus, Currency: r.Currency, At: r.At, Amount: r.TotalAmount})
	}

	// Cash book
	var cash []struct {
		ID, Type, Currency, Category, LinkType, Reference, D string
		Amount, Tax                                          int64
	}
	l.scan(&cash, `SELECT e.id, e.type, e.currency, COALESCE(c.name,'') AS category, COALESCE(e.link_type,'') AS link_type, COALESCE(e.reference,'') AS reference,
		  to_char(e.date,'YYYY-MM-DD') AS d, e.amount, e.tax
		FROM smallbiz_entries e LEFT JOIN smallbiz_categories c ON c.id = e.category_id AND c.organization_id = e.organization_id
		WHERE e.organization_id = ? AND e.deleted_at IS NULL AND e.date >= ?::date AND e.date <= ?::date`, org, f, t)
	refs := []string{}
	for _, r := range cash {
		in.Cash = append(in.Cash, CashEntry{ID: r.ID, Type: r.Type, Currency: strings.ToUpper(r.Currency), Category: r.Category, LinkType: r.LinkType,
			Reference: r.Reference, Date: parseDay(r.D), Amount: r.Amount, Tax: r.Tax})
		if ref := strings.ToLower(strings.TrimSpace(r.Reference)); ref != "" {
			refs = append(refs, ref)
		}
	}
	in.InvoiceNumbers, in.BillRefs, in.PONumbers, in.PayRunNumbers = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	if len(refs) > 0 {
		set := func(dst map[string]bool, q string, args ...interface{}) {
			var v []string
			l.scan(&v, q, args...)
			for _, s := range v {
				dst[s] = true
			}
		}
		set(in.InvoiceNumbers, `SELECT LOWER(number) FROM billing_invoices WHERE organization_id = ? AND deleted_at IS NULL AND doc_type = 'invoice' AND LOWER(number) IN ?`, org, refs)
		set(in.BillRefs, `SELECT LOWER(bill_number) FROM bills WHERE organization_id = ? AND LOWER(bill_number) IN ?`, org, refs)
		set(in.BillRefs, `SELECT LOWER(reference) FROM bills WHERE organization_id = ? AND reference <> '' AND LOWER(reference) IN ?`, org, refs)
		set(in.PONumbers, `SELECT LOWER(order_number) FROM purchase_orders WHERE organization_id = ? AND deleted_at IS NULL AND LOWER(order_number) IN ?`, org, refs)
		set(in.PayRunNumbers, `SELECT LOWER(number) FROM hr_pay_runs WHERE org_id = ? AND LOWER(number) IN ?`, org, refs)
		// POS numbers outside the loaded window
		var posNums []string
		l.scan(&posNums, `SELECT transaction_number FROM pos_transactions WHERE organization_id = ? AND deleted_at IS NULL AND LOWER(transaction_number) IN ?`, org, refs)
		for _, n := range posNums {
			in.POS = append(in.POS, POSTxn{Number: n, Status: "reference-only"})
		}
	}

	// Stock received on purchase orders
	var rec []struct {
		POID, PONumber string
		CreatedAt      time.Time
		Value          float64
	}
	l.scan(&rec, `SELECT COALESCE(po.id,'') AS po_id, m.reference AS po_number, m.created_at, m.quantity * m.unit_cost AS value
		FROM inventory_movements m
		LEFT JOIN purchase_orders po ON po.organization_id = m.organization_id AND po.order_number = m.reference AND po.deleted_at IS NULL
		WHERE m.organization_id = ? AND m.reference_type = 'purchase_order' AND m.movement_type = 'in' AND m.created_at >= ? AND m.created_at < ?`, org, tsFrom, tsTo)
	for _, r := range rec {
		in.Receipts = append(in.Receipts, StockReceipt{POID: r.POID, PONumber: r.PONumber, At: r.CreatedAt, Value: r.Value})
	}

	// Supplier bills dated in the window, plus bills paid in it, plus every
	// bill linked to a purchase order (for the count-once rule).
	var bills []struct {
		ID, BillNumber, Reference, Status, PurchaseOrderID, D string
	}
	l.scan(&bills, `SELECT id, bill_number, COALESCE(reference,'') AS reference, status, COALESCE(purchase_order_id,'') AS purchase_order_id,
		  to_char(bill_date AT TIME ZONE 'UTC','YYYY-MM-DD') AS d
		FROM bills
		WHERE organization_id = ? AND ((bill_date >= ?::date AND bill_date < (?::date + 1))
		  OR COALESCE(purchase_order_id,'') <> ''
		  OR id IN (SELECT bill_id FROM payments WHERE organization_id = ? AND type = 'vendor_payment' AND payment_date >= ?::date AND payment_date < (?::date + 1)))`,
		org, f, t, org, f, t)
	if len(bills) > 0 {
		ids := make([]string, len(bills))
		for i, b := range bills {
			ids[i] = b.ID
		}
		var lines []struct {
			BillID, AccountName, AccountType, SubType string
			LineTotal, TaxAmount                      float64
		}
		l.scan(&lines, `SELECT li.bill_id, COALESCE(a.name,'') AS account_name, COALESCE(a.account_type,'Expense') AS account_type, COALESCE(a.sub_type,'') AS sub_type,
			  li.line_total, li.tax_amount
			FROM bill_line_items li LEFT JOIN chart_of_accounts a ON a.id = li.account_id
			WHERE li.bill_id IN ? ORDER BY li.bill_id, li.sort_order`, ids)
		byBill := map[string][]BillLine{}
		for _, r := range lines {
			byBill[r.BillID] = append(byBill[r.BillID], BillLine{AccountName: r.AccountName, AccountType: r.AccountType, SubType: r.SubType, Amount: r.LineTotal, Tax: r.TaxAmount})
		}
		for _, b := range bills {
			in.Bills = append(in.Bills, Bill{ID: b.ID, Number: b.BillNumber, Reference: b.Reference, Status: b.Status, POID: b.PurchaseOrderID,
				Date: parseDay(b.D), Lines: byBill[b.ID]})
		}
	}
	var bpays []struct {
		BillID, D string
		Amount    float64
	}
	l.scan(&bpays, `SELECT bill_id, to_char(payment_date AT TIME ZONE 'UTC','YYYY-MM-DD') AS d, amount FROM payments
		WHERE organization_id = ? AND type = 'vendor_payment' AND bill_id IS NOT NULL AND payment_date >= ?::date AND payment_date < (?::date + 1)`, org, f, t)
	for _, r := range bpays {
		in.BillPayments = append(in.BillPayments, BillPayment{BillID: r.BillID, Date: parseDay(r.D), Amount: r.Amount})
	}

	// Payroll
	var runs []struct {
		ID, Number, Status, Currency, PayDate, PeriodEnd string
		PaidAt                                           *time.Time
		TotalGross, TotalDeductions, TotalNet            float64
	}
	l.scan(&runs, `SELECT id, number, status, currency, COALESCE(pay_date,'') AS pay_date, period_end, paid_at, total_gross, total_deductions, total_net
		FROM hr_pay_runs WHERE org_id = ? AND status = 'paid'
		  AND ((COALESCE(NULLIF(pay_date,''), period_end) >= ? AND COALESCE(NULLIF(pay_date,''), period_end) <= ?) OR (paid_at >= ? AND paid_at < ?))`,
		org, f, t, tsFrom, tsTo)
	for _, r := range runs {
		d := parseDay(r.PayDate)
		if d.IsZero() {
			d = parseDay(r.PeriodEnd)
		}
		in.PayRuns = append(in.PayRuns, PayRun{ID: r.ID, Number: r.Number, Status: r.Status, Currency: strings.ToUpper(r.Currency), PayDate: d, PaidAt: r.PaidAt,
			Gross: r.TotalGross, Deductions: r.TotalDeductions, Net: r.TotalNet})
	}

	// Online payments: processor fees and surcharges
	if db.Migrator().HasTable("pay_transactions") {
		var fees []struct {
			Currency    string
			CompletedAt time.Time
			AppFee      float64
			Surcharge   float64
		}
		l.scan(&fees, `SELECT currency, completed_at, app_fee, surcharge FROM pay_transactions
			WHERE organization_id = ? AND status = 'successful' AND completed_at >= ? AND completed_at < ? AND (app_fee <> 0 OR surcharge <> 0)`, org, tsFrom, tsTo)
		for _, r := range fees {
			in.ProcessorFees = append(in.ProcessorFees, ProcessorFee{Currency: strings.ToUpper(r.Currency), At: r.CompletedAt, Fee: r.AppFee, Surcharge: r.Surcharge})
		}
	}
	return in
}

// OrgSettings returns the organisation's home currency, tax label and
// financial-year start month.
func OrgSettings(db *gorm.DB, org string) (cur, taxLabel string, fyStart int, name string) {
	var s struct {
		Currency, TaxLabel string
	}
	db.Raw("SELECT COALESCE(currency,'') AS currency, COALESCE(tax_label,'') AS tax_label FROM billing_settings WHERE organization_id = ? LIMIT 1", org).Scan(&s)
	var sb struct {
		Currency, TaxName  string
		FinancialYearStart int
		TradingName        string
	}
	db.Raw("SELECT COALESCE(currency,'') AS currency, COALESCE(tax_name,'') AS tax_name, COALESCE(financial_year_start,7) AS financial_year_start, COALESCE(trading_name,'') AS trading_name FROM smallbiz_settings WHERE organization_id = ? LIMIT 1", org).Scan(&sb)
	// The organisation's home currency — the same one the dashboard,
	// invoicing and every other module use (pkg/orgprefs).
	cur = orgprefs.Currency(db, org)
	taxLabel = strings.TrimSpace(s.TaxLabel)
	if taxLabel == "" {
		taxLabel = strings.TrimSpace(sb.TaxName)
	}
	if taxLabel == "" {
		taxLabel = "GST"
	}
	fyStart = sb.FinancialYearStart
	if fyStart < 1 || fyStart > 12 {
		fyStart = 7
	}
	db.Raw("SELECT COALESCE(name,'') FROM organizations WHERE id = ?", org).Scan(&name)
	if sb.TradingName != "" {
		name = sb.TradingName
	}
	return
}

// OtherCurrencies lists currencies (other than home) that have money-bearing
// records in the window.
func OtherCurrencies(in *Input, home string) []string {
	seen := map[string]bool{home: true, "": true}
	out := []string{}
	add := func(c string) {
		c = strings.ToUpper(strings.TrimSpace(c))
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, i := range in.Invoices {
		add(i.Currency)
	}
	for _, e := range in.Cash {
		add(e.Currency)
	}
	for _, o := range in.Online {
		add(o.Currency)
	}
	for _, r := range in.Events {
		add(r.Currency)
	}
	for _, r := range in.PayRuns {
		add(r.Currency)
	}
	return out
}
