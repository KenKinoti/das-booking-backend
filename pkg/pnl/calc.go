// Package pnl is the single, authoritative Profit & Loss calculation for an
// organisation. It combines every place money comes in or goes out of the
// ERP — invoices, point of sale, online store orders, event tickets, the
// Business hub cash book, supplier bills, purchase orders (stock received)
// and payroll — into one statement, on an accrual or cash basis, with
// explicit de-duplication rules so nothing is counted twice.
//
// The calculation (this file) is pure: it works on plain records loaded by
// load.go, so the rules are unit-tested without a database. handler.go serves
// GET /api/v1/reports/profit-loss; the dashboard, the report page, the
// Finance page, the Business hub, Analytics and the AI tools all read it.
package pnl

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Basis of the statement.
const (
	Accrual = "accrual" // income when invoiced, costs when billed
	Cash    = "cash"    // income when paid, costs when paid
)

// Sections of the statement.
const (
	SecIncome   = "income"
	SecCOS      = "cost_of_sales"
	SecExpenses = "expenses"
)

// Period is an inclusive range of calendar dates (UTC-labelled midnights).
type Period struct {
	From time.Time
	To   time.Time
}

// Contains reports whether calendar date d (UTC-labelled) is in the period.
func (p Period) Contains(d time.Time) bool { return !d.Before(p.From) && !d.After(p.To) }

// Days is the number of calendar days in the period.
func (p Period) Days() int { return int(p.To.Sub(p.From).Hours()/24) + 1 }

// Day returns t's calendar date as a UTC-labelled midnight.
func Day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// LocalDay converts an instant to the calendar date in loc.
func LocalDay(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	return Day(t.In(loc))
}

// ---------- records ----------

// Invoice is an invoicing-module document (billing_invoices).
type Invoice struct {
	ID, Number, DocType, Status, Currency string
	IssueDate                             time.Time // calendar date
	Total, Tax                            float64
}

// InvoicePayment is money received against an invoice (billing_payments).
type InvoicePayment struct {
	InvoiceID string
	Date      time.Time // calendar date
	Amount    float64
}

// POSTxn is a point-of-sale transaction (pos_transactions). Sales record the
// organisation's currency at the time; a blank currency (older sales) means
// the organisation's own currency.
type POSTxn struct {
	Number, Type, Status string // type sale|return|refund, status completed|voided
	Currency             string
	At                   time.Time
	Total, Tax           float64
}

// OnlineOrder is an order imported from a connected online store.
type OnlineOrder struct {
	Status, PaymentStatus, Currency string
	At                              time.Time
	Total, Tax                      float64
}

// EventReg is an event registration (ticket sale).
type EventReg struct {
	PaymentStatus, Currency string
	At                      time.Time
	Amount                  float64
}

// CashEntry is a Business hub cash-book entry. Amount and Tax are minor units.
type CashEntry struct {
	ID, Type, Currency, Category, LinkType, Reference string
	Date                                              time.Time // calendar date
	Amount, Tax                                       int64
}

// StockReceipt is stock booked in against a purchase order (one delivery line).
type StockReceipt struct {
	POID, PONumber string
	At             time.Time
	Value          float64 // quantity × unit cost (ex tax)
}

// Bill is a supplier bill (finance module).
type Bill struct {
	ID, Number, Reference, Status, POID string
	Date                                time.Time // calendar date
	Lines                               []BillLine
}

// BillLine is one line of a bill with its ledger account.
type BillLine struct {
	AccountName, AccountType, SubType string
	Amount, Tax                       float64 // ex-tax line total, tax
}

// BillPayment is a payment made against a bill.
type BillPayment struct {
	BillID string
	Date   time.Time // calendar date
	Amount float64
}

// PayRun is a payroll run (HR module).
type PayRun struct {
	ID, Number, Status, Currency string
	PayDate                      time.Time  // calendar date (zero when unknown)
	PaidAt                       *time.Time // instant it was marked paid
	Gross, Deductions, Net       float64
}

