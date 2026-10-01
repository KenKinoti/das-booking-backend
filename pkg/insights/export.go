package insights

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Clean fact-table exports (CSV) for analysis elsewhere. Each dataset has a
// documented, stable column list; amounts are in the document currency plus
// a *_home column in the home currency (see Conv for how rates are chosen).

// DatasetDoc documents one export.
type DatasetDoc struct {
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	Grain   string   `json:"grain"`
	Dated   string   `json:"dated_by"` // column the period filter applies to ("" = not filtered)
	Columns []string `json:"columns"`
}

// Datasets lists the exports.
var Datasets = []DatasetDoc{
	{Key: "invoices", Title: "Invoices & credit notes", Grain: "one row per document (not draft / void)", Dated: "issue_date",
		Columns: []string{"id", "number", "doc_type", "status", "issue_date", "due_date", "paid_at", "customer_id", "client_name", "currency", "subtotal", "tax_total", "total", "amount_paid", "balance_due", "credits_applied", "write_off_amount", "exchange_rate", "fx_to_home", "net_home", "total_home", "is_recurring", "recurrence_interval", "source", "external_id"}},
	{Key: "invoice_lines", Title: "Invoice lines", Grain: "one row per line; net_allocated splits the document's net total by line totals", Dated: "issue_date",
		Columns: []string{"invoice_id", "number", "doc_type", "issue_date", "customer_id", "client_name", "position", "description", "service_line", "quantity", "unit_price", "tax_rate", "line_total", "currency", "net_allocated", "net_allocated_home"}},
	{Key: "payments", Title: "Payments received", Grain: "one row per payment", Dated: "date",
		Columns: []string{"id", "invoice_id", "invoice_number", "customer_id", "client_name", "date", "amount", "currency", "amount_home", "method", "reference", "is_deposit", "source"}},
	{Key: "expenses", Title: "Expenses", Grain: "one row per expense (all statuses)", Dated: "invoice_date",
		Columns: []string{"id", "status", "source", "vendor_name", "invoice_number", "invoice_date", "period_start", "period_end", "service", "category", "currency", "net", "tax", "total", "home_currency", "home_net", "home_tax", "home_total", "customer_id", "bill_id"}},
	{Key: "synergy_transactions", Title: "Synergy statement lines", Grain: "one row per statement line (AUD)", Dated: "date",
		Columns: []string{"date", "domain", "kind", "category", "product_type", "description", "debit", "credit", "amount_aud", "balance", "month", "service_month", "plan_from", "plan_to"}},
	{Key: "synergy_usage", Title: "Synergy hosting usage snapshots", Grain: "one row per site per usage report", Dated: "snapshot_date",
		Columns: []string{"snapshot_date", "domain", "status", "plan", "usage_mb", "limit_mb", "pct_used", "remaining_mb"}},
	{Key: "synergy_sites", Title: "Hosted sites (current unit economics)", Grain: "one row per site, as of today", Dated: "",
		Columns: []string{"domain", "status", "plan", "customer_id", "client", "cost_aud_month", "cost_home_month", "billed_home_month", "billed_source", "margin_home_month", "margin_pct", "usage_mb", "limit_mb", "pct_used", "flags"}},
	{Key: "customers", Title: "Customers", Grain: "one row per customer, with lifetime totals", Dated: "",
		Columns: []string{"id", "name", "company_name", "email", "country_code", "currency", "source", "created_at", "first_invoice", "last_invoice", "invoices", "lifetime_net_home"}},
}

// DatasetByKey finds a dataset.
func DatasetByKey(k string) *DatasetDoc {
	for i := range Datasets {
		if Datasets[i].Key == k {
			return &Datasets[i]
		}
	}
	return nil
}

