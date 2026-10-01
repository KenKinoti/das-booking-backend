// Package insights is the analyst layer of the ERP: the dashboard business
// snapshot, analyst-grade analytics (revenue breakdowns with YoY, cohorts,
// concentration, churn, lifetime value, payment behaviour, seasonality,
// service-line margins, hosting unit economics), internal business
// opportunities and public market indicators (World Bank Indicators API),
// plus clean CSV fact-table exports.
//
// Money is reported in the organisation's home currency. A document in
// another currency is converted with the rate locked on it when it was
// created (exchange_rate, units of the document currency per 1 home unit);
// documents without a locked rate use today's rate from pkg/fxrates. Every
// response says how many documents were converted and how.
package insights

import (
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/recurbills"
)

const dayFmt = "2006-01-02"

// Service holds the dependencies. Exp, Bills and WB are optional.
type Service struct {
	DB    *gorm.DB
	FX    *fxrates.Service
	Exp   *expenses.Service
	Bills *recurbills.Service
	WB    *WorldBank
	Now   func() time.Time
}

// New builds the service.
func New(db *gorm.DB, fx *fxrates.Service, exp *expenses.Service, bills *recurbills.Service, wb *WorldBank) *Service {
	if fx == nil {
		fx = &fxrates.Service{DB: db}
	}
	return &Service{DB: db, FX: fx, Exp: exp, Bills: bills, WB: wb, Now: time.Now}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Home is the organisation's home currency.
func (s *Service) Home(org string) string {
	h := strings.ToUpper(strings.TrimSpace(orgprefs.Currency(s.DB, org)))
	if !reCur.MatchString(h) {
		return "AUD"
	}
	return h
}

func (s *Service) scan(dest interface{}, q string, args ...interface{}) error {
	err := s.DB.Raw(q, args...).Scan(dest).Error
	if err != nil {
		log.Printf("insights: query failed: %v", err)
	}
	return err
}

var reCur = regexp.MustCompile(`^[A-Z]{3}$`)

func r2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func r1(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10) / 10
}

func fptr(v float64) *float64 { return &v }

func pctChange(cur, prev float64) *float64 {
	if prev == 0 {
		return nil
	}
	v := r1((cur - prev) / math.Abs(prev) * 100)
	return &v
}