// ProcessorFee is an online payment (pay_transactions): the processor's fee
// (an operating expense) and any surcharge the customer paid on top of the
// invoice amount (income). Counted when the payment completed, on both bases.
type ProcessorFee struct {
	Currency       string
	At             time.Time
	Fee, Surcharge float64
}

// Input is everything the calculation needs.
type Input struct {
	Home     string         // currency the statement is in
	OrgHome  string         // the organisation's own currency (POS, bills and purchase orders trade in it)
	Decimals int            // minor-unit digits of Home
	Loc      *time.Location // the user's time zone, for timestamps

	Invoices        []Invoice
	InvoicePayments []InvoicePayment
	POS             []POSTxn
	Online          []OnlineOrder
	Events          []EventReg
	Cash            []CashEntry
	Receipts        []StockReceipt
	Bills           []Bill
	BillPayments    []BillPayment
	PayRuns         []PayRun
	ProcessorFees   []ProcessorFee

	// Document numbers (lower case) the cash book can refer to, for the
	// reference-matching de-duplication rule. POS numbers come from POS.
	InvoiceNumbers, BillRefs, PONumbers, PayRunNumbers map[string]bool

	prepared   bool
	invByID    map[string]*Invoice
	billByID   map[string]*Bill
	billedPOs  map[string]bool
	posNumbers map[string]bool
}

// Line accumulates one line of the statement (minor units).
type Line struct {
	Key, Section, Label string
	Amount              int64
	Count               int
	Parts               map[string]int64 // sources / categories / sales-refunds breakdown
}

func (l *Line) add(v int64, part string) {
	l.Amount += v
	l.Count++
	if part != "" {
		if l.Parts == nil {
			l.Parts = map[string]int64{}
		}
		l.Parts[part] += v
	}
}

// Excluded records what a rule left out (so people can see why).
type Excluded struct {
	Count  int
	Amount int64
}

// Statement is the result for one period (minor units of Input.Home).
type Statement struct {
	Lines        map[string]*Line
	TaxCollected int64
	TaxPaid      int64
	Excluded     map[string]*Excluded
}

// Totals are the headline figures.
type Totals struct {
	Revenue, CostOfSales, GrossProfit, Expenses, TotalExpenses, NetProfit int64
}

// Totals sums the statement.
func (s *Statement) Totals() Totals {
	var t Totals
	for _, l := range s.Lines {
		switch l.Section {
		case SecIncome:
			t.Revenue += l.Amount
		case SecCOS:
			t.CostOfSales += l.Amount
		case SecExpenses:
			t.Expenses += l.Amount
		}
	}
	t.GrossProfit = t.Revenue - t.CostOfSales
	t.TotalExpenses = t.CostOfSales + t.Expenses
	t.NetProfit = t.GrossProfit - t.Expenses
	return t
}

// SortedLines returns the lines of a section: fixed order for income and cost
// of sales, largest first for operating expenses.
func (s *Statement) SortedLines(section string) []*Line {
	out := []*Line{}
	for _, l := range s.Lines {
		if l.Section == section {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		oi, oj := lineOrder[out[i].Key], lineOrder[out[j].Key]
		if oi != oj {
			return oi < oj
		}
		if out[i].Amount != out[j].Amount {
			return out[i].Amount > out[j].Amount
		}
		return out[i].Label < out[j].Label
	})
	return out
}

var lineOrder = map[string]int{
	"invoices": 1, "pos": 2, "online": 3, "events": 4, "cashbook_income": 5, "surcharges": 6,
	"stock_received": 1, "bills_cos": 2, "cashbook_cos": 3,
}

// ---------- rules ----------

// Excluded keys.
const (
	ExCashLinked   = "cashbook_linked"   // cash-book entry linked to a document counted elsewhere
	ExCashMatched  = "cashbook_matched"  // cash-book reference equals an invoice/bill/PO/pay-run/POS number
	ExCashTransfer = "cashbook_transfer" // transfers, loans, owner money
	ExCashTax      = "cashbook_tax"      // tax payments to the tax office
	ExPOBilled     = "po_billed"         // stock received on a purchase order that has a bill
	ExBillCapital  = "bill_capital"      // bill lines coded to asset / liability / equity accounts
	ExPOSVoided    = "pos_voided"        // voided POS sales
	ExInvoiceVoid  = "invoices_void"     // void invoices
)

