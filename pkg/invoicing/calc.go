package invoicing

import (
	"math"
	"strconv"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// roundCur rounds to the currency's minor units (JPY/UGX 0, KWD 3, else 2).
func roundCur(v float64, cur string) float64 {
	p := math.Pow10(currency.Decimals(cur))
	return math.Round(v*p) / p
}

// fmtAmount formats v with the currency's number of decimals.
func fmtAmount(v float64, cur string) string {
	return strconv.FormatFloat(roundCur(v, cur), 'f', currency.Decimals(cur), 64)
}

// Recalculate computes every line and document total. Invoice-level discounts are
// spread across lines in proportion to their net value so tax is always charged
// on the discounted amount. When PricesIncludeTax is set, unit prices are treated
// as tax-inclusive and tax is extracted rather than added.
func Recalculate(inv *Invoice) {
	r := func(v float64) float64 { return roundCur(v, inv.Currency) }
	// Money entered at document level is in the currency's minor units too:
	// an unrounded shipping of 12.345 AUD would print as A$12.35 but add
	// 12.345 to the total, so the printed lines would not add up.
	if inv.ShippingAmount < 0 {
		inv.ShippingAmount = 0
	}
	inv.ShippingAmount = r(inv.ShippingAmount)
	if inv.DiscountType == "amount" {
		inv.DiscountValue = r(inv.DiscountValue)
	}
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
		it.LineSubtotal = r(it.Quantity * it.UnitPrice)
		it.LineDiscount = r(it.LineSubtotal * it.DiscountPercent / 100)
		net += it.LineSubtotal - it.LineDiscount
	}
	net = r(net)

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
	docDiscount = r(docDiscount)

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
				share = r(docDiscount - allocated)
			} else {
				share = r(docDiscount * lineNet / net)
				allocated += share
			}
		}
		taxable := lineNet - share
		if inv.PricesIncludeTax {
			it.LineTax = r(taxable - taxable/(1+it.TaxRate/100))
			it.LineTotal = r(lineNet)
		} else {
			it.LineTax = r(taxable * it.TaxRate / 100)
			it.LineTotal = r(lineNet)
		}
		subtotal += it.LineSubtotal
		lineDiscounts += it.LineDiscount
		tax += it.LineTax
		it.Position = i + 1
	}

	inv.Subtotal = r(subtotal)
	inv.DiscountTotal = r(lineDiscounts + docDiscount)
	inv.TaxTotal = r(tax)
	if inv.PricesIncludeTax {
		inv.Total = r(inv.Subtotal - inv.DiscountTotal + inv.ShippingAmount)
	} else {
		inv.Total = r(inv.Subtotal - inv.DiscountTotal + inv.TaxTotal + inv.ShippingAmount)
	}
	inv.BalanceDue = r(inv.Total - inv.AmountPaid - inv.CreditsApplied - inv.WriteOffAmount)
}

// ApplyPayments recomputes AmountPaid/BalanceDue and moves the invoice between
// sent / partial / paid. Drafts that receive a payment become sent/partial/paid.
func ApplyPayments(inv *Invoice, now time.Time) {
	r := func(v float64) float64 { return roundCur(v, inv.Currency) }
	paid := 0.0
	for _, p := range inv.Payments {
		paid += p.Amount
	}
	inv.AmountPaid = r(paid)
	inv.BalanceDue = r(inv.Total - inv.AmountPaid - inv.CreditsApplied - inv.WriteOffAmount)
	if inv.DocType != TypeInvoice || inv.Status == StatusVoid {
		return
	}
	settled := inv.AmountPaid + inv.CreditsApplied + inv.WriteOffAmount
	switch {
	case settled > 0 && inv.BalanceDue <= 0.4/math.Pow10(currency.Decimals(inv.Currency)):
		inv.Status = StatusPaid
		if inv.PaidAt == nil {
			t := now
			inv.PaidAt = &t
		}
	case settled > 0:
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