func cell(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case time.Time:
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 {
			return x.Format(dayFmt)
		}
		return x.UTC().Format(time.RFC3339)
	case *time.Time:
		if x == nil {
			return ""
		}
		return cell(*x)
	case []byte:
		return string(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return fmt.Sprint(v)
}

func safeCSV(s string) string {
	// Neutralise spreadsheet formulas in text cells.
	if s != "" && strings.ContainsRune("=+@", rune(s[0])) {
		return "'" + s
	}
	return s
}

// WriteDataset streams one export as CSV.
func (s *Service) WriteDataset(w io.Writer, org, key string, r Range) (int, error) {
	ds := DatasetByKey(key)
	if ds == nil {
		return 0, fmt.Errorf("unknown dataset")
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(ds.Columns); err != nil {
		return 0, err
	}
	if key == "synergy_sites" {
		n := 0
		for _, st := range s.HostingUnits(org).Sites {
			cw.Write([]string{st.Domain, st.Status, st.Plan, st.CustomerID, safeCSV(st.Client), cell(st.CostAUD), cell(st.CostHome), cellp(st.Billed), st.BilledSource,
				cellp(st.Margin), cellp(st.MarginPct), cellp(st.UsageMB), cellp(st.LimitMB), cellp(st.PctUsed), strings.Join(st.Flags, ";")})
			n++
		}
		cw.Flush()
		return n, cw.Error()
	}
	c := s.conv(org, "")
	a := map[string]interface{}{"org": org, "from": r.From.Format(dayFmt), "to": r.To.Format(dayFmt)}
	q := ""
	switch key {
	case "invoices":
		q = `SELECT i.id, i.number, i.doc_type, i.status, i.issue_date::date, i.due_date::date, i.paid_at, COALESCE(i.customer_id,''), i.client_name,
			UPPER(COALESCE(NULLIF(i.currency,''),'` + c.Home + `')), i.subtotal, i.tax_total, i.total, i.amount_paid, i.balance_due, COALESCE(i.credits_applied,0), COALESCE(i.write_off_amount,0),
			i.exchange_rate, ROUND((` + c.Factor("i") + `)::numeric, 8), ROUND(((CASE WHEN i.doc_type = 'credit_note' THEN -1 ELSE 1 END) * (i.total - i.tax_total) * ` + c.Factor("i") + `)::numeric, 2),
			ROUND((i.total * ` + c.Factor("i") + `)::numeric, 2), i.is_recurring, COALESCE(i.recurrence_interval,''), COALESCE(i.source,''), COALESCE(i.external_id,'')
		FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type IN ('invoice','credit_note') AND i.status NOT IN ('draft','void')
			AND i.issue_date BETWEEN (@from)::date AND (@to)::date ORDER BY i.issue_date, i.number`
	case "invoice_lines":
		q = "WITH " + linesCTEExport(c, s.Rules(org)) + `
		SELECT invoice_id, number, doc_type, d, customer_id, client_name, position, descr, line, quantity, unit_price, tax_rate, line_total, cur,
			ROUND(amt_doc::numeric, 2), ROUND((amt_doc * f)::numeric, 2)
		FROM x WHERE d BETWEEN (@from)::date AND (@to)::date ORDER BY d, number, position`
	case "payments":
		q = `SELECT p.id, p.invoice_id, i.number, COALESCE(i.customer_id,''), i.client_name, p.date::date, p.amount, UPPER(COALESCE(NULLIF(i.currency,''),'` + c.Home + `')),
			ROUND((p.amount * ` + c.Factor("i") + `)::numeric, 2), COALESCE(p.method,''), COALESCE(p.reference,''), p.is_deposit, COALESCE(p.source,'')
		FROM billing_payments p JOIN billing_invoices i ON i.id = p.invoice_id
		WHERE p.organization_id = @org AND p.date BETWEEN (@from)::date AND (@to)::date ORDER BY p.date, i.number`
	case "expenses":
		q = `SELECT id, status, source, vendor_name, invoice_number, invoice_date, period_start, period_end, service, category, currency, net, tax, total,
			home_currency, home_net, home_tax, home_total, COALESCE(customer_id,''), COALESCE(bill_id,'')
		FROM expense_items WHERE organization_id = @org AND invoice_date BETWEEN (@from)::date AND (@to)::date ORDER BY invoice_date, vendor_name`
	case "synergy_transactions":
		q = `SELECT date::date, domain, kind, category, product_type, description, debit, credit, amount, balance, month, service_month, plan_from, plan_to
		FROM synergy_transactions WHERE organization_id = @org AND date::date BETWEEN (@from)::date AND (@to)::date ORDER BY date, seq`
	case "synergy_usage":
		q = `SELECT snapshot_date::date, domain, status, plan, usage_mb, limit_mb, pct_used, remaining_mb
		FROM synergy_usage_snapshots WHERE organization_id = @org AND snapshot_date BETWEEN (@from)::date AND (@to)::date ORDER BY snapshot_date, domain`
	case "customers":
		q = "WITH " + c.docsCTE() + `,
		agg AS (SELECT customer_id, MIN(d) AS first_d, MAX(d) AS last_d, COUNT(*) FILTER (WHERE doc_type = 'invoice') AS n,
			ROUND(SUM(sgn * f * (total - tax_total))::numeric, 2) AS life FROM docs WHERE customer_id <> '' GROUP BY customer_id)
		SELECT cu.id, TRIM(COALESCE(cu.first_name,'') || ' ' || COALESCE(cu.last_name,'')), COALESCE(cu.company_name,''), COALESCE(cu.email,''), COALESCE(cu.country_code,''),
			COALESCE(cu.currency,''), COALESCE(cu.source,''), cu.created_at, agg.first_d, agg.last_d, COALESCE(agg.n,0), COALESCE(agg.life,0)
		FROM customers cu LEFT JOIN agg ON agg.customer_id = cu.id
		WHERE cu.organization_id = @org AND cu.deleted_at IS NULL ORDER BY cu.created_at`
	}
	rows, err := s.DB.Raw(q, a).Rows()
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	vals := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	rec := make([]string, len(cols))
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		for i, v := range vals {
			rec[i] = safeCSV(cell(v))
		}
		if err := cw.Write(rec); err != nil {
			return n, err
		}
		n++
		if n%500 == 0 {
			cw.Flush()
		}
	}
	cw.Flush()
	return n, cw.Error()
}

func cellp(v *float64) string {
	if v == nil {
		return ""
	}
	return cell(*v)
}

// linesCTEExport is linesCTE with the extra line columns of the export.
func linesCTEExport(c *Conv, rules []ServiceRule) string {
	desc := "COALESCE(NULLIF(TRIM(it.description),''), NULLIF(d.title,''), '')"
	share := `CASE WHEN COALESCE(SUM(it.line_total) OVER (PARTITION BY d.id), 0) <> 0
			THEN COALESCE(it.line_total,0) / SUM(it.line_total) OVER (PARTITION BY d.id)
			ELSE 1.0 / COUNT(*) OVER (PARTITION BY d.id) END`
	return c.docsCTE() + `,
x AS (
	SELECT d.id AS invoice_id, d.number, d.doc_type, d.d, d.customer_id, d.client_name, COALESCE(it.position,0) AS position, ` + desc + ` AS descr,
		` + LineSQL(rules, desc) + ` AS line, COALESCE(it.quantity,0) AS quantity, COALESCE(it.unit_price,0) AS unit_price, COALESCE(it.tax_rate,0) AS tax_rate,
		COALESCE(it.line_total,0) AS line_total, d.cur, COALESCE(d.f, 0) AS f,
		d.sgn * (d.total - d.tax_total) * (` + share + `) AS amt_doc
	FROM docs d LEFT JOIN billing_invoice_items it ON it.invoice_id = d.id
)`
}
