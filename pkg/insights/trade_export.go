package insights

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// CSV exports of the trade & exports view (the same figures as the page).

// TradeDatasets are the downloadable tables.
var TradeDatasets = []struct{ Key, Title string }{
	{"countries", "Revenue by client country"},
	{"months", "Domestic vs export revenue by month"},
	{"currencies", "Revenue by currency"},
	{"export_clients", "Top export clients"},
	{"receivables", "Open foreign-currency receivables and unrealised FX"},
	{"realised", "Realised FX on paid foreign invoices"},
	{"costs", "Costs in foreign currencies"},
	{"hedge", "Natural hedge by currency"},
	{"documents", "Documents behind the figures"},
	{"markets", "Target markets"},
}

// TradeDataset reports whether key is a trade export.
func TradeDataset(key string) bool {
	for _, d := range TradeDatasets {
		if d.Key == key {
			return true
		}
	}
	return false
}

func cp(v *float64) string {
	if v == nil {
		return ""
	}
	return cell(*v)
}

// WriteTradeCSV writes one table of the trade view.
func WriteTradeCSV(w io.Writer, key string, t *Trade, docs []TradeDoc, m *Markets) error {
	cw := csv.NewWriter(w)
	row := func(v ...interface{}) {
		rec := make([]string, len(v))
		for k, x := range v {
			if s, ok := x.(string); ok {
				rec[k] = safeCSV(s)
			} else {
				rec[k] = cell(x)
			}
		}
		_ = cw.Write(rec)
	}
	h := ""
	if t != nil {
		h = strings.ToLower(t.Currency)
	} else if m != nil {
		h = strings.ToLower(m.Currency)
	}
	switch key {
	case "countries":
		row("country", "country_name", "domestic", "revenue_"+h, "share_pct", "previous_year_"+h, "yoy_pct", "invoices", "clients", "avg_invoice_"+h, "avg_days_to_pay", "paid_invoices", "docs_country_inferred")
		for _, c := range t.Countries {
			row(c.Country, c.Name, c.Domestic, c.Revenue, c.SharePct, c.Prev, cp(c.YoY), c.Invoices, c.Clients, c.AvgInvoice, cp(c.AvgDaysToPay), c.PaidInvoices, c.Inferred)
		}
	case "months":
		row("month", "domestic_"+h, "export_"+h, "export_pct")
		for _, x := range t.Months {
			row(x.Month, x.Domestic, x.Export, cp(x.ExportPct))
		}
	case "currencies":
		row("currency", "amount", "amount_"+h, "share_pct", "documents")
		for _, c := range t.Currencies {
			row(c.Currency, c.Amount, c.Home, c.SharePct, c.Docs)
		}
	case "export_clients":
		row("client", "customer_id", "country", "country_name", "currencies", "revenue_"+h, "share_of_export_pct", "invoices")
		for _, c := range t.TopExport {
			row(c.Name, c.CustomerID, c.Country, c.CountryNm, c.Currencies, c.Revenue, c.SharePct, c.Invoices)
		}
	case "receivables":
		row("currency", "open_invoices", "balance", "balance_with_locked_rate", "value_at_invoice_rate_"+h, "today_rate", "value_at_today_rate_"+h, "unrealised_fx_"+h, "unrealised_pct", "avg_locked_rate", "invoices_without_locked_rate", "overdue", "rate_source")
		for _, r := range t.Receivables.Rows {
			row(r.Currency, r.Invoices, r.Balance, r.LockedBalance, r.LockedHome, r.TodayRate, cp(r.TodayHome), cp(r.Unrealised), cp(r.UnrealisedPct), cp(r.AvgLockedRate), r.NoLocked, r.Overdue, r.RateSource)
		}
	case "realised":
		row("currency", "payments", "amount", "value_at_invoice_rate_"+h, "value_at_payment_rate_"+h, "realised_fx_"+h, "payments_without_month_rate")
		for _, r := range t.Realised.Rows {
			row(r.Currency, r.Payments, r.Amount, r.AtInvoice, r.AtPayment, r.Gain, r.Skipped)
		}
	case "costs":
		row("currency", "supplier", "amount", "amount_"+h, "expenses")
		for _, c := range t.Costs {
			for _, v := range c.Vendors {
				row(c.Currency, v.Name, v.Amount, v.Home, v.Items)
			}
		}
	case "hedge":
		row("currency", "earned", "spent", "net", "cover_pct", "today_rate", "net_"+h, "five_pct_move_"+h, "summary")
		for _, x := range t.Hedge {
			row(x.Currency, x.In, x.Out, x.Net, cp(x.CoverPct), x.Rate, cp(x.NetHome), cp(x.Move5Home), x.Summary)
		}
	case "documents":
		row("number", "type", "status", "date", "due", "client", "customer_id", "country", "country_basis", "currency", "net", "net_"+h, "balance", "locked_rate", "balance_at_today_rate_"+h, "unrealised_fx_"+h, "invoice_id")
		for _, d := range docs {
			row(d.Number, d.DocType, d.Status, d.Date, d.Due, d.Client, d.CustomerID, d.Country, d.Basis, d.Currency, d.Net, d.NetHome, d.Balance, cp(d.LockedRate), cp(d.TodayHome), cp(d.Unrealised), d.ID)
		}
	case "markets":
		head := []interface{}{"rank", "country", "name", "currency", "score", "factors_used"}
		for _, in := range m.Indicators {
			head = append(head, in.Code, in.Code+"_year")
		}
		head = append(head, "fx_volatility_pct_month", "fx_basis", "revenue_12m_"+h, "revenue_share_pct", "clients")
		for _, f := range m.Factors {
			head = append(head, "points_"+f.Key)
		}
		head = append(head, "note")
		row(head...)
		for _, r := range m.Rows {
			rec := []interface{}{r.Rank, r.Country, r.Name, r.Currency, r.Score, r.Factors}
			for _, in := range m.Indicators {
				if v := r.Indicators[in.Code]; v != nil {
					rec = append(rec, v.Value, v.Year)
				} else {
					rec = append(rec, "", "")
				}
			}
			rec = append(rec, cp(r.FXVol), r.FXBasis, r.Revenue, r.SharePct, r.Clients)
			for _, p := range r.Parts {
				if p.Norm != nil {
					rec = append(rec, p.Points)
				} else {
					rec = append(rec, "")
				}
			}
			rec = append(rec, r.Note)
			row(rec...)
		}
	default:
		return fmt.Errorf("unknown dataset")
	}
	cw.Flush()
	return cw.Error()
}