// IsCOSCategory reports whether a cash-book category is a cost of sales
// ("Stock / COGS", "Cost of goods sold", "Stock purchases" …).
func IsCOSCategory(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(n, "cogs") || strings.Contains(n, "cost of goods") || strings.Contains(n, "cost of sales") ||
		strings.HasPrefix(n, "stock") || strings.HasPrefix(n, "inventory")
}

// IsTaxCategory reports whether a cash-book expense category records payments
// of tax to the tax office (not an operating expense).
func IsTaxCategory(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == "tax" || n == "taxes" || n == "tax payments"
}

// isStockAccount reports whether a bill line's account means stock bought for
// resale (cost of sales) rather than an operating expense.
func isStockAccount(l BillLine) bool {
	if strings.EqualFold(l.AccountType, "Expense") && strings.EqualFold(strings.TrimSpace(l.SubType), "Cost of Sales") {
		return true
	}
	n := strings.ToLower(l.AccountName)
	return strings.EqualFold(l.AccountType, "Asset") && (strings.Contains(n, "inventory") || strings.Contains(n, "stock"))
}

func (in *Input) prepare() {
	if in.prepared {
		return
	}
	in.prepared = true
	in.invByID = map[string]*Invoice{}
	for i := range in.Invoices {
		in.invByID[in.Invoices[i].ID] = &in.Invoices[i]
	}
	in.billByID = map[string]*Bill{}
	in.billedPOs = map[string]bool{}
	for i := range in.Bills {
		b := &in.Bills[i]
		in.billByID[b.ID] = b
		if b.POID != "" && billCounts(b) {
			in.billedPOs[b.POID] = true
		}
	}
	in.posNumbers = map[string]bool{}
	for _, t := range in.POS {
		if t.Number != "" {
			in.posNumbers[strings.ToLower(t.Number)] = true
		}
	}
}

func billCounts(b *Bill) bool {
	return b.Status != "draft" && b.Status != "void" && b.Status != "cancelled"
}

// inHome reports whether a record in currency c belongs in this statement.
// A blank currency means the organisation's own currency.
func (in *Input) inHome(c string) bool {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		c = in.OrgHome
	}
	return c == in.Home
}

func (in *Input) minor(v float64) int64 {
	return int64(math.Round(v * math.Pow10(in.Decimals)))
}

// share returns round(v × part / whole).
func share(v, part, whole int64) int64 {
	if whole == 0 {
		return v
	}
	return int64(math.Round(float64(v) * float64(part) / float64(whole)))
}

// CashExclusion says why a cash-book entry is left out of profit ("" = counted).
func (in *Input) CashExclusion(e CashEntry) string {
	in.prepare()
	switch strings.ToLower(e.LinkType) {
	case "":
	case "transfer":
		return ExCashTransfer
	default:
		return ExCashLinked
	}
	if ref := strings.ToLower(strings.TrimSpace(e.Reference)); ref != "" {
		if e.Type == "income" && (in.InvoiceNumbers[ref] || in.posNumbers[ref]) {
			return ExCashMatched
		}
		if e.Type == "expense" && (in.BillRefs[ref] || in.PONumbers[ref] || in.PayRunNumbers[ref]) {
			return ExCashMatched
		}
	}
	if e.Type == "expense" && IsTaxCategory(e.Category) {
		return ExCashTax
	}
	return ""
}