func day(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// ---------- currency conversion ----------

// RateUsed describes how documents in one currency were converted.
type RateUsed struct {
	Currency string  `json:"currency"`
	Docs     int     `json:"docs"`
	Locked   int     `json:"locked"`   // converted at the rate locked on the document
	Today    int     `json:"today"`    // converted at today's rate (no locked rate)
	Missing  int     `json:"missing"`  // no rate at all: left out
	Rate     float64 `json:"rate"`     // today's rate (home per 1 unit) used for Today docs
	Source   string  `json:"source"`   // fxrates source of that rate
	Provider string  `json:"provider"` // where today's rate came from
}

// Conv is the currency context of one request.
type Conv struct {
	Home     string     `json:"home"`
	Currency string     `json:"currency"` // reporting currency (home, or the filter)
	Filter   string     `json:"filter,omitempty"`
	Rates    []RateUsed `json:"rates"`
	Note     string     `json:"note"`
	today    map[string]float64
}

// conv prepares the conversion for an organisation. filter (optional) limits
// the analysis to documents in that currency, reported in that currency.
func (s *Service) conv(org, filter string) *Conv {
	home := s.Home(org)
	filter = strings.ToUpper(strings.TrimSpace(filter))
	if !reCur.MatchString(filter) || filter == home {
		filter = ""
	}
	c := &Conv{Home: home, Currency: home, Filter: filter, Rates: []RateUsed{}, today: map[string]float64{}}
	if filter != "" {
		c.Currency = filter
		c.Note = fmt.Sprintf("Only documents in %s, shown in %s (no conversion).", filter, filter)
		return c
	}
	var rows []struct {
		Cur            string
		Docs, Locked   int
		NoRate, Credit int
	}
	s.scan(&rows, `SELECT UPPER(currency) AS cur, COUNT(*) AS docs,
		COUNT(*) FILTER (WHERE UPPER(COALESCE(base_currency,'')) = ? AND exchange_rate > 0) AS locked,
		COUNT(*) FILTER (WHERE NOT (UPPER(COALESCE(base_currency,'')) = ? AND exchange_rate > 0)) AS no_rate, 0 AS credit
		FROM billing_invoices WHERE organization_id = ? AND deleted_at IS NULL AND doc_type IN ('invoice','credit_note','quote')
		  AND status NOT IN ('draft','void') AND COALESCE(NULLIF(currency,''), ?) <> '' AND UPPER(COALESCE(NULLIF(currency,''), ?)) <> ?
		GROUP BY 1 ORDER BY 2 DESC`, home, home, org, home, home, home)
	parts := []string{}
	for _, r := range rows {
		if !reCur.MatchString(r.Cur) {
			continue
		}
		ru := RateUsed{Currency: r.Cur, Docs: r.Docs, Locked: r.Locked}
		q := s.FX.Quote(org, r.Cur, home)
		if q.OK() {
			ru.Rate, ru.Source, ru.Provider = q.Rate, q.Source, q.Provider
			c.today[r.Cur] = q.Rate
			ru.Today = r.NoRate
		} else {
			ru.Missing = r.NoRate
		}
		c.Rates = append(c.Rates, ru)
		p := fmt.Sprintf("%d %s document%s", r.Docs, r.Cur, plural(r.Docs))
		det := []string{}
		if ru.Locked > 0 {
			det = append(det, fmt.Sprintf("%d at the rate locked on the document", ru.Locked))
		}
		if ru.Today > 0 {
			det = append(det, fmt.Sprintf("%d at today's rate 1 %s = %s %s (%s)", ru.Today, r.Cur, trimFloat(ru.Rate), home, ru.Provider))
		}
		if ru.Missing > 0 {
			det = append(det, fmt.Sprintf("%d left out (no exchange rate — set a manual rate)", ru.Missing))
		}
		parts = append(parts, p+": "+strings.Join(det, ", "))
	}
	if len(parts) == 0 {
		c.Note = "All amounts in " + home + "."
	} else {
		c.Note = "All amounts in " + home + ", excluding tax. Converted: " + strings.Join(parts, "; ") + "."
	}
	return c
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.6f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// Factor is the SQL expression (for billing_invoices alias a) that converts an
// amount of the document into the reporting currency; NULL = no rate.
func (c *Conv) Factor(a string) string {
	if c.Filter != "" {
		return "1.0"
	}
	h := c.Home
	cur := fmt.Sprintf("UPPER(COALESCE(NULLIF(%s.currency,''),'%s'))", a, h)
	b := &strings.Builder{}
	fmt.Fprintf(b, "(CASE WHEN %s = '%s' THEN 1.0 WHEN UPPER(COALESCE(%s.base_currency,'')) = '%s' AND %s.exchange_rate > 0 THEN 1.0 / %s.exchange_rate", cur, h, a, h, a, a)
	keys := make([]string, 0, len(c.today))
	for k := range c.today {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if reCur.MatchString(k) {
			fmt.Fprintf(b, " WHEN %s = '%s' THEN %s", cur, k, trimFloat(c.today[k]))
		}
	}
	b.WriteString(" ELSE NULL END)")
	return b.String()
}

// Where adds the currency filter for alias a ("" when none).
func (c *Conv) Where(a string) string {
	if c.Filter == "" {
		return ""
	}
	return fmt.Sprintf(" AND UPPER(COALESCE(NULLIF(%s.currency,''),'%s')) = '%s'", a, c.Home, c.Filter)
}

// docsCTE is the common table of counted documents (invoices and credit
// notes that are not draft / void) with their conversion factor and a client
// key (customer id, else the client name).
func (c *Conv) docsCTE() string {
	return `docs AS (
	SELECT i.id, i.number, i.doc_type, i.status, i.issue_date::date AS d, i.due_date::date AS due,
		UPPER(COALESCE(NULLIF(i.currency,''),'` + c.Home + `')) AS cur, ` + c.Factor("i") + ` AS f,
		CASE WHEN i.doc_type = 'credit_note' THEN -1 ELSE 1 END AS sgn,
		i.total, i.tax_total, i.balance_due, i.amount_paid, COALESCE(i.credits_applied,0) AS credits, COALESCE(i.write_off_amount,0) AS writeoff,
		COALESCE(NULLIF(i.customer_id,''), 'n:' || LOWER(TRIM(i.client_name))) AS ck,
		COALESCE(i.customer_id,'') AS customer_id, i.client_name, COALESCE(i.source,'') AS source, COALESCE(i.title,'') AS title,
		i.is_recurring, i.recurrence_parent_id, i.paid_at
	FROM billing_invoices i
	WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type IN ('invoice','credit_note') AND i.status NOT IN ('draft','void')` + c.Where("i") + `
)`
}

// clientNames maps client keys to display names.
func (s *Service) clientNames(org string) map[string]string {
	out := map[string]string{}
	var rows []struct{ CK, Name string }
	s.scan(&rows, `SELECT DISTINCT ON (ck) ck, name FROM (
		SELECT COALESCE(NULLIF(customer_id,''), 'n:' || LOWER(TRIM(client_name))) AS ck, client_name AS name, issue_date
		FROM billing_invoices WHERE organization_id = ? AND deleted_at IS NULL AND client_name <> '') x ORDER BY ck, issue_date DESC`, org)
	for _, r := range rows {
		out[r.CK] = r.Name
	}
	var cs []struct{ ID, Name string }
	s.scan(&cs, `SELECT id, COALESCE(NULLIF(TRIM(company_name),''), TRIM(COALESCE(first_name,'') || ' ' || COALESCE(last_name,''))) AS name
		FROM customers WHERE organization_id = ? AND deleted_at IS NULL`, org)
	for _, c := range cs {
		if strings.TrimSpace(c.Name) != "" {
			out[c.ID] = c.Name
		}
	}
	return out
}

func nameOf(names map[string]string, ck string) string {
	if n := names[ck]; n != "" {
		return n
	}
	if strings.HasPrefix(ck, "n:") {
		return strings.TrimPrefix(ck, "n:")
	}
	return "(unknown client)"
}

// customerID returns the customer id of a client key ("" for name-only keys).
func customerID(ck string) string {
	if strings.HasPrefix(ck, "n:") {
		return ""
	}
	return ck
}

// ---------- ranges ----------

// Range is an inclusive date range.
type Range struct {
	From time.Time
	To   time.Time
}

// Months returns the number of calendar months the range touches.
func (r Range) Months() int {
	return (r.To.Year()-r.From.Year())*12 + int(r.To.Month()) - int(r.From.Month()) + 1
}

// ParseRange reads from/to (YYYY-MM-DD); missing values default to the last
// `defMonths` whole months up to today.
func ParseRange(today time.Time, from, to string, defMonths int) (Range, error) {
	today = day(today)
	r := Range{From: monthStart(today).AddDate(0, -(defMonths - 1), 0), To: today}
	if from = strings.TrimSpace(from); from != "" {
		if strings.EqualFold(from, "all") {
			r.From = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
		} else {
			f, err := time.Parse(dayFmt, from)
			if err != nil {
				return r, fmt.Errorf("invalid 'from' date")
			}
			r.From = f
		}
	}
	if to = strings.TrimSpace(to); to != "" {
		t, err := time.Parse(dayFmt, to)
		if err != nil {
			return r, fmt.Errorf("invalid 'to' date")
		}
		r.To = t
	}
	if r.To.Before(r.From) {
		return r, fmt.Errorf("'from' must be on or before 'to'")
	}
	if r.To.Sub(r.From) > 40*366*24*time.Hour {
		return r, fmt.Errorf("date range too large")
	}
	return r, nil
}

// clampAll narrows an "all history" range to the first document.
func (s *Service) clampAll(org string, r Range) Range {
	if r.From.Year() > 1990 {
		return r
	}
	var first *time.Time
	s.DB.Raw(`SELECT MIN(issue_date) FROM billing_invoices WHERE organization_id = ? AND deleted_at IS NULL AND doc_type = 'invoice' AND status NOT IN ('draft','void')`, org).Row().Scan(&first)
	if first != nil && !first.IsZero() {
		r.From = monthStart(*first)
	} else {
		r.From = monthStart(r.To)
	}
	return r
}
