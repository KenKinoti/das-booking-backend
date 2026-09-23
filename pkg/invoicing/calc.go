package invoicing

import (
	"math"
	"time"
)

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Recalculate computes every line and document total. Invoice-level discounts are
// spread across lines in proportion to their net value so tax is always charged
// on the discounted amount. When PricesIncludeTax is set, unit prices are treated
// as tax-inclusive and tax is extracted rather than added.
func Recalculate(inv *Invoice) {
	var net float64
	for i := range inv.Items {
		it := &inv.Items[i]
		if it.Quantity < 0 {
			it.Quantity = 0
		}
		if it.DiscountPercent < 0 {
			it.DiscountPercent = 0
		}
		if it.DiscountPercent > 100 {
			it.DiscountPercent = 100
		}
		it.LineSubtotal = round2(it.Quantity * it.UnitPrice)
		it.LineDiscount = round2(it.LineSubtotal * it.DiscountPercent / 100)
		net += it.LineSubtotal - it.LineDiscount
	}
	net = round2(net)

	docDiscount := 0.0
	if inv.DiscountValue > 0 {
		if inv.DiscountType == "amount" {
			docDiscount = inv.DiscountValue
		} else {
			docDiscount = net * math.Min(inv.DiscountValue, 100) / 100
		}
	}
	if docDiscount > net {
		docDiscount = net
	}
	docDiscount = round2(docDiscount)

	var subtotal, lineDiscounts, tax, allocated float64
	lastIdx := -1
	for i := range inv.Items {
		if inv.Items[i].LineSubtotal-inv.Items[i].LineDiscount != 0 {
			lastIdx = i
		}
	}
	for i := range inv.Items {
		it := &inv.Items[i]
		lineNet := it.LineSubtotal - it.LineDiscount
		share := 0.0
		if docDiscount > 0 && net > 0 {
			if i == lastIdx {
				share = round2(docDiscount - allocated)
			} else {
				share = round2(docDiscount * lineNet / net)
				allocated += share
			}
		}
		taxable := lineNet - share
		if inv.PricesIncludeTax {
			it.LineTax = round2(taxable - taxable/(1+it.TaxRate/100))
			it.LineTotal = round2(lineNet)
		} else {
			it.LineTax = round2(taxable * it.TaxRate / 100)
			it.LineTotal = round2(lineNet)
		}
		subtotal += it.LineSubtotal
		lineDiscounts += it.LineDiscount
		tax += it.LineTax
		it.Position = i + 1
	}

	inv.Subtotal = round2(subtotal)
	inv.DiscountTotal = round2(lineDiscounts + docDiscount)
	inv.TaxTotal = round2(tax)
	if inv.ShippingAmount < 0 {
		inv.ShippingAmount = 0
	}
	if inv.PricesIncludeTax {
		inv.Total = round2(inv.Subtotal - inv.DiscountTotal + inv.ShippingAmount)
	} else {
		inv.Total = round2(inv.Subtotal - inv.DiscountTotal + inv.TaxTotal + inv.ShippingAmount)
	}
	inv.BalanceDue = round2(inv.Total - inv.AmountPaid)
}

// ApplyPayments recomputes AmountPaid/BalanceDue and moves the invoice between
// sent / partial / paid. Drafts that receive a payment become sent/partial/paid.
func ApplyPayments(inv *Invoice, now time.Time) {
	paid := 0.0
	for _, p := range inv.Payments {
		paid += p.Amount
	}
	inv.AmountPaid = round2(paid)
	inv.BalanceDue = round2(inv.Total - inv.AmountPaid)
	if inv.DocType != TypeInvoice || inv.Status == StatusVoid {
		return
	}
	switch {
	case inv.AmountPaid > 0 && inv.BalanceDue <= 0.004:
		inv.Status = StatusPaid
		if inv.PaidAt == nil {
			t := now
			inv.PaidAt = &t
		}
	case inv.AmountPaid > 0:
		inv.Status = StatusPartial
		inv.PaidAt = nil
	default:
		if inv.Status == StatusPaid || inv.Status == StatusPartial {
			inv.Status = StatusSent
		}
		inv.PaidAt = nil
	}
}

// Derive fills DisplayStatus / DaysOverdue.
func Derive(inv *Invoice, today time.Time) {
	inv.DisplayStatus = inv.Status
	inv.DaysOverdue = 0
	due := dateOnly(inv.DueDate)
	t := dateOnly(today)
	if inv.DocType == TypeInvoice && (inv.Status == StatusSent || inv.Status == StatusPartial) && !due.IsZero() && due.Before(t) {
		inv.DisplayStatus = StatusOverdue
		inv.DaysOverdue = int(t.Sub(due).Hours() / 24)
	}
	if inv.DocType == TypeQuote && (inv.Status == StatusSent || inv.Status == StatusDraft) && !due.IsZero() && due.Before(t) {
		inv.DisplayStatus = StatusExpired
	}
}

func dateOnly(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// NextDate advances a date by a recurrence interval.
func NextDate(from time.Time, interval string) time.Time {
	switch interval {
	case "weekly":
		return from.AddDate(0, 0, 7)
	case "fortnightly":
		return from.AddDate(0, 0, 14)
	case "quarterly":
		return from.AddDate(0, 3, 0)
	case "yearly":
		return from.AddDate(1, 0, 0)
	default:
		return from.AddDate(0, 1, 0)
	}
}
