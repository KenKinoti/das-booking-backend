package invoicing

import (
	"testing"
	"time"
)

func TestRecalculateBasic(t *testing.T) {
	inv := &Invoice{DocType: TypeInvoice, Items: []InvoiceItem{
		{Quantity: 2, UnitPrice: 100, TaxRate: 10},
		{Quantity: 1, UnitPrice: 50.5, TaxRate: 0},
	}}
	Recalculate(inv)
	if inv.Subtotal != 250.5 || inv.TaxTotal != 20 || inv.Total != 270.5 {
		t.Fatalf("unexpected totals: %+v", inv)
	}
}

func TestRecalculateDiscounts(t *testing.T) {
	inv := &Invoice{DocType: TypeInvoice, DiscountType: "percent", DiscountValue: 10, ShippingAmount: 15, Items: []InvoiceItem{
		{Quantity: 1, UnitPrice: 200, DiscountPercent: 50, TaxRate: 10}, // net 100
		{Quantity: 4, UnitPrice: 25, TaxRate: 10},                       // net 100
	}}
	Recalculate(inv)
	// net 200, doc discount 20 => taxable 180, tax 18
	if inv.DiscountTotal != 120 || inv.TaxTotal != 18 || inv.Total != 300-120+18+15 {
		t.Fatalf("unexpected totals: sub=%v disc=%v tax=%v total=%v", inv.Subtotal, inv.DiscountTotal, inv.TaxTotal, inv.Total)
	}
}

func TestFixedDiscountCappedAndRoundingAllocation(t *testing.T) {
	inv := &Invoice{DiscountType: "amount", DiscountValue: 10, Items: []InvoiceItem{
		{Quantity: 1, UnitPrice: 3.33, TaxRate: 10},
		{Quantity: 1, UnitPrice: 3.33, TaxRate: 10},
		{Quantity: 1, UnitPrice: 3.34, TaxRate: 10},
	}}
	Recalculate(inv)
	if inv.Total != 0 || inv.TaxTotal != 0 {
		t.Fatalf("discount should zero the invoice: %+v", inv)
	}
	inv.DiscountValue = 1000
	Recalculate(inv)
	if inv.DiscountTotal != 10 {
		t.Fatalf("discount must be capped at net: %v", inv.DiscountTotal)
	}
}

func TestTaxInclusive(t *testing.T) {
	inv := &Invoice{PricesIncludeTax: true, Items: []InvoiceItem{{Quantity: 1, UnitPrice: 110, TaxRate: 10}}}
	Recalculate(inv)
	if inv.Total != 110 || inv.TaxTotal != 10 {
		t.Fatalf("inclusive tax wrong: total=%v tax=%v", inv.Total, inv.TaxTotal)
	}
}

func TestPaymentsStatusFlow(t *testing.T) {
	now := time.Now()
	inv := &Invoice{DocType: TypeInvoice, Status: StatusSent, Items: []InvoiceItem{{Quantity: 1, UnitPrice: 100}}}
	Recalculate(inv)
	inv.Payments = []Payment{{Amount: 40}}
	ApplyPayments(inv, now)
	if inv.Status != StatusPartial || inv.BalanceDue != 60 {
		t.Fatalf("expected partial: %s %v", inv.Status, inv.BalanceDue)
	}
	inv.Payments = append(inv.Payments, Payment{Amount: 60})
	ApplyPayments(inv, now)
	if inv.Status != StatusPaid || inv.PaidAt == nil {
		t.Fatalf("expected paid: %s", inv.Status)
	}
	inv.Payments = nil
	ApplyPayments(inv, now)
	if inv.Status != StatusSent || inv.PaidAt != nil {
		t.Fatalf("expected back to sent: %s", inv.Status)
	}
}

func TestDeriveOverdue(t *testing.T) {
	today := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	inv := &Invoice{DocType: TypeInvoice, Status: StatusSent, DueDate: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	Derive(inv, today)
	if inv.DisplayStatus != StatusOverdue || inv.DaysOverdue != 9 {
		t.Fatalf("expected overdue 9 days, got %s %d", inv.DisplayStatus, inv.DaysOverdue)
	}
	inv.Status = StatusPaid
	Derive(inv, today)
	if inv.DisplayStatus != StatusPaid {
		t.Fatal("paid invoices are never overdue")
	}
}

func TestNextDate(t *testing.T) {
	d := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	if NextDate(d, "weekly").Day() != 7 || NextDate(d, "yearly").Year() != 2027 {
		t.Fatal("bad next date")
	}
}

func TestRecalculateCurrencyDecimals(t *testing.T) {
	jpy := &Invoice{Currency: "JPY", Items: []InvoiceItem{{Quantity: 1, UnitPrice: 1234, TaxRate: 10}}}
	Recalculate(jpy)
	if jpy.TaxTotal != 123 || jpy.Total != 1357 {
		t.Fatalf("JPY should round to whole yen: tax=%v total=%v", jpy.TaxTotal, jpy.Total)
	}
	kwd := &Invoice{Currency: "KWD", Items: []InvoiceItem{{Quantity: 1, UnitPrice: 1.2346, TaxRate: 5}}}
	Recalculate(kwd)
	if kwd.Subtotal != 1.235 || kwd.TaxTotal != 0.062 || kwd.Total != 1.297 {
		t.Fatalf("KWD should keep 3 decimals: %+v", kwd)
	}
	if fmtAmount(1357, "JPY") != "1357" || fmtAmount(1.2965, "KWD") != "1.297" || fmtAmount(3.1, "KES") != "3.10" {
		t.Fatal("fmtAmount decimals wrong")
	}
}
