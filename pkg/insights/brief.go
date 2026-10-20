package insights

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/emailprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/pnl"
)

// Strategy brief: one stored document per month that turns the organisation's
// own numbers, its export & FX position, market indicators and the industry
// watch into ranked actions. The rule-based brief is deterministic — the same
// data and date give the same brief — and always available; the optional AI
// version is written from the same data and stored next to it.

// Brief is one stored brief (insights_briefs).
type Brief struct {
	ID             string        `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrganizationID string        `json:"-" gorm:"type:varchar(255);not null;uniqueIndex:idx_ins_brief,priority:1"`
	Period         string        `json:"period" gorm:"type:varchar(7);not null;uniqueIndex:idx_ins_brief,priority:2"`        // YYYY-MM
	GeneratedBy    string        `json:"generated_by" gorm:"type:varchar(10);not null;uniqueIndex:idx_ins_brief,priority:3"` // rule | ai
	PeriodFrom     time.Time     `json:"period_from" gorm:"type:date"`
	PeriodTo       time.Time     `json:"period_to" gorm:"type:date"`
	GeneratedAt    time.Time     `json:"generated_at"`
	Title          string        `json:"title" gorm:"type:varchar(200)"`
	Headline       string        `json:"headline" gorm:"type:text"`
	Content        *BriefContent `json:"content,omitempty" gorm:"type:text;serializer:json"`
	AIText         string        `json:"ai_text,omitempty" gorm:"column:ai_text;type:text"`
	AIModel        string        `json:"ai_model,omitempty" gorm:"column:ai_model;type:varchar(80)"`
	Trigger        string        `json:"trigger" gorm:"type:varchar(20)"` // manual | scheduled
	CreatedBy      string        `json:"-" gorm:"type:varchar(255)"`
	EmailedAt      *time.Time    `json:"emailed_at"`
	EmailedTo      string        `json:"emailed_to" gorm:"type:text"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// TableName is fixed.
func (Brief) TableName() string { return "insights_briefs" }

// BriefSnapshot is the performance of the period.
type BriefSnapshot struct {
	// Revenue = the profit & loss report's revenue (home-currency records,
	// accrual, excl. tax) + invoices in other currencies at their locked rates
	// (the P&L lists one currency at a time and leaves those out).
	Revenue        float64  `json:"revenue"`
	PnLRevenue     float64  `json:"pnl_revenue"`
	ForeignRevenue float64  `json:"foreign_revenue"`
	PrevRevenue    float64  `json:"prev_revenue"`
	RevenueDelta   *float64 `json:"revenue_delta_pct"`
	YearAgoRevenue float64  `json:"year_ago_revenue"`
	RevenueYoY     *float64 `json:"revenue_yoy_pct"`
	Expenses       float64  `json:"expenses"`
	PrevExpenses   float64  `json:"prev_expenses"`
	ExpensesDelta  *float64 `json:"expenses_delta_pct"`
	NetProfit      float64  `json:"net_profit"` // P&L net profit + ForeignRevenue
	PnLNetProfit   float64  `json:"pnl_net_profit"`
	PrevNetProfit  float64  `json:"prev_net_profit"`
	MarginPct      *float64 `json:"margin_pct"`
	Invoiced       float64  `json:"invoiced"`
	Invoices       int      `json:"invoices"`
	Cash           float64  `json:"cash_collected"`
	PrevCash       float64  `json:"prev_cash_collected"`
	Clients        int      `json:"active_clients"`
	NewClients     int      `json:"new_clients"`
	Outstanding    float64  `json:"outstanding"` // as of the brief's date
	Overdue        float64  `json:"overdue"`
	OverdueCount   int      `json:"overdue_count"`
	MRR            float64  `json:"mrr"`
}

// BriefChange is one "what changed" line.
type BriefChange struct {
	Key   string   `json:"key"`
	Text  string   `json:"text"`
	Delta *float64 `json:"delta_pct"`
	Tone  string   `json:"tone"` // good | bad | neutral
}

// BriefMarket is one export market in the brief.
type BriefMarket struct {
	Country  string  `json:"country"`
	Name     string  `json:"name"`
	Revenue  float64 `json:"revenue"`
	SharePct float64 `json:"share_pct"`
}

// BriefTrade is the export & FX position.
type BriefTrade struct {
	From          string        `json:"from"` // trailing 12 months
	To            string        `json:"to"`
	Revenue       float64       `json:"revenue"`
	Export        float64       `json:"export"`
	ExportPct     *float64      `json:"export_pct"`
	PrevExportPct *float64      `json:"prev_export_pct"`
	PeriodExport  float64       `json:"period_export"`
	PeriodPct     *float64      `json:"period_export_pct"`
	Markets       []BriefMarket `json:"markets"`
	ForeignOpen   float64       `json:"foreign_open_home"` // open foreign receivables at locked rates
	ForeignToday  float64       `json:"foreign_open_today"`
	Unrealised    float64       `json:"unrealised"`
	OpenInvoices  int           `json:"open_invoices"`
	Hedge         []string      `json:"hedge"`
	Lines         []string      `json:"lines"`
}

// BriefSignal is one market signal.
type BriefSignal struct {
	Title  string `json:"title"`
	Text   string `json:"text"`
	Source string `json:"source"`
	Link   string `json:"link,omitempty"`
}

// BriefHeadline is one industry-watch item.
type BriefHeadline struct {
	Title  string   `json:"title"`
	Link   string   `json:"link"`
	Source string   `json:"source"`
	Date   string   `json:"date"`
	Score  float64  `json:"score"`
	Topics []string `json:"topics"`
	Why    string   `json:"why"`
}

// BriefRisk is one risk.
type BriefRisk struct {
	Key      string `json:"key"`
	Severity string `json:"severity"` // high | medium | low
	Title    string `json:"title"`
	Text     string `json:"text"`
}

// BriefAction is one recommended action.
type BriefAction struct {
	Rank      int     `json:"rank"`
	Key       string  `json:"key"`
	Type      string  `json:"type"`
	Severity  string  `json:"severity"`
	Title     string  `json:"title"`
	Rationale string  `json:"rationale"`
	Value     float64 `json:"value"`
	ValueKind string  `json:"value_kind"` // per_year | one_off | at_risk | none
	Basis     string  `json:"basis"`
	Link      string  `json:"link"`
}

// BriefContent is the stored brief.
type BriefContent struct {
	Currency    string          `json:"currency"`
	Period      string          `json:"period"`
	PeriodLabel string          `json:"period_label"`
	PrevLabel   string          `json:"prev_label"`
	From        string          `json:"from"`
	To          string          `json:"to"`
	AsOf        string          `json:"as_of"`
	Partial     bool            `json:"partial"` // the month is not over
	Snapshot    BriefSnapshot   `json:"snapshot"`
	Changes     []BriefChange   `json:"changes"`
	Trade       BriefTrade      `json:"trade"`
	Signals     []BriefSignal   `json:"signals"`
	Headlines   []BriefHeadline `json:"headlines"`
	Risks       []BriefRisk     `json:"risks"`
	Actions     []BriefAction   `json:"actions"`
	ActionTotal float64         `json:"action_total"` // sum of per-year and one-off estimates
	Notes       []string        `json:"notes"`
}

// BriefPeriod resolves "YYYY-MM" ("" = the last complete month before today).
func BriefPeriod(period string, today time.Time) (from, to time.Time, label string, partial bool, err error) {
	today = day(today)
	period = strings.TrimSpace(period)
	if period == "" {
		period = monthStart(today).AddDate(0, -1, 0).Format("2006-01")
	}
	from, err = time.Parse("2006-01", period)
	if err != nil {
		return from, to, "", false, fmt.Errorf("the period must look like 2026-09")
	}
	if from.After(today) {
		return from, to, "", false, fmt.Errorf("that month has not started yet")
	}
	if from.Before(today.AddDate(-10, 0, 0)) {
		return from, to, "", false, fmt.Errorf("choose a month in the last ten years")
	}
	to = from.AddDate(0, 1, -1)
	if to.After(today) {
		to, partial = today, true
	}
	return from, to, from.Format("January 2006"), partial, nil
}

func signed(v float64) string {
	if v > 0 {
		return "+" + trimFloat(v)
	}
	return trimFloat(v)
}

func upDown(v float64) string {
	if v >= 0 {
		return "up"
	}
	return "down"
}

// BuildBrief computes the rule-based brief for a month as of today.
func (i *Intel) BuildBrief(ctx context.Context, org, period string, today time.Time, loc *time.Location) (*BriefContent, error) {
	today = day(today)
	from, to, label, partial, err := BriefPeriod(period, today)
	if err != nil {
		return nil, err
	}
	if loc == nil {
		loc = time.UTC
	}
	c := i.conv(org, "")
	home := c.Home
	// Previous period: the month before (the same days when the month is not over).
	pfrom := from.AddDate(0, -1, 0)
	pto := from.AddDate(0, 0, -1)
	if partial {
		pto = pfrom.AddDate(0, 0, to.Day()-1)
		if pto.Month() != pfrom.Month() {
			pto = from.AddDate(0, 0, -1)
		}
	}
	prevLabel := pfrom.Format("January 2006")
	if partial {
		prevLabel = fmt.Sprintf("the first %d days of %s", to.Day(), pfrom.Format("January"))
	}
	b := &BriefContent{Currency: home, Period: from.Format("2006-01"), PeriodLabel: label, PrevLabel: prevLabel, From: from.Format(dayFmt), To: to.Format(dayFmt), AsOf: today.Format(dayFmt),
		Partial: partial, Changes: []BriefChange{}, Signals: []BriefSignal{}, Headlines: []BriefHeadline{}, Risks: []BriefRisk{}, Actions: []BriefAction{}, Notes: []string{}}
	money := func(v float64) string { return home + " " + amt(v) }

	// ---- 1. performance snapshot (pkg/pnl, accrual) ----
	sn := &b.Snapshot
	yfrom, yto := from.AddDate(-1, 0, 0), to.AddDate(-1, 0, 0)
	in := pnl.Load(i.DB, org, pfrom, to, loc)
	in.Home, in.OrgHome, in.Decimals = home, home, currency.Decimals(home)
	mj := func(v int64) float64 { return r2(float64(v) / math.Pow10(in.Decimals)) }
	cur := pnl.Compute(in, pnl.Period{From: from, To: to}, pnl.Accrual).Totals()
	prev := pnl.Compute(in, pnl.Period{From: pfrom, To: pto}, pnl.Accrual).Totals()
	a := map[string]interface{}{"org": org, "from": from.Format(dayFmt), "to": to.Format(dayFmt), "pfrom": pfrom.Format(dayFmt), "pto": pto.Format(dayFmt), "today": today.Format(dayFmt),
		"yfrom": yfrom.Format(dayFmt), "yto": yto.Format(dayFmt), "home": home}
	// Invoices and credit notes in other currencies, at their locked rates: the
	// profit & loss report is per currency and does not include them.
	var fr struct{ Cur, Prev, Yago float64 }
	i.scan(&fr, "WITH "+c.docsCTE()+`
SELECT COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date), 0) AS cur,
	COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@pfrom)::date AND (@pto)::date), 0) AS prev,
	COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@yfrom)::date AND (@yto)::date), 0) AS yago
FROM docs WHERE f IS NOT NULL AND cur <> @home`, a)
	sn.PnLRevenue, sn.ForeignRevenue, sn.PnLNetProfit = mj(cur.Revenue), r2(fr.Cur), mj(cur.NetProfit)
	sn.Revenue, sn.PrevRevenue = r2(sn.PnLRevenue+fr.Cur), r2(mj(prev.Revenue)+fr.Prev)
	sn.Expenses, sn.PrevExpenses = mj(cur.TotalExpenses), mj(prev.TotalExpenses)
	sn.NetProfit, sn.PrevNetProfit = r2(sn.PnLNetProfit+fr.Cur), r2(mj(prev.NetProfit)+fr.Prev)
	sn.RevenueDelta, sn.ExpensesDelta = pctChange(sn.Revenue, sn.PrevRevenue), pctChange(sn.Expenses, sn.PrevExpenses)
	if sn.Revenue != 0 {
		sn.MarginPct = fptr(r1(sn.NetProfit / sn.Revenue * 100))
	}
	yin := pnl.Load(i.DB, org, yfrom, yto, loc)
	yin.Home, yin.OrgHome, yin.Decimals = home, home, in.Decimals
	sn.YearAgoRevenue = r2(mj(pnl.Compute(yin, pnl.Period{From: yfrom, To: yto}, pnl.Accrual).Totals().Revenue) + fr.Yago)
	sn.RevenueYoY = pctChange(sn.Revenue, sn.YearAgoRevenue)

	var dv struct {
		Invoiced       float64
		N, Clients, Nw int
	}
	i.scan(&dv, "WITH "+c.docsCTE()+`,
firsts AS (SELECT ck, MIN(d) AS first FROM docs WHERE doc_type = 'invoice' GROUP BY ck)
SELECT COALESCE(SUM(sgn * f * (total - tax_total)), 0) AS invoiced, COUNT(*) FILTER (WHERE doc_type = 'invoice') AS n, COUNT(DISTINCT docs.ck) AS clients,
	COUNT(DISTINCT docs.ck) FILTER (WHERE firsts.first BETWEEN (@from)::date AND (@to)::date) AS nw
FROM docs LEFT JOIN firsts ON firsts.ck = docs.ck WHERE f IS NOT NULL AND d BETWEEN (@from)::date AND (@to)::date`, a)
	sn.Invoiced, sn.Invoices, sn.Clients, sn.NewClients = r2(dv.Invoiced), dv.N, dv.Clients, dv.Nw
	var cash struct{ Cur, Prev float64 }
	i.scan(&cash, `SELECT COALESCE(SUM(p.amount * `+c.Factor("i")+`) FILTER (WHERE p.date BETWEEN (@from)::date AND (@to)::date), 0) AS cur,
	COALESCE(SUM(p.amount * `+c.Factor("i")+`) FILTER (WHERE p.date BETWEEN (@pfrom)::date AND (@pto)::date), 0) AS prev
FROM billing_payments p JOIN billing_invoices i ON i.id = p.invoice_id AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status <> 'void'
WHERE p.organization_id = @org AND p.date BETWEEN (@pfrom)::date AND (@to)::date`, a)
	sn.Cash, sn.PrevCash = r2(cash.Cur), r2(cash.Prev)
	var ar struct {
		Out, Over float64
		NO        int
	}
	i.scan(&ar, `SELECT COALESCE(SUM(i.balance_due * `+c.Factor("i")+`), 0) AS out,
	COALESCE(SUM(i.balance_due * `+c.Factor("i")+`) FILTER (WHERE i.due_date::date < (@today)::date), 0) AS over,
	COUNT(*) FILTER (WHERE i.due_date::date < (@today)::date) AS no
FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.status IN ('sent','partial') AND `+c.Factor("i")+` IS NOT NULL`, a)
	sn.Outstanding, sn.Overdue, sn.OverdueCount = r2(ar.Out), r2(ar.Over), ar.NO
	var recs []struct {
		Interval string
		Net      float64
	}
	i.scan(&recs, `SELECT COALESCE(i.recurrence_interval,'monthly') AS interval, (i.total - i.tax_total) * `+c.Factor("i")+` AS net
FROM billing_invoices i WHERE i.organization_id = @org AND i.deleted_at IS NULL AND i.doc_type = 'invoice' AND i.is_recurring AND i.status <> 'void'
	AND (i.recurrence_end_date IS NULL OR i.recurrence_end_date >= (@today)::date) AND `+c.Factor("i")+` IS NOT NULL`, a)
	for _, r := range recs {
		sn.MRR += r.Net / intervalMonths(r.Interval)
	}
	sn.MRR = r2(sn.MRR)

	// ---- 2. what changed ----
	tone := func(delta float64, goodUp bool) string {
		switch {
		case math.Abs(delta) < 0.05:
			return "neutral"
		case (delta > 0) == goodUp:
			return "good"
		}
		return "bad"
	}
	if sn.RevenueDelta != nil {
		b.Changes = append(b.Changes, BriefChange{Key: "revenue", Delta: sn.RevenueDelta, Tone: tone(*sn.RevenueDelta, true),
			Text: fmt.Sprintf("Revenue %s, %s %s%% on %s (%s).", money(sn.Revenue), upDown(*sn.RevenueDelta), trimFloat(math.Abs(*sn.RevenueDelta)), prevLabel, money(sn.PrevRevenue))})
	} else {
		b.Changes = append(b.Changes, BriefChange{Key: "revenue", Tone: "neutral", Text: fmt.Sprintf("Revenue %s; there was none in %s to compare with.", money(sn.Revenue), prevLabel)})
	}
	if sn.RevenueYoY != nil {
		b.Changes = append(b.Changes, BriefChange{Key: "revenue_yoy", Delta: sn.RevenueYoY, Tone: tone(*sn.RevenueYoY, true),
			Text: fmt.Sprintf("Against the same period a year ago (%s) revenue is %s %s%%.", money(sn.YearAgoRevenue), upDown(*sn.RevenueYoY), trimFloat(math.Abs(*sn.RevenueYoY)))})
	}
	if sn.ExpensesDelta != nil {
		b.Changes = append(b.Changes, BriefChange{Key: "expenses", Delta: sn.ExpensesDelta, Tone: tone(*sn.ExpensesDelta, false),
			Text: fmt.Sprintf("Costs %s, %s %s%% (%s before).", money(sn.Expenses), upDown(*sn.ExpensesDelta), trimFloat(math.Abs(*sn.ExpensesDelta)), money(sn.PrevExpenses))})
	}
	pd := sn.NetProfit - sn.PrevNetProfit
	b.Changes = append(b.Changes, BriefChange{Key: "net_profit", Delta: pctChange(sn.NetProfit, sn.PrevNetProfit), Tone: tone(pd, true),
		Text: fmt.Sprintf("Net profit %s (%s before): %s %s.", money(sn.NetProfit), money(sn.PrevNetProfit), upDown(pd), money(math.Abs(pd)))})
	if sn.Cash != 0 || sn.PrevCash != 0 {
		cd := pctChange(sn.Cash, sn.PrevCash)
		t := fmt.Sprintf("Cash collected from invoices %s (%s before).", money(sn.Cash), money(sn.PrevCash))
		tn := "neutral"
		if cd != nil {
			tn = tone(*cd, true)
		}
		b.Changes = append(b.Changes, BriefChange{Key: "cash", Delta: cd, Tone: tn, Text: t})
	}
	if sn.NewClients > 0 {
		b.Changes = append(b.Changes, BriefChange{Key: "new_clients", Tone: "good", Text: fmt.Sprintf("%d new client%s invoiced for the first time (%d active client%s in the period).", sn.NewClients, plural(sn.NewClients), sn.Clients, plural(sn.Clients))})
	}
	// Biggest movers by client.
	var mv []struct {
		CK        string
		Cur, Prev float64
	}
	i.scan(&mv, "WITH "+c.docsCTE()+`
SELECT ck, COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date), 0) AS cur,
	COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@pfrom)::date AND (@pto)::date), 0) AS prev
FROM docs WHERE f IS NOT NULL AND (d BETWEEN (@from)::date AND (@to)::date OR d BETWEEN (@pfrom)::date AND (@pto)::date)
GROUP BY ck ORDER BY ABS(COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@from)::date AND (@to)::date), 0)
	- COALESCE(SUM(sgn * f * (total - tax_total)) FILTER (WHERE d BETWEEN (@pfrom)::date AND (@pto)::date), 0)) DESC, ck LIMIT 3`, a)
	names := i.clientNames(org)
	for _, m := range mv {
		d := m.Cur - m.Prev
		if math.Abs(d) < 0.005 {
			continue
		}
		b.Changes = append(b.Changes, BriefChange{Key: "client:" + m.CK, Tone: tone(d, true),
			Text: fmt.Sprintf("%s: %s invoiced vs %s before (%s %s).", nameOf(names, m.CK), money(m.Cur), money(m.Prev), upDown(d), money(math.Abs(d)))})
	}

	// ---- 3. export & FX position (trailing 12 months to the period end; receivables as of today) ----
	trFrom := monthStart(to).AddDate(0, -11, 0)
	tr := i.TradeFor(org, Opts{Range: Range{From: trFrom, To: to}, Today: today})
	bt := &b.Trade
	bt.From, bt.To, bt.Revenue, bt.Export, bt.ExportPct, bt.PrevExportPct = tr.From, tr.To, tr.Totals.Revenue, tr.Totals.Export, tr.Totals.ExportPct, tr.Totals.PrevExportPct
	bt.Markets, bt.Hedge, bt.Lines = []BriefMarket{}, []string{}, []string{}
	for _, m := range tr.Months {
		if m.Month == b.Period {
			bt.PeriodExport, bt.PeriodPct = m.Export, m.ExportPct
		}
	}
	for _, cc := range tr.Countries {
		if !cc.Domestic && len(bt.Markets) < 5 {
			bt.Markets = append(bt.Markets, BriefMarket{Country: cc.Country, Name: cc.Name, Revenue: cc.Revenue, SharePct: cc.SharePct})
		}
	}
	rc := tr.Receivables
	bt.ForeignOpen, bt.ForeignToday, bt.Unrealised, bt.OpenInvoices = rc.LockedHome, rc.TodayHome, rc.Unrealised, rc.Invoices
	if tr.Totals.ExportPct != nil {
		l := fmt.Sprintf("Exports are %s%% of revenue over the last 12 months (%s of %s)", trimFloat(*tr.Totals.ExportPct), money(tr.Totals.Export), money(tr.Totals.Revenue))
		if tr.Totals.PrevExportPct != nil {
			l += fmt.Sprintf("; %s%% in the 12 months before", trimFloat(*tr.Totals.PrevExportPct))
		}
		bt.Lines = append(bt.Lines, l+".")
	} else {
		bt.Lines = append(bt.Lines, "No revenue in the last 12 months to split into domestic and export.")
	}
	if len(bt.Markets) > 0 {
		parts := []string{}
		for _, m := range bt.Markets {
			parts = append(parts, fmt.Sprintf("%s %s (%s%%)", m.Name, money(m.Revenue), trimFloat(m.SharePct)))
		}
		bt.Lines = append(bt.Lines, "Export markets: "+strings.Join(parts, ", ")+".")
	}
	if rc.Invoices > 0 {
		l := fmt.Sprintf("%d open foreign-currency invoice%s worth %s at the rates locked on them", rc.Invoices, plural(rc.Invoices), money(rc.LockedHome))
		if rc.TodayHome > 0 {
			word := "more"
			if rc.Unrealised < 0 {
				word = "less"
			}
			l += fmt.Sprintf(" and %s at today's rates: %s %s (unrealised).", money(rc.TodayHome), money(math.Abs(rc.Unrealised)), word)
		} else {
			l += "."
		}
		bt.Lines = append(bt.Lines, l)
	}
	if tr.Realised.Counted > 0 {
		word := "gain"
		if tr.Realised.Gain < 0 {
			word = "loss"
		}
		bt.Lines = append(bt.Lines, fmt.Sprintf("Realised exchange %s on foreign invoices paid in the last 12 months: %s.", word, money(math.Abs(tr.Realised.Gain))))
	}
	for _, h := range tr.Hedge {
		bt.Hedge = append(bt.Hedge, h.Summary)
	}

	// ---- 4. market signals ----
	b.Signals = i.briefSignals(ctx, org, tr)

	// ---- 5. industry headlines ----
	rules := map[string]string{}
	for _, r := range i.WatchRules(org) {
		rules[r.Key] = r.Label
	}
	for _, it := range i.TopHeadlines(org, from, today, 6) {
		h := BriefHeadline{Title: it.Title, Link: it.Link, Source: it.Source, Score: it.Score, Topics: []string{}}
		for _, t := range it.Topics {
			h.Topics = append(h.Topics, nonEmpty(rules[t], t))
		}
		if it.PublishedAt != nil {
			h.Date = it.PublishedAt.Format(dayFmt)
		} else {
			h.Date = it.FirstSeenAt.Format(dayFmt)
		}
		why := []string{}
		for k, r := range it.Reasons {
			if k >= 3 {
				break
			}
			why = append(why, r.Label)
		}
		h.Why = strings.Join(why, " · ")
		b.Headlines = append(b.Headlines, h)
	}

	// ---- 6 + 7. risks and recommended actions (the opportunity rules) ----
	// Opportunities come ranked (severity, then value): the brief keeps the top
	// six and always adds the exchange-rate and market actions after them.
	opp := i.OpportunitiesFor(org, today)
	rank := map[string]int{"high": 0, "medium": 1, "low": 2}
	const maxOppActions, maxFXActions = 6, 2
	oppActions, fxActions, moreOpps := 0, 0, 0
	for _, o := range opp.Items {
		if o.Type == "risk" {
			b.Risks = append(b.Risks, BriefRisk{Key: o.Key, Severity: o.Severity, Title: o.Title, Text: o.Summary})
			continue
		}
		if oppActions >= maxOppActions {
			moreOpps++
			continue
		}
		oppActions++
		who := []string{}
		for k, it := range o.Items {
			if k >= 3 {
				break
			}
			who = append(who, it.Name)
		}
		rat := strings.TrimRight(o.Summary, ". ") + "."
		if len(who) > 0 {
			rat += " Start with " + joinAnd(who) + "."
		}
		b.Actions = append(b.Actions, BriefAction{Key: o.Key, Type: o.Type, Severity: o.Severity, Title: o.Title, Rationale: rat, Value: o.Value, ValueKind: o.ValueKind, Basis: o.ValueBasis, Link: o.Link})
	}
	if sn.Overdue > 0 && sn.Outstanding > 0 {
		share := sn.Overdue / sn.Outstanding * 100
		sv := "low"
		if share >= 50 {
			sv = "high"
		} else if share >= 20 {
			sv = "medium"
		}
		b.Risks = append(b.Risks, BriefRisk{Key: "overdue", Severity: sv, Title: "Overdue receivables",
			Text: fmt.Sprintf("%s is overdue across %d invoice%s — %s%% of what clients owe you.", money(sn.Overdue), sn.OverdueCount, plural(sn.OverdueCount), trimFloat(r1(share)))})
	}
	if sn.RevenueDelta != nil && *sn.RevenueDelta <= -20 && (sn.RevenueYoY == nil || *sn.RevenueYoY < 0) && !partial {
		b.Risks = append(b.Risks, BriefRisk{Key: "revenue_drop", Severity: "medium", Title: "Revenue fell",
			Text: fmt.Sprintf("Revenue is down %s%% on %s and not ahead of last year — check the pipeline and renewals below.", trimFloat(math.Abs(*sn.RevenueDelta)), prevLabel)})
	}
	if rc.Unrealised < 0 && rc.LockedHome > 0 && -rc.Unrealised/rc.LockedHome >= 0.01 {
		b.Risks = append(b.Risks, BriefRisk{Key: "fx_unrealised", Severity: "low", Title: "Open foreign invoices have lost value",
			Text: fmt.Sprintf("Unpaid foreign-currency invoices are worth %s less at today's rates than when they were issued; collecting sooner limits further loss.", money(-rc.Unrealised))})
	}
	monthly := tr.Totals.Revenue / 12
	for _, h := range tr.Hedge {
		if h.Move5Home == nil || *h.Move5Home <= 0 {
			continue
		}
		sv := "low"
		if monthly > 0 && *h.Move5Home >= monthly*0.10 {
			sv = "medium"
		}
		if h.Net > 0 {
			b.Risks = append(b.Risks, BriefRisk{Key: "fx_" + strings.ToLower(h.Currency), Severity: sv, Title: h.Currency + " exposure", Text: h.Summary})
		} else if fxActions < maxFXActions { // the hedge rows come largest exposure first
			fxActions++
			b.Actions = append(b.Actions, BriefAction{Key: "fx_buffer_" + strings.ToLower(h.Currency), Type: "fx", Severity: sv, Title: "Add an exchange-rate buffer to prices that depend on " + h.Currency + " costs",
				Rationale: h.Summary, Value: *h.Move5Home, ValueKind: "at_risk",
				Basis: fmt.Sprintf("5%% of your net %s costs over the last 12 months at today's rate (1 %s = %s %s).", h.Currency, h.Currency, trimFloat(h.Rate), home), Link: "/insights?tab=trade"})
		}
	}
	if len(bt.Markets) > 0 && tr.Totals.Export > 0 && tr.Totals.ExportPct != nil && *tr.Totals.ExportPct >= 20 {
		top := bt.Markets[0]
		if share := top.Revenue / tr.Totals.Export * 100; share >= 80 {
			b.Risks = append(b.Risks, BriefRisk{Key: "export_single_market", Severity: "medium", Title: "Exports depend on one market",
				Text: fmt.Sprintf("%s%% of export revenue comes from %s. A second market would spread currency and demand risk — see Target markets.", trimFloat(r1(share)), top.Name)})
		}
	}
	// One market action from the target-market table: the best-ranked foreign market.
	if mk := i.MarketsFor(ctx, org, false); mk != nil {
		for _, r := range mk.Rows {
			if r.Home || r.Factors < 3 {
				continue
			}
			title := "Grow " + r.Name + ": your best-scoring export market"
			if r.Revenue <= 0 {
				title = "Test " + r.Name + ": the best-scoring market you are not in"
			}
			b.Actions = append(b.Actions, BriefAction{Key: "market_" + strings.ToLower(r.Country), Type: "market", Severity: "low", Title: title,
				Rationale: fmt.Sprintf("Opportunity score %s of 100 (rank %d of %d). %s", trimFloat(r.Score), r.Rank, len(mk.Rows), r.Note), ValueKind: "none",
				Basis: "Not estimated: the score ranks markets, it does not forecast sales.", Link: "/insights?tab=trade"})
			break
		}
	}
	sort.SliceStable(b.Risks, func(x, y int) bool {
		if rank[b.Risks[x].Severity] != rank[b.Risks[y].Severity] {
			return rank[b.Risks[x].Severity] < rank[b.Risks[y].Severity]
		}
		return b.Risks[x].Key < b.Risks[y].Key
	})
	sort.SliceStable(b.Actions, func(x, y int) bool {
		ax, ay := b.Actions[x], b.Actions[y]
		if rank[ax.Severity] != rank[ay.Severity] {
			return rank[ax.Severity] < rank[ay.Severity]
		}
		if ax.Value != ay.Value {
			return ax.Value > ay.Value
		}
		return ax.Key < ay.Key
	})
	for k := range b.Actions {
		b.Actions[k].Rank = k + 1
		if b.Actions[k].ValueKind == "per_year" || b.Actions[k].ValueKind == "one_off" {
			b.ActionTotal += b.Actions[k].Value
		}
	}
	b.ActionTotal = r2(b.ActionTotal)

	if moreOpps > 0 {
		b.Notes = append(b.Notes, fmt.Sprintf("%d more opportunit%s with a smaller estimate %s listed under Insights → Opportunities.", moreOpps, map[bool]string{true: "y", false: "ies"}[moreOpps == 1], map[bool]string{true: "is", false: "are"}[moreOpps == 1]))
	}
	if partial {
		b.Notes = append(b.Notes, fmt.Sprintf("%s is not over: figures run to %s and are compared with %s.", label, to.Format("2 January"), prevLabel))
	}
	if sn.ForeignRevenue != 0 {
		b.Notes = append(b.Notes, fmt.Sprintf("Revenue and net profit include %s of invoices in other currencies at the rates locked on them. The Profit & loss report lists %s documents only, so it shows revenue %s and net profit %s for the same period.",
			money(sn.ForeignRevenue), home, money(sn.PnLRevenue), money(sn.PnLNetProfit)))
	}
	b.Notes = append(b.Notes, "Revenue, costs and net profit follow the profit & loss report (accrual, excluding tax). Receivables, the FX position and the recommended actions are as of "+today.Format("2 January 2006")+".",
		"Estimates help you prioritise; each action states how its figure was calculated.", c.Note)
	return b, nil
}

// briefSignals turns indicator moves into short signals for the home country
// and the largest export market.
func (i *Intel) briefSignals(ctx context.Context, org string, tr *Trade) []BriefSignal {
	out := []BriefSignal{}
	if i.WB == nil {
		return out
	}
	m := i.MarketFor(ctx, org, false)
	want := []string{}
	if ci, ok := CountryOf(tr.HomeCountry); ok {
		want = append(want, ci.ISO3)
	}
	for _, c := range tr.Countries {
		if !c.Domestic {
			if ci, ok := CountryOf(c.Country); ok && !containsStr(want, ci.ISO3) {
				want = append(want, ci.ISO3)
			}
			break
		}
	}
	for _, code := range []string{"NY.GDP.MKTP.KD.ZG", "FP.CPI.TOTL.ZG", "IT.NET.USER.ZS", "IC.BUS.NREG", "BX.GSR.CCIS.ZS"} {
		for k := range m.Indicators {
			card := &m.Indicators[k]
			if card.Code != code || card.Custom {
				continue
			}
			for _, iso := range want {
				st := statOf(card, iso)
				if st == nil || st.Latest == nil {
					continue
				}
				t := fmt.Sprintf("%s: %s (%d)", st.Name, fnum(*st.Latest, card.Unit), st.LatestYear)
				if st.Change != nil && st.PrevYear > 0 {
					unit := "%"
					if st.ChangeUnit == "pts" {
						unit = " pts"
					}
					t += fmt.Sprintf(", %s%s on %d", signed(*st.Change), unit, st.PrevYear)
				}
				out = append(out, BriefSignal{Title: card.Title, Text: t + ". " + card.Why, Source: "World Bank · " + card.Code, Link: card.SourceURL})
			}
		}
	}
	for _, f := range m.FX {
		if f.Live == nil {
			continue
		}
		t := fmt.Sprintf("1 %s = %s %s today", f.Base, trimFloat(roundSig(*f.Live)), f.Quote)
		if f.YoY != nil && len(f.Annual) > 0 {
			t += fmt.Sprintf("; the %d yearly average was %s%% on the year before", f.Annual[len(f.Annual)-1].Year, signed(*f.YoY))
		}
		out = append(out, BriefSignal{Title: f.Pair, Text: t + ".", Source: nonEmpty(f.LiveSource, "Exchange rates")})
	}
	return out
}

// Headline is the one-line summary of a brief.
func (b *BriefContent) Headline() string {
	sn := b.Snapshot
	parts := []string{fmt.Sprintf("revenue %s %s", b.Currency, amt(sn.Revenue))}
	if sn.RevenueDelta != nil {
		parts[0] += fmt.Sprintf(" (%s%% on %s)", signed(*sn.RevenueDelta), b.PrevLabel)
	}
	parts = append(parts, fmt.Sprintf("net profit %s %s", b.Currency, amt(sn.NetProfit)))
	if b.Trade.ExportPct != nil {
		parts = append(parts, fmt.Sprintf("exports %s%% of 12-month revenue", trimFloat(*b.Trade.ExportPct)))
	}
	if n := len(b.Actions); n > 0 {
		a := fmt.Sprintf("%d recommended action%s", n, plural(n))
		if b.ActionTotal > 0 {
			a += fmt.Sprintf(" worth about %s %s", b.Currency, fmtThousands(b.ActionTotal))
		}
		parts = append(parts, a)
	}
	return b.PeriodLabel + ": " + strings.Join(parts, "; ") + "."
}

// ---------- storage ----------

// GenerateBrief builds the rule-based brief of a month and stores it,
// replacing the rule-based brief already stored for that month.
func (i *Intel) GenerateBrief(ctx context.Context, org, user, period, trigger string, today time.Time, loc *time.Location) (*Brief, error) {
	content, err := i.BuildBrief(ctx, org, period, today, loc)
	if err != nil {
		return nil, err
	}
	return i.storeBrief(org, user, trigger, "rule", content, "", "")
}

func (i *Intel) storeBrief(org, user, trigger, by string, content *BriefContent, aiText, aiModel string) (*Brief, error) {
	now := i.now()
	from, _ := time.Parse(dayFmt, content.From)
	to, _ := time.Parse(dayFmt, content.To)
	var b Brief
	err := i.DB.Where("organization_id = ? AND period = ? AND generated_by = ?", org, content.Period, by).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		b = Brief{ID: uuid.NewString(), OrganizationID: org, Period: content.Period, GeneratedBy: by, CreatedAt: now}
	} else if err != nil {
		return nil, err
	}
	b.PeriodFrom, b.PeriodTo, b.GeneratedAt, b.UpdatedAt = from, to, now, now
	b.Title = "Strategy brief — " + content.PeriodLabel
	b.Headline, b.Content, b.AIText, b.AIModel, b.Trigger, b.CreatedBy = content.Headline(), content, aiText, aiModel, trigger, user
	b.EmailedAt, b.EmailedTo = nil, ""
	if err := i.DB.Save(&b).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

// Briefs lists the stored briefs, newest period first (without content).
func (i *Intel) Briefs(org string) []Brief {
	out := []Brief{}
	i.DB.Omit("content", "ai_text").Where("organization_id = ?", org).Order("period DESC, generated_by DESC").Limit(120).Find(&out)
	return out
}

// BriefByID loads one brief.
func (i *Intel) BriefByID(org, id string) *Brief {
	var b Brief
	if i.DB.Where("organization_id = ? AND id = ?", org, id).First(&b).Error != nil {
		return nil
	}
	return &b
}

// DeleteBrief removes a stored brief.
func (i *Intel) DeleteBrief(org, id string) bool {
	res := i.DB.Where("organization_id = ? AND id = ?", org, id).Delete(&Brief{})
	return res.Error == nil && res.RowsAffected > 0
}

// ---------- AI version ----------

// briefPrompt is the instruction given to the model with the brief's data.
const briefPrompt = `You are writing the monthly strategy brief for the owner of a small services business that sells to clients at home and abroad.

RULES
- Use ONLY the facts and numbers in the JSON below. Do not add figures, names, dates, market facts or news that are not in it. If something is not in the data, leave it out.
- Quote amounts exactly as given, with the currency code in "currency". Do not convert currencies or do arithmetic beyond simple comparisons already implied by the data.
- Plain, direct English for a business owner. No preamble, no disclaimers.

WRITE (at most 380 words, plain text with these headings on their own lines)
Summary — three sentences on how the period went.
What changed — up to four bullets (start each with "- ").
Export and exchange rates — two or three sentences on the export share, the main markets and the foreign-currency position.
What to do next — up to five bullets (start each with "- "), in the order of "actions", each with its estimate when "value" is above zero and one clause of rationale.
Watch — up to three bullets from "risks", "signals" or "headlines".

DATA
`

// WriteBriefAI writes the AI version of a stored rule-based brief from the
// same data and stores it next to it (generated_by = "ai").
func (i *Intel) WriteBriefAI(ctx context.Context, org, user, briefID string) (*Brief, error) {
	if !AIEnabled() {
		return nil, errors.New("AI is not configured (add an Anthropic API key in System settings → AI assistant)")
	}
	src := i.BriefByID(org, briefID)
	if src == nil || src.Content == nil {
		return nil, gorm.ErrRecordNotFound
	}
	if src.GeneratedBy != "rule" {
		if rb := i.ruleBrief(org, src.Period); rb != nil {
			src = rb
		}
	}
	data, err := json.Marshal(src.Content)
	if err != nil {
		return nil, err
	}
	cfg := aiConfig()
	body, _ := json.Marshal(map[string]any{"model": cfg.model, "max_tokens": 900, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "text", "text": briefPrompt + string(data)}}}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", cfg.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return nil, errors.New("the AI service could not be reached")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the AI service returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("unexpected AI response")
	}
	parts := []string{}
	for _, c := range out.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" {
		return nil, errors.New("the AI service returned an empty brief")
	}
	return i.storeBrief(org, user, "manual", "ai", src.Content, clipRunes(text, 12000), cfg.model)
}