// Compute builds the statement for one period on the given basis.
func Compute(in *Input, p Period, basis string) *Statement {
	in.prepare()
	s := &Statement{Lines: map[string]*Line{}, Excluded: map[string]*Excluded{}}
	line := func(key, section, label string) *Line {
		l := s.Lines[key]
		if l == nil {
			l = &Line{Key: key, Section: section, Label: label}
			s.Lines[key] = l
		}
		return l
	}
	exclude := func(key string, v int64) {
		x := s.Excluded[key]
		if x == nil {
			x = &Excluded{}
			s.Excluded[key] = x
		}
		x.Count++
		x.Amount += v
	}
	orgCur := in.Home == in.OrgHome // POS, bills and purchase orders are always in the org currency

	// --- Invoices ---
	if basis == Cash {
		for _, pay := range in.InvoicePayments {
			if !p.Contains(Day(pay.Date)) {
				continue
			}
			inv := in.invByID[pay.InvoiceID]
			if inv == nil || inv.DocType != "invoice" || inv.Status == "void" || !in.inHome(inv.Currency) {
				continue
			}
			amt, total, tax := in.minor(pay.Amount), in.minor(inv.Total), in.minor(inv.Tax)
			ex := share(amt, total-tax, total)
			line("invoices", SecIncome, "Invoice payments received").add(ex, "")
			s.TaxCollected += amt - ex
		}
	} else {
		for _, inv := range in.Invoices {
			if !in.inHome(inv.Currency) || !p.Contains(Day(inv.IssueDate)) {
				continue
			}
			// Credit notes (e.g. imported from Zoho) reduce revenue on the
			// accrual basis by their amount excluding tax.
			if inv.DocType == "credit_note" {
				if inv.Status == "void" || inv.Status == "draft" {
					continue
				}
				total, tax := in.minor(math.Abs(inv.Total)), in.minor(math.Abs(inv.Tax))
				line("invoices", SecIncome, "Invoices issued").add(-(total - tax), "credit notes")
				s.TaxCollected -= tax
				continue
			}
			if inv.DocType != "invoice" {
				continue
			}
			if inv.Status == "void" {
				exclude(ExInvoiceVoid, in.minor(inv.Total))
				continue
			}
			if inv.Status == "draft" {
				continue
			}
			total, tax := in.minor(inv.Total), in.minor(inv.Tax)
			line("invoices", SecIncome, "Invoices issued").add(total-tax, "")
			s.TaxCollected += tax
		}
	}

	// --- Point of sale (paid at the till: same on both bases) ---
	{
		for _, t := range in.POS {
			if !in.inHome(t.Currency) || !p.Contains(LocalDay(t.At, in.Loc)) {
				continue
			}
			total, tax := in.minor(math.Abs(t.Total)), in.minor(math.Abs(t.Tax))
			if t.Status != "completed" {
				if t.Status == "voided" {
					exclude(ExPOSVoided, total)
				}
				continue
			}
			switch t.Type {
			case "return", "refund":
				line("pos", SecIncome, "Point of sale").add(-(total - tax), "refunds")
				s.TaxCollected -= tax
			default:
				line("pos", SecIncome, "Point of sale").add(total-tax, "sales")
				s.TaxCollected += tax
			}
		}
	}

	// --- Online store orders ---
	for _, o := range in.Online {
		if !in.inHome(o.Currency) || !p.Contains(LocalDay(o.At, in.Loc)) {
			continue
		}
		if o.Status != "processing" && o.Status != "completed" {
			continue
		}
		if ps := strings.ToLower(o.PaymentStatus); ps == "unpaid" || ps == "refunded" || ps == "pending" {
			continue
		}
		total, tax := in.minor(o.Total), in.minor(o.Tax)
		line("online", SecIncome, "Online store orders").add(total-tax, "")
		s.TaxCollected += tax
	}

	// --- Event tickets (paid registrations) ---
	for _, r := range in.Events {
		if r.PaymentStatus != "paid" || !in.inHome(r.Currency) || !p.Contains(LocalDay(r.At, in.Loc)) {
			continue
		}
		line("events", SecIncome, "Event tickets").add(in.minor(r.Amount), "")
	}

	// --- Cash book ---
	for _, e := range in.Cash {
		if !in.inHome(e.Currency) || !p.Contains(Day(e.Date)) {
			continue
		}
		if why := in.CashExclusion(e); why != "" {
			exclude(why, e.Amount)
			continue
		}
		net := e.Amount - e.Tax
		cat := strings.TrimSpace(e.Category)
		if cat == "" {
			cat = "Uncategorised"
		}
		if e.Type == "income" {
			line("cashbook_income", SecIncome, "Other income (cash book)").add(net, cat)
			s.TaxCollected += e.Tax
			continue
		}
		s.TaxPaid += e.Tax
		if IsCOSCategory(cat) {
			line("cashbook_cos", SecCOS, "Cash book — stock & cost of sales").add(net, cat)
			continue
		}
		expenseLine(line, cat).add(net, "cashbook")
	}

	// --- Stock received on purchase orders (count once: not when billed) ---
	if orgCur {
		for _, r := range in.Receipts {
			if !p.Contains(LocalDay(r.At, in.Loc)) {
				continue
			}
			v := in.minor(r.Value)
			if in.billedPOs[r.POID] {
				exclude(ExPOBilled, v)
				continue
			}
			line("stock_received", SecCOS, "Stock received (purchase orders)").add(v, "")
		}
	}

	// --- Supplier bills ---
	if orgCur {
		addBill := func(b *Bill, frac func(net, tax int64) (int64, int64)) {
			for _, l := range b.Lines {
				net, tax := frac(in.minor(l.Amount), in.minor(l.Tax))
				s.TaxPaid += tax
				switch {
				case b.POID != "":
					line("bills_cos", SecCOS, "Supplier bills — stock & cost of sales").add(net, "Bills for purchase orders")
				case isStockAccount(l):
					line("bills_cos", SecCOS, "Supplier bills — stock & cost of sales").add(net, l.AccountName)
				case strings.EqualFold(l.AccountType, "Expense"):
					expenseLine(line, l.AccountName).add(net, "bills")
				default:
					exclude(ExBillCapital, net)
				}
			}
		}
		if basis == Cash {
			for _, pay := range in.BillPayments {
				b := in.billByID[pay.BillID]
				if b == nil || !billCounts(b) || !p.Contains(Day(pay.Date)) {
					continue
				}
				var total int64
				for _, l := range b.Lines {
					total += in.minor(l.Amount) + in.minor(l.Tax)
				}
				amt := in.minor(pay.Amount)
				addBill(b, func(net, tax int64) (int64, int64) { return share(amt, net, total), share(amt, tax, total) })
			}
		} else {
			for i := range in.Bills {
				b := &in.Bills[i]
				if !billCounts(b) || !p.Contains(Day(b.Date)) {
					continue
				}
				addBill(b, func(net, tax int64) (int64, int64) { return net, tax })
			}
		}
	}

	// --- Online payment processing (Flutterwave): fees and surcharges ---
	for _, f := range in.ProcessorFees {
		if !in.inHome(f.Currency) || !p.Contains(LocalDay(f.At, in.Loc)) {
			continue
		}
		if v := in.minor(f.Fee); v != 0 {
			line("processing_fees", SecExpenses, "Payment processing fees").add(v, "flutterwave")
		}
		if v := in.minor(f.Surcharge); v != 0 {
			line("surcharges", SecIncome, "Payment surcharges collected").add(v, "flutterwave")
		}
	}

	// --- Payroll (paid pay runs; gross wages incl. amounts withheld) ---
	for _, r := range in.PayRuns {
		if r.Status != "paid" || !in.inHome(r.Currency) {
			continue
		}
		d := r.PayDate
		if basis == Cash && r.PaidAt != nil {
			d = LocalDay(*r.PaidAt, in.Loc)
		}
		if d.IsZero() && r.PaidAt != nil {
			d = LocalDay(*r.PaidAt, in.Loc)
		}
		if d.IsZero() || !p.Contains(Day(d)) {
			continue
		}
		l := line("payroll", SecExpenses, "Wages & salaries (payroll)")
		l.add(in.minor(r.Gross), "")
		if l.Parts == nil {
			l.Parts = map[string]int64{}
		}
		l.Parts["deductions"] += in.minor(r.Deductions)
		l.Parts["net"] += in.minor(r.Net)
	}
	return s
}

// expenseLine returns the operating-expense line for a category name; bills
// and cash-book entries with the same category name share a line.
func expenseLine(line func(key, section, label string) *Line, name string) *Line {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Other expenses"
	}
	return line("exp:"+strings.ToLower(name), SecExpenses, name)
}