func (i *Intel) ruleBrief(org, period string) *Brief {
	var b Brief
	if i.DB.Where("organization_id = ? AND period = ? AND generated_by = 'rule'", org, period).First(&b).Error != nil {
		return nil
	}
	return &b
}

// ---------- email ----------

// briefAdminRoles are the roles that receive the brief by email.
var briefAdminRoles = []string{"owner", "admin", "org_admin", "super_admin"}

// BriefRecipients are the organisation's active administrators with an
// email, as far as Who gets emails (pkg/emailprefs) lets them have it.
func (i *Intel) BriefRecipients(org string) []string {
	var ids []string
	i.DB.Raw(`SELECT id FROM users WHERE organization_id = ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active) AND role IN ? ORDER BY created_at`, org, briefAdminRoles).Scan(&ids)
	v, _ := mail.ParseList(strings.Join(emailprefs.UserEmails(i.DB, org, emailprefs.StrategyBrief, ids), ","))
	return v
}

func (i *Intel) appLink(path string) string {
	base := strings.TrimRight(strings.TrimSpace(i.AppURL), "/")
	if base == "" {
		return ""
	}
	return base + path
}

// BriefMessage renders the brief with the shared email template.
func (i *Intel) BriefMessage(org string, b *Brief, to []string) mail.Message {
	c := b.Content
	money := func(v float64) string { return invoicing.FormatMoney(v, c.Currency) }
	var o struct {
		Name, Email, Phone string
	}
	i.DB.Raw(`SELECT name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone FROM organizations WHERE id = ?`, org).Scan(&o)
	name := nonEmpty(o.Name, "DASYIN ERP")
	var accent []string
	if i.DB.Migrator().HasTable("billing_settings") {
		i.DB.Raw(`SELECT accent_color FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&accent)
	}
	msg := &strings.Builder{}
	if b.GeneratedBy == "ai" && b.AIText != "" {
		msg.WriteString("AI-written from your data — check the figures in the app before acting.\n\n" + b.AIText + "\n")
	} else {
		msg.WriteString("WHAT CHANGED\n")
		for _, ch := range c.Changes {
			msg.WriteString("• " + ch.Text + "\n")
		}
		if len(c.Trade.Lines) > 0 {
			msg.WriteString("\nEXPORT & FX\n")
			for _, l := range c.Trade.Lines {
				msg.WriteString("• " + l + "\n")
			}
		}
		if len(c.Risks) > 0 {
			msg.WriteString("\nRISKS\n")
			for _, r := range c.Risks {
				msg.WriteString("• " + r.Title + " — " + r.Text + "\n")
			}
		}
		if len(c.Headlines) > 0 {
			msg.WriteString("\nINDUSTRY HEADLINES\n")
			for k, h := range c.Headlines {
				if k >= 4 {
					break
				}
				msg.WriteString("• " + h.Title + " (" + h.Source + ") " + h.Link + "\n")
			}
		}
	}
	facts := []invoicing.EmailRow{{Label: "Net profit", Value: money(c.Snapshot.NetProfit)}, {Label: "Costs", Value: money(c.Snapshot.Expenses)}}
	if c.Trade.ExportPct != nil {
		facts = append(facts, invoicing.EmailRow{Label: "Export share (12 months)", Value: trimFloat(*c.Trade.ExportPct) + "%"})
	}
	facts = append(facts, invoicing.EmailRow{Label: "Owed by clients", Value: money(c.Snapshot.Outstanding)})
	if c.Snapshot.Overdue > 0 {
		facts = append(facts, invoicing.EmailRow{Label: "Overdue", Value: money(c.Snapshot.Overdue)})
	}
	lines := []invoicing.EmailLine{}
	for _, a := range c.Actions {
		l := invoicing.EmailLine{Description: fmt.Sprintf("%d. %s", a.Rank, a.Title), Detail: clipWords(a.Rationale, 180)}
		if a.Value > 0 {
			l.Amount = money(a.Value)
		}
		lines = append(lines, l)
	}
	status, tn := "Revenue "+money(c.Snapshot.Revenue), "info"
	if c.Snapshot.RevenueDelta != nil {
		status = fmt.Sprintf("%s%% on %s", signed(*c.Snapshot.RevenueDelta), c.PrevLabel)
		tn = "ok"
		if *c.Snapshot.RevenueDelta < 0 {
			tn = "warn"
		}
	}
	actions := []invoicing.EmailAction{}
	if u := i.appLink("/insights?tab=brief&brief=" + b.ID); u != "" {
		actions = append(actions, invoicing.EmailAction{Label: "Open the brief", URL: u, Primary: true})
	}
	v := invoicing.EmailView{Kind: "reminder", Business: invoicing.EmailBusiness{Name: name, Email: o.Email, Phone: o.Phone}, Preheader: b.Headline, DocLabel: "Strategy brief", Number: c.PeriodLabel,
		Message: strings.TrimSpace(msg.String()), AmountLabel: "Revenue · " + c.PeriodLabel, Amount: money(c.Snapshot.Revenue), StatusLine: status, StatusTone: tn,
		Facts: facts, Lines: lines, Actions: actions,
		Note: "Sent to the administrators of " + name + " when a monthly brief is generated. Turn it off in Insights → Strategy brief."}
	if len(accent) > 0 {
		v.Accent = accent[0]
	}
	if i.LogoURL != nil {
		v.LogoURL = i.LogoURL(org)
	}
	htmlOut, text := invoicing.RenderEmail(v)
	// Subject: the title and the first clause of the headline (the revenue).
	lead := strings.TrimPrefix(b.Headline, c.PeriodLabel+": ")
	if k := strings.Index(lead, ";"); k > 0 {
		lead = lead[:k]
	}
	return mail.Message{To: to, Subject: b.Title + ": " + strings.TrimSuffix(lead, "."), HTML: htmlOut, Text: text}
}

// EmailBrief sends a stored brief to the organisation's administrators.
func (i *Intel) EmailBrief(ctx context.Context, org, id string) ([]string, error) {
	b := i.BriefByID(org, id)
	if b == nil || b.Content == nil {
		return nil, gorm.ErrRecordNotFound
	}
	if i.Mail == nil {
		return nil, errors.New("outgoing email is not set up on this server")
	}
	to := i.BriefRecipients(org)
	if len(to) == 0 {
		return nil, errors.New("no administrators with an email address to send the brief to")
	}
	if _, err := i.Mail.SendAs(ctx, org, i.BriefMessage(org, b, to)); err != nil {
		return nil, err
	}
	now := i.now()
	i.DB.Model(&Brief{}).Where("id = ?", b.ID).Updates(map[string]any{"emailed_at": now, "emailed_to": strings.Join(to, ", ")})
	return to, nil
}
