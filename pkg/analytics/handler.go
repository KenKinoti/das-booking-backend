package analytics

import (
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Handler serves /analytics endpoints.
type Handler struct {
	DB *gorm.DB
	// Now is overridable in tests.
	Now func() time.Time
}

// NewHandler creates an analytics handler.
func NewHandler(db *gorm.DB) *Handler { return &Handler{DB: db, Now: time.Now} }

// RegisterRoutes mounts the endpoints on a group (e.g. /api/v1/analytics).
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("/overview", h.Overview)
}

func orgFrom(c *gin.Context) string {
	for _, k := range []string{"org_id", "organization_id"} {
		if v, ok := c.Get(k); ok && v != nil {
			if s := fmt.Sprint(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// ---------- response types ----------

// KPI is a headline number with a period-over-period comparison.
type KPI struct {
	Value    float64   `json:"value"`
	Previous *float64  `json:"previous"`
	DeltaPct *float64  `json:"delta_pct"`
	Count    int64     `json:"count,omitempty"`
	Spark    []float64 `json:"spark"`
}

// SeriesPoint is one bucket of the time series.
type SeriesPoint struct {
	Date         string  `json:"date"`
	Invoiced     float64 `json:"invoiced"`
	Collected    float64 `json:"collected"`
	POS          float64 `json:"pos"`
	Revenue      float64 `json:"revenue"` // collected + pos
	Bookings     int64   `json:"bookings"`
	NewCustomers int64   `json:"new_customers"`
	// Comparison period values aligned by index (nil when compare=none).
	PrevDate     string   `json:"prev_date,omitempty"`
	PrevInvoiced *float64 `json:"prev_invoiced,omitempty"`
	PrevRevenue  *float64 `json:"prev_revenue,omitempty"`
}

type named struct {
	Name   string  `json:"name"`
	ID     string  `json:"id,omitempty"`
	Amount float64 `json:"amount"`
	Count  int64   `json:"count"`
	Extra  float64 `json:"extra,omitempty"`
}

type topCustomer struct {
	Name     string  `json:"name"`
	ID       string  `json:"id,omitempty"`
	Invoices int64   `json:"invoices"`
	Invoiced float64 `json:"invoiced"`
	Paid     float64 `json:"paid"`
	Balance  float64 `json:"balance"`
	POS      float64 `json:"pos"`
	Total    float64 `json:"total"`
}

type topItem struct {
	Name     string  `json:"name"`
	Kind     string  `json:"kind"` // service | product
	Revenue  float64 `json:"revenue"`
	Quantity float64 `json:"quantity"`
	Bookings int64   `json:"bookings"`
	Sources  string  `json:"sources"`
}

type agingBucket struct {
	Key    string  `json:"key"`
	Label  string  `json:"label"`
	Amount float64 `json:"amount"`
	Count  int64   `json:"count"`
}

type cashDay struct {
	Date       string  `json:"date"`
	Amount     float64 `json:"amount"`
	Count      int64   `json:"count"`
	Cumulative float64 `json:"cumulative"`
}

type activity struct {
	Kind   string   `json:"kind"` // invoice | payment | booking | customer | pos
	Type   string   `json:"type"`
	Title  string   `json:"title"`
	Detail string   `json:"detail"`
	Amount *float64 `json:"amount,omitempty"`
	// Currency of Amount when it differs from the organisation's currency.
	Currency string     `json:"currency,omitempty"`
	At       time.Time  `json:"at"`
	Ref      *time.Time `json:"ref_time,omitempty"`
	Link     string     `json:"link,omitempty"`
}

type upcomingBooking struct {
	ID        string    `json:"id"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Status    string    `json:"status"`
	Total     float64   `json:"total_price"`
	Customer  string    `json:"customer"`
	Services  string    `json:"services"`
}

type lowStockItem struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	SKU          string  `json:"sku"`
	CurrentStock int64   `json:"current_stock"`
	MinStock     int64   `json:"min_stock"`
	ReorderPoint int64   `json:"reorder_point"`
	CostPrice    float64 `json:"cost_price"`
}

// ---------- helpers ----------

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func delta(cur, prev float64) *float64 {
	if prev == 0 {
		if cur == 0 {
			z := 0.0
			return &z
		}
		return nil
	}
	d := math.Round((cur-prev)/math.Abs(prev)*1000) / 10
	return &d
}

func fp(v float64) *float64 { v = round2(v); return &v }

// scan runs a raw query; errors are logged and yield an empty result so a
// missing table or column never breaks the dashboard.
func (h *Handler) scan(dest interface{}, q string, args ...interface{}) {
	if err := h.DB.Raw(q, args...).Scan(dest).Error; err != nil {
		log.Printf("analytics: query failed: %v", err)
	}
}

type dayRow struct {
	D time.Time `gorm:"column:d"`
	N int64     `gorm:"column:n"`
	V float64   `gorm:"column:v"`
}

// currency is the organisation's home currency (pkg/orgprefs).
func (h *Handler) currency(org string) string { return orgprefs.Currency(h.DB, org) }

// Currency exposes the organisation's invoicing currency (used by the market module).
func Currency(db *gorm.DB, org string) string { return (&Handler{DB: db}).currency(org) }

// ---------- overview ----------

// Overview returns KPIs, series, breakdowns and lists for a date range.
// Query: from, to (YYYY-MM-DD), range (7d|30d|90d|12m|mtd|qtd|ytd),
// compare (previous|year|none), interval (auto|day|week|month), tz (IANA),
// limit (top list size, default 6, max 50).
func (h *Handler) Overview(c *gin.Context) {
	org := orgFrom(c)
	if org == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": gin.H{"code": "NO_ORGANIZATION", "message": "Organization not found in token"}})
		return
	}
	loc := time.UTC
	if tz := c.Query("tz"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	now := h.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	cur, prev, err := resolvePeriods(today, c.Query("from"), c.Query("to"), c.DefaultQuery("range", "30d"), c.DefaultQuery("compare", "previous"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": gin.H{"code": "INVALID_RANGE", "message": err.Error()}})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "6"))
	if limit <= 0 {
		limit = 6
	}
	if limit > 50 {
		limit = 50
	}
	interval := pickInterval(c.DefaultQuery("interval", "auto"), cur.Days())
	tzName := loc.String()
	// Money is reported in the organisation's currency only; documents in other
	// currencies are counted separately (other_currencies) instead of being
	// added to these totals.
	curCode := h.currency(org)

	// The daily queries span both periods at once.
	span := cur
	if prev != nil && prev.From.Before(span.From) {
		span.From = prev.From
	}
	spanStart, spanEnd := span.Bounds(loc)
	sFrom, sTo := span.From.Format(dayFmt), span.To.Format(dayFmt)

	var invoiced, collected, pos, bookings, customers []dayRow
	h.scan(&invoiced, `SELECT issue_date::date AS d, COUNT(*) AS n, COALESCE(SUM(total),0) AS v
		FROM billing_invoices
		WHERE organization_id = ? AND doc_type = 'invoice' AND status NOT IN ('draft','void') AND deleted_at IS NULL
		  AND COALESCE(NULLIF(currency,''), ?) = ?
		  AND issue_date >= ?::date AND issue_date <= ?::date
		GROUP BY 1`, org, curCode, curCode, sFrom, sTo)
	h.scan(&collected, `SELECT p.date::date AS d, COUNT(*) AS n, COALESCE(SUM(p.amount),0) AS v
		FROM billing_payments p JOIN billing_invoices i ON i.id = p.invoice_id AND i.deleted_at IS NULL
		WHERE p.organization_id = ? AND COALESCE(NULLIF(i.currency,''), ?) = ? AND p.date >= ?::date AND p.date <= ?::date
		GROUP BY 1`, org, curCode, curCode, sFrom, sTo)
	h.scan(&pos, `SELECT (created_at AT TIME ZONE ?)::date AS d, COUNT(*) AS n, COALESCE(SUM(total_amount),0) AS v
		FROM pos_transactions
		WHERE organization_id = ? AND deleted_at IS NULL AND status = 'completed' AND type = 'sale'
		  AND UPPER(COALESCE(NULLIF(currency,''), ?)) = ?
		  AND created_at >= ? AND created_at < ?
		GROUP BY 1`, tzName, org, curCode, curCode, spanStart, spanEnd)
	h.scan(&bookings, `SELECT (start_time AT TIME ZONE ?)::date AS d, COUNT(*) AS n,
		  COALESCE(SUM(CASE WHEN status NOT IN ('cancelled','no_show') THEN total_price ELSE 0 END),0) AS v
		FROM bookings
		WHERE organization_id = ? AND deleted_at IS NULL AND start_time >= ? AND start_time < ?
		GROUP BY 1`, tzName, org, spanStart, spanEnd)
	h.scan(&customers, `SELECT (created_at AT TIME ZONE ?)::date AS d, COUNT(*) AS n, 0 AS v
		FROM customers
		WHERE organization_id = ? AND deleted_at IS NULL AND created_at >= ? AND created_at < ?
		GROUP BY 1`, tzName, org, spanStart, spanEnd)

	// Totals per period
	type tot struct{ N, V float64 }
	sum := func(rows []dayRow, p Period) tot {
		var t tot
		for _, r := range rows {
			if p.Contains(r.D) {
				t.N += float64(r.N)
				t.V += r.V
			}
		}
		return t
	}
	// Series
	curB := buckets(cur, interval)
	idx := map[string]int{}
	series := make([]SeriesPoint, len(curB))
	for i, b := range curB {
		series[i] = SeriesPoint{Date: b.Format(dayFmt)}
		idx[b.Format(dayFmt)] = i
	}
	var prevB []time.Time
	prevIdx := map[string]int{}
	prevRev := make([]float64, len(curB))
	prevInv := make([]float64, len(curB))
	if prev != nil {
		prevB = buckets(*prev, interval)
		for i, b := range prevB {
			prevIdx[b.Format(dayFmt)] = i
		}
	}
	fold := func(rows []dayRow, apply func(i int, r dayRow), applyPrev func(i int, r dayRow)) {
		for _, r := range rows {
			if cur.Contains(r.D) {
				if i, ok := idx[bucketStart(r.D, interval).Format(dayFmt)]; ok {
					apply(i, r)
				}
			} else if prev != nil && applyPrev != nil && prev.Contains(r.D) {
				if i, ok := prevIdx[bucketStart(r.D, interval).Format(dayFmt)]; ok && i < len(curB) {
					applyPrev(i, r)
				}
			}
		}
	}
	fold(invoiced, func(i int, r dayRow) { series[i].Invoiced += r.V }, func(i int, r dayRow) { prevInv[i] += r.V })
	fold(collected, func(i int, r dayRow) { series[i].Collected += r.V }, func(i int, r dayRow) { prevRev[i] += r.V })
	fold(pos, func(i int, r dayRow) { series[i].POS += r.V }, func(i int, r dayRow) { prevRev[i] += r.V })
	fold(bookings, func(i int, r dayRow) { series[i].Bookings += r.N }, nil)
	fold(customers, func(i int, r dayRow) { series[i].NewCustomers += r.N }, nil)
	for i := range series {
		series[i].Invoiced = round2(series[i].Invoiced)
		series[i].Collected = round2(series[i].Collected)
		series[i].POS = round2(series[i].POS)
		series[i].Revenue = round2(series[i].Collected + series[i].POS)
		if prev != nil && i < len(prevB) {
			series[i].PrevDate = prevB[i].Format(dayFmt)
			series[i].PrevRevenue = fp(prevRev[i])
			series[i].PrevInvoiced = fp(prevInv[i])
		}
	}

	// KPIs
	kpi := func(rows []dayRow, useCount bool, spark func(p SeriesPoint) float64) KPI {
		c := sum(rows, cur)
		v := c.V
		if useCount {
			v = c.N
		}
		k := KPI{Value: round2(v), Count: int64(c.N), Spark: make([]float64, len(series))}
		for i, p := range series {
			k.Spark[i] = spark(p)
		}
		if prev != nil {
			p := sum(rows, *prev)
			pv := p.V
			if useCount {
				pv = p.N
			}
			k.Previous = fp(pv)
			k.DeltaPct = delta(v, pv)
		}
		return k
	}
	kInvoiced := kpi(invoiced, false, func(p SeriesPoint) float64 { return p.Invoiced })
	kCollectedInv := kpi(collected, false, func(p SeriesPoint) float64 { return p.Collected })
	kPOS := kpi(pos, false, func(p SeriesPoint) float64 { return p.POS })
	kBookings := kpi(bookings, true, func(p SeriesPoint) float64 { return float64(p.Bookings) })
	kCustomers := kpi(customers, true, func(p SeriesPoint) float64 { return float64(p.NewCustomers) })
	kBookingValue := kpi(bookings, false, func(p SeriesPoint) float64 { return 0 })
	kBookingValue.Spark = nil

	kRevenue := KPI{Value: round2(kCollectedInv.Value + kPOS.Value), Count: kCollectedInv.Count + kPOS.Count, Spark: make([]float64, len(series))}
	for i, p := range series {
		kRevenue.Spark[i] = p.Revenue
	}
	if prev != nil {
		pv := *kCollectedInv.Previous + *kPOS.Previous
		kRevenue.Previous = fp(pv)
		kRevenue.DeltaPct = delta(kRevenue.Value, pv)
	}
	avg := func(total, n float64) float64 {
		if n == 0 {
			return 0
		}
		return round2(total / n)
	}
	kAvg := KPI{Value: avg(kInvoiced.Value, float64(kInvoiced.Count)), Count: kInvoiced.Count, Spark: make([]float64, len(series))}
	for i := range series {
		kAvg.Spark[i] = 0
	}
	// average per bucket from the daily rows
	{
		cnt := make([]float64, len(series))
		for _, r := range invoiced {
			if cur.Contains(r.D) {
				if i, ok := idx[bucketStart(r.D, interval).Format(dayFmt)]; ok {
					cnt[i] += float64(r.N)
				}
			}
		}
		for i := range series {
			kAvg.Spark[i] = avg(series[i].Invoiced, cnt[i])
		}
	}
	if prev != nil {
		pa := avg(*kInvoiced.Previous, sum(invoiced, *prev).N)
		kAvg.Previous = fp(pa)
		kAvg.DeltaPct = delta(kAvg.Value, pa)
	}
	collectionRate := KPI{}
	if kInvoiced.Value > 0 {
		collectionRate.Value = math.Round(kCollectedInv.Value/kInvoiced.Value*1000) / 10
	}
	if prev != nil && *kInvoiced.Previous > 0 {
		pr := math.Round(*kCollectedInv.Previous / *kInvoiced.Previous * 1000) / 10
		collectionRate.Previous = &pr
		d := math.Round((collectionRate.Value-pr)*10) / 10 // percentage points
		collectionRate.DeltaPct = &d
	}

	// Receivables snapshot: aging + cash-flow forecast (one query)
	var open []struct {
		Due time.Time `gorm:"column:due"`
		N   int64     `gorm:"column:n"`
		V   float64   `gorm:"column:v"`
	}
	h.scan(&open, `SELECT due_date::date AS due, COUNT(*) AS n, COALESCE(SUM(balance_due),0) AS v
		FROM billing_invoices
		WHERE organization_id = ? AND doc_type = 'invoice' AND status IN ('sent','partial') AND deleted_at IS NULL AND balance_due > 0
		  AND COALESCE(NULLIF(currency,''), ?) = ?
		GROUP BY 1`, org, curCode, curCode)
	aging := []agingBucket{
		{Key: "current", Label: "Not yet due"},
		{Key: "1_30", Label: "1–30 days"},
		{Key: "31_60", Label: "31–60 days"},
		{Key: "61_90", Label: "61–90 days"},
		{Key: "90_plus", Label: "90+ days"},
	}
	var outstanding, overdue float64
	var outstandingN, overdueN int64
	horizon := 30
	cash := make([]cashDay, horizon)
	for i := range cash {
		cash[i].Date = today.AddDate(0, 0, i).Format(dayFmt)
	}
	var cashOverdue, cashLater float64
	for _, o := range open {
		due := dateOnly(o.Due)
		outstanding += o.V
		outstandingN += o.N
		days := int(today.Sub(due).Hours() / 24)
		b := 0
		switch {
		case days <= 0:
			b = 0
		case days <= 30:
			b = 1
		case days <= 60:
			b = 2
		case days <= 90:
			b = 3
		default:
			b = 4
		}
		aging[b].Amount += o.V
		aging[b].Count += o.N
		if days > 0 {
			overdue += o.V
			overdueN += o.N
			cashOverdue += o.V
		} else if -days < horizon {
			cash[-days].Amount += o.V
			cash[-days].Count += o.N
		} else {
			cashLater += o.V
		}
	}
	for i := range aging {
		aging[i].Amount = round2(aging[i].Amount)
	}
	run := 0.0
	for i := range cash {
		cash[i].Amount = round2(cash[i].Amount)
		run += cash[i].Amount
		cash[i].Cumulative = round2(run)
	}

	// Breakdowns for the current period
	cFrom, cTo := cur.From.Format(dayFmt), cur.To.Format(dayFmt)
	cStart, cEnd := cur.Bounds(loc)

	var topCust []topCustomer
	h.scan(&topCust, `SELECT COALESCE(NULLIF(TRIM(client_name),''),'Unnamed client') AS name, COALESCE(MAX(customer_id),'') AS id,
		  COUNT(*) AS invoices, COALESCE(SUM(total),0) AS invoiced, COALESCE(SUM(amount_paid),0) AS paid, COALESCE(SUM(balance_due),0) AS balance
		FROM billing_invoices
		WHERE organization_id = ? AND doc_type = 'invoice' AND status NOT IN ('draft','void') AND deleted_at IS NULL
		  AND COALESCE(NULLIF(currency,''), ?) = ?
		  AND issue_date >= ?::date AND issue_date <= ?::date
		GROUP BY 1 ORDER BY invoiced DESC LIMIT ?`, org, curCode, curCode, cFrom, cTo, limit)
	var posCust []struct {
		ID   string  `gorm:"column:id"`
		Name string  `gorm:"column:name"`
		V    float64 `gorm:"column:v"`
		N    int64   `gorm:"column:n"`
	}
	h.scan(&posCust, `SELECT t.customer_id AS id, TRIM(COALESCE(c.first_name,'') || ' ' || COALESCE(c.last_name,'')) AS name,
		  COALESCE(SUM(t.total_amount),0) AS v, COUNT(*) AS n
		FROM pos_transactions t LEFT JOIN customers c ON c.id = t.customer_id
		WHERE t.organization_id = ? AND t.deleted_at IS NULL AND t.status = 'completed' AND t.type = 'sale' AND t.customer_id IS NOT NULL
		  AND UPPER(COALESCE(NULLIF(t.currency,''), ?)) = ?
		  AND t.created_at >= ? AND t.created_at < ?
		GROUP BY 1,2 ORDER BY v DESC LIMIT ?`, org, curCode, curCode, cStart, cEnd, limit)
	for _, pc := range posCust {
		merged := false
		for i := range topCust {
			if (pc.ID != "" && topCust[i].ID == pc.ID) || strings.EqualFold(topCust[i].Name, pc.Name) {
				topCust[i].POS += pc.V
				merged = true
				break
			}
		}
		if !merged && pc.Name != "" {
			topCust = append(topCust, topCustomer{Name: pc.Name, ID: pc.ID, POS: pc.V})
		}
	}
	for i := range topCust {
		t := &topCust[i]
		t.Total = round2(t.Invoiced + t.POS)
		t.Invoiced, t.Paid, t.Balance, t.POS = round2(t.Invoiced), round2(t.Paid), round2(t.Balance), round2(t.POS)
	}
	sort.SliceStable(topCust, func(i, j int) bool { return topCust[i].Total > topCust[j].Total })
	if len(topCust) > limit {
		topCust = topCust[:limit]
	}
	if topCust == nil {
		topCust = []topCustomer{}
	}

	// Items: invoice lines + POS lines + booked services
	type itemRow struct {
		Name string  `gorm:"column:name"`
		Kind string  `gorm:"column:kind"`
		Q    float64 `gorm:"column:q"`
		V    float64 `gorm:"column:v"`
		N    int64   `gorm:"column:n"`
	}
	var invItems, posItems, booked []itemRow
	h.scan(&invItems, `SELECT COALESCE(NULLIF(TRIM(it.description),''),'Unnamed item') AS name, COALESCE(it.item_type,'service') AS kind,
		  COALESCE(SUM(it.quantity),0) AS q, COALESCE(SUM(it.line_total),0) AS v, COUNT(DISTINCT i.id) AS n
		FROM billing_invoice_items it JOIN billing_invoices i ON i.id = it.invoice_id
		WHERE i.organization_id = ? AND i.doc_type = 'invoice' AND i.status NOT IN ('draft','void') AND i.deleted_at IS NULL
		  AND COALESCE(NULLIF(i.currency,''), ?) = ?
		  AND i.issue_date >= ?::date AND i.issue_date <= ?::date
		GROUP BY 1,2 ORDER BY v DESC LIMIT 200`, org, curCode, curCode, cFrom, cTo)
	h.scan(&posItems, `SELECT COALESCE(NULLIF(TRIM(pi.item_name),''),'Unnamed item') AS name, COALESCE(pi.item_type,'product') AS kind,
		  COALESCE(SUM(pi.quantity),0) AS q, COALESCE(SUM(pi.total_price),0) AS v, COUNT(DISTINCT t.id) AS n
		FROM pos_items pi JOIN pos_transactions t ON t.id = pi.transaction_id
		WHERE t.organization_id = ? AND t.deleted_at IS NULL AND t.status = 'completed' AND t.type = 'sale'
		  AND t.created_at >= ? AND t.created_at < ?
		GROUP BY 1,2 ORDER BY v DESC LIMIT 200`, org, cStart, cEnd)
	h.scan(&booked, `SELECT s.name AS name, 'service' AS kind, COUNT(*) AS q, COALESCE(SUM(s.price),0) AS v, COUNT(*) AS n
		FROM booking_services bs
		JOIN bookings b ON b.id = bs.booking_id
		JOIN services s ON s.id = bs.service_id
		WHERE b.organization_id = ? AND b.deleted_at IS NULL AND b.status NOT IN ('cancelled','no_show')
		  AND b.start_time >= ? AND b.start_time < ?
		GROUP BY 1 ORDER BY q DESC LIMIT 200`, org, cStart, cEnd)
	items := map[string]*topItem{}
	order := []string{}
	add := func(r itemRow, source string, isBooking bool) {
		kind := "service"
		if strings.EqualFold(r.Kind, "product") {
			kind = "product"
		}
		if strings.EqualFold(r.Kind, "discount") || strings.EqualFold(r.Kind, "fee") {
			return
		}
		key := kind + "|" + strings.ToLower(strings.TrimSpace(r.Name))
		it, ok := items[key]
		if !ok {
			it = &topItem{Name: strings.TrimSpace(r.Name), Kind: kind}
			items[key] = it
			order = append(order, key)
		}
		if isBooking {
			it.Bookings += r.N
		} else {
			it.Revenue += r.V
			it.Quantity += r.Q
		}
		if !strings.Contains(it.Sources, source) {
			if it.Sources != "" {
				it.Sources += ", "
			}
			it.Sources += source
		}
	}
	for _, r := range invItems {
		add(r, "invoices", false)
	}
	for _, r := range posItems {
		add(r, "POS", false)
	}
	for _, r := range booked {
		add(r, "bookings", true)
	}
	topServices, topProducts := []topItem{}, []topItem{}
	for _, k := range order {
		it := items[k]
		it.Revenue = round2(it.Revenue)
		it.Quantity = round2(it.Quantity)
		if it.Kind == "product" {
			topProducts = append(topProducts, *it)
		} else {
			topServices = append(topServices, *it)
		}
	}
	rank := func(list []topItem) []topItem {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Revenue != list[j].Revenue {
				return list[i].Revenue > list[j].Revenue
			}
			return list[i].Bookings > list[j].Bookings
		})
		if len(list) > limit {
			list = list[:limit]
		}
		return list
	}
	topServices, topProducts = rank(topServices), rank(topProducts)

	// Booking status mix (current period)
	bookingStatus := []named{}
	h.scan(&bookingStatus, `SELECT COALESCE(NULLIF(status,''),'scheduled') AS name, COUNT(*) AS count, COALESCE(SUM(total_price),0) AS amount
		FROM bookings WHERE organization_id = ? AND deleted_at IS NULL AND start_time >= ? AND start_time < ?
		GROUP BY 1 ORDER BY count DESC`, org, cStart, cEnd)

	// Invoice status mix (current period, overdue derived)
	invoiceStatus := []named{}
	h.scan(&invoiceStatus, `SELECT CASE WHEN status IN ('sent','partial') AND due_date < ?::date THEN 'overdue' ELSE status END AS name,
		  COUNT(*) AS count, COALESCE(SUM(total),0) AS amount, COALESCE(SUM(balance_due),0) AS extra
		FROM billing_invoices
		WHERE organization_id = ? AND doc_type = 'invoice' AND deleted_at IS NULL AND issue_date >= ?::date AND issue_date <= ?::date
		  AND COALESCE(NULLIF(currency,''), ?) = ?
		GROUP BY 1 ORDER BY amount DESC`, today.Format(dayFmt), org, cFrom, cTo, curCode, curCode)

	// Payment methods (invoice payments + POS tenders)
	payMethods := []named{}
	h.scan(&payMethods, `SELECT name, SUM(n) AS count, SUM(v) AS amount FROM (
		  SELECT LOWER(COALESCE(NULLIF(p.method,''),'other')) AS name, COUNT(*) AS n, COALESCE(SUM(p.amount),0) AS v
		  FROM billing_payments p JOIN billing_invoices i ON i.id = p.invoice_id AND i.deleted_at IS NULL
		  WHERE p.organization_id = ? AND COALESCE(NULLIF(i.currency,''), ?) = ? AND p.date >= ?::date AND p.date <= ?::date GROUP BY 1
		  UNION ALL
		  SELECT name, COUNT(*) AS n, COALESCE(SUM(share),0) AS v FROM (
		    SELECT LOWER(COALESCE(NULLIF(pp.method,''),'other')) AS name,
		      t.total_amount * pp.amount / NULLIF(SUM(pp.amount) OVER (PARTITION BY pp.transaction_id),0) AS share
		    FROM pos_payments pp JOIN pos_transactions t ON t.id = pp.transaction_id
		    WHERE t.organization_id = ? AND t.deleted_at IS NULL AND t.status = 'completed' AND t.type = 'sale'
		      AND t.created_at >= ? AND t.created_at < ?
		  ) pp GROUP BY 1
		) x GROUP BY name ORDER BY amount DESC`, org, curCode, curCode, cFrom, cTo, org, cStart, cEnd)

	for _, l := range [][]named{bookingStatus, invoiceStatus, payMethods} {
		for i := range l {
			l[i].Amount = round2(l[i].Amount)
			l[i].Extra = round2(l[i].Extra)
		}
	}

	// Inventory snapshot
	var inv struct {
		Products    int64   `gorm:"column:products"`
		CostValue   float64 `gorm:"column:cost_value"`
		RetailValue float64 `gorm:"column:retail_value"`
		Units       int64   `gorm:"column:units"`
		Low         int64   `gorm:"column:low"`
		Out         int64   `gorm:"column:out"`
	}
	h.scan(&inv, `SELECT COUNT(*) AS products,
		  COALESCE(SUM(GREATEST(current_stock,0) * cost_price),0) AS cost_value,
		  COALESCE(SUM(GREATEST(current_stock,0) * selling_price),0) AS retail_value,
		  COALESCE(SUM(GREATEST(current_stock,0)),0) AS units,
		  COUNT(*) FILTER (WHERE current_stock > 0 AND GREATEST(min_stock, reorder_point) > 0 AND current_stock <= GREATEST(min_stock, reorder_point)) AS low,
		  COUNT(*) FILTER (WHERE current_stock <= 0) AS out
		FROM products WHERE organization_id = ? AND deleted_at IS NULL AND is_active = true`, org)
	lowStock := []lowStockItem{}
	h.scan(&lowStock, `SELECT id, name, COALESCE(sku,'') AS sku, current_stock, min_stock, reorder_point, cost_price
		FROM products WHERE organization_id = ? AND deleted_at IS NULL AND is_active = true
		  AND current_stock <= GREATEST(min_stock, reorder_point)
		ORDER BY current_stock ASC, name ASC LIMIT ?`, org, limit+2)

	// Bookings: today + next 7 days
	todayStart, todayEnd := Period{From: today, To: today}.Bounds(loc)
	weekEnd := todayStart.AddDate(0, 0, 7)
	var bookingsToday, bookingsWeek int64
	// today: every live booking today (as on the Bookings page "Today" card);
	// week: still-to-do bookings (scheduled/confirmed/in progress) from today
	// for 7 days (as on the Bookings page "Next 7 days" card).
	h.DB.Raw(`SELECT COUNT(*) FILTER (WHERE start_time < ?),
		  COUNT(*) FILTER (WHERE COALESCE(NULLIF(status,''),'scheduled') IN ('scheduled','confirmed','in_progress')) FROM bookings
		WHERE organization_id = ? AND deleted_at IS NULL AND status NOT IN ('cancelled','no_show') AND start_time >= ? AND start_time < ?`,
		todayEnd, org, todayStart, weekEnd).Row().Scan(&bookingsToday, &bookingsWeek)
	upcoming := []upcomingBooking{}
	h.scan(&upcoming, `SELECT b.id, b.start_time, b.end_time, COALESCE(NULLIF(b.status,''),'scheduled') AS status, b.total_price AS total,
		  TRIM(COALESCE(c.first_name,'') || ' ' || COALESCE(c.last_name,'')) AS customer,
		  COALESCE(STRING_AGG(s.name, ', ' ORDER BY s.name), '') AS services
		FROM bookings b
		LEFT JOIN customers c ON c.id = b.customer_id
		LEFT JOIN booking_services bs ON bs.booking_id = b.id
		LEFT JOIN services s ON s.id = bs.service_id
		WHERE b.organization_id = ? AND b.deleted_at IS NULL AND b.status NOT IN ('cancelled','no_show','completed')
		  AND b.end_time >= ? AND b.start_time < ?
		GROUP BY b.id, c.first_name, c.last_name
		ORDER BY b.start_time ASC LIMIT ?`, org, now, weekEnd, limit+2)

	// Invoices in other currencies are excluded from the totals above; report
	// them separately so the UI can say so.
	type otherCur struct {
		Currency    string  `json:"currency"`
		Invoices    int64   `json:"invoices"`
		Invoiced    float64 `json:"invoiced"`
		Open        int64   `json:"open"`
		Outstanding float64 `json:"outstanding"`
	}
	others := []otherCur{}
	h.scan(&others, `SELECT UPPER(currency) AS currency,
		  COUNT(*) FILTER (WHERE status NOT IN ('draft','void') AND issue_date >= ?::date AND issue_date <= ?::date) AS invoices,
		  COALESCE(SUM(total) FILTER (WHERE status NOT IN ('draft','void') AND issue_date >= ?::date AND issue_date <= ?::date),0) AS invoiced,
		  COUNT(*) FILTER (WHERE status IN ('sent','partial') AND balance_due > 0) AS open,
		  COALESCE(SUM(balance_due) FILTER (WHERE status IN ('sent','partial') AND balance_due > 0),0) AS outstanding
		FROM billing_invoices
		WHERE organization_id = ? AND doc_type = 'invoice' AND deleted_at IS NULL AND COALESCE(currency,'') <> '' AND UPPER(currency) <> ?
		GROUP BY 1 ORDER BY 1`, cFrom, cTo, cFrom, cTo, org, curCode)
	for i := range others {
		others[i].Invoiced, others[i].Outstanding = round2(others[i].Invoiced), round2(others[i].Outstanding)
	}

	// Recent activity
	acts := h.recentActivity(org)

	sources := []named{
		{Name: "Invoice payments", ID: "invoices", Amount: kCollectedInv.Value, Count: kCollectedInv.Count},
		{Name: "Point of sale", ID: "pos", Amount: kPOS.Value, Count: kPOS.Count},
	}

	var cmp gin.H
	if prev != nil {
		cmp = gin.H{"from": prev.From.Format(dayFmt), "to": prev.To.Format(dayFmt), "days": prev.Days()}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"currency":         curCode,
		"other_currencies": others,
		"timezone":         tzName,
		"generated_at":     time.Now().UTC(),
		"range":            gin.H{"from": cFrom, "to": cTo, "days": cur.Days(), "interval": interval},
		"compare":          cmp,
		"kpis": gin.H{
			"revenue":            kRevenue,
			"collected":          kCollectedInv,
			"invoiced":           kInvoiced,
			"pos_sales":          kPOS,
			"avg_invoice":        kAvg,
			"new_customers":      kCustomers,
			"bookings":           kBookings,
			"booking_value":      kBookingValue,
			"collection_rate":    collectionRate,
			"outstanding":        KPI{Value: round2(outstanding), Count: outstandingN, Spark: []float64{}},
			"overdue":            KPI{Value: round2(overdue), Count: overdueN, Spark: []float64{}},
			"stock_value":        KPI{Value: round2(inv.CostValue), Count: inv.Products, Spark: []float64{}},
			"low_stock":          KPI{Value: float64(inv.Low), Count: inv.Out, Spark: []float64{}},
			"bookings_today":     KPI{Value: float64(bookingsToday), Count: bookingsWeek, Spark: []float64{}},
			"invoice_count":      KPI{Value: float64(kInvoiced.Count), Previous: kpiCount(prev, invoiced), Spark: []float64{}},
			"pos_transactions":   KPI{Value: float64(kPOS.Count), Previous: kpiCount(prev, pos), Spark: []float64{}},
			"payments_collected": KPI{Value: float64(kCollectedInv.Count), Previous: kpiCount(prev, collected), Spark: []float64{}},
		},
		"series":            series,
		"revenue_by_source": sources,
		"top_customers":     topCust,
		"top_services":      topServices,
		"top_products":      topProducts,
		"booking_status":    bookingStatus,
		"invoice_status":    invoiceStatus,
		"payment_methods":   payMethods,
		"aging":             aging,
		"cashflow": gin.H{
			"horizon_days": horizon,
			"overdue":      round2(cashOverdue),
			"expected":     round2(run),
			"later":        round2(cashLater),
			"days":         cash,
		},
		"inventory": gin.H{
			"products":     inv.Products,
			"units":        inv.Units,
			"cost_value":   round2(inv.CostValue),
			"retail_value": round2(inv.RetailValue),
			"low_stock":    inv.Low,
			"out_of_stock": inv.Out,
			"items":        lowStock,
		},
		"bookings": gin.H{
			"today":    bookingsToday,
			"week":     bookingsWeek,
			"upcoming": upcoming,
		},
		"activity": acts,
	}})
}

func kpiCount(prev *Period, rows []dayRow) *float64 {
	if prev == nil {
		return nil
	}
	n := 0.0
	for _, r := range rows {
		if prev.Contains(r.D) {
			n += float64(r.N)
		}
	}
	return &n
}

func (h *Handler) recentActivity(org string) []activity {
	out := []activity{}
	var inv []struct {
		Type    string    `gorm:"column:type"`
		Message string    `gorm:"column:message"`
		At      time.Time `gorm:"column:at"`
		ID      string    `gorm:"column:id"`
		Number  string    `gorm:"column:number"`
		Client  string    `gorm:"column:client"`
		DocType string    `gorm:"column:doc_type"`
		Total   float64   `gorm:"column:total"`
		Cur     string    `gorm:"column:currency"`
	}
	// Latest event per invoice, so one invoice's create/send/pay burst shows once.
	h.scan(&inv, `SELECT * FROM (
		  SELECT DISTINCT ON (a.invoice_id) a.type, a.message, a.created_at AS at, i.id, i.number, i.client_name AS client, i.doc_type, i.total, UPPER(COALESCE(i.currency,'')) AS currency
		  FROM billing_activities a JOIN billing_invoices i ON i.id = a.invoice_id AND i.deleted_at IS NULL
		  WHERE i.organization_id = ? AND a.created_at > now() - interval '180 days'
		  ORDER BY a.invoice_id, a.created_at DESC
		) x ORDER BY at DESC LIMIT 10`, org)
	for _, a := range inv {
		kind := "invoice"
		if strings.Contains(strings.ToLower(a.Type), "pay") {
			kind = "payment"
		}
		link := "/invoices/" + a.ID
		if a.DocType == "quote" {
			link = "/quotes/" + a.ID
		}
		title := strings.TrimSpace(a.Number + " · " + a.Client)
		msg := a.Message
		if msg == "" {
			msg = strings.ReplaceAll(a.Type, "_", " ")
		}
		var amt *float64
		if kind == "invoice" {
			amt = amountPtr(a.Total)
		}
		out = append(out, activity{Kind: kind, Type: a.Type, Title: title, Detail: msg, At: a.At, Link: link, Amount: amt, Currency: a.Cur})
	}
	var bk []struct {
		ID       string    `gorm:"column:id"`
		At       time.Time `gorm:"column:at"`
		Start    time.Time `gorm:"column:start_time"`
		Status   string    `gorm:"column:status"`
		Customer string    `gorm:"column:customer"`
		Total    float64   `gorm:"column:total_price"`
	}
	h.scan(&bk, `SELECT b.id, b.created_at AS at, b.start_time, b.status, b.total_price,
		  TRIM(COALESCE(c.first_name,'') || ' ' || COALESCE(c.last_name,'')) AS customer
		FROM bookings b LEFT JOIN customers c ON c.id = b.customer_id
		WHERE b.organization_id = ? AND b.deleted_at IS NULL ORDER BY b.created_at DESC LIMIT 6`, org)
	for _, b := range bk {
		name := b.Customer
		if name == "" {
			name = "Customer"
		}
		start := b.Start
		out = append(out, activity{Kind: "booking", Type: b.Status, Title: "Booking · " + name,
			Detail: "Booking " + strings.ReplaceAll(b.Status, "_", " "), At: b.At, Ref: &start, Link: "/bookings",
			Amount: amountPtr(b.Total)})
	}
	var cs []struct {
		ID    string    `gorm:"column:id"`
		At    time.Time `gorm:"column:at"`
		Name  string    `gorm:"column:name"`
		Email string    `gorm:"column:email"`
	}
	h.scan(&cs, `SELECT id, created_at AS at, TRIM(COALESCE(first_name,'') || ' ' || COALESCE(last_name,'')) AS name, COALESCE(email,'') AS email
		FROM customers WHERE organization_id = ? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 6`, org)
	for _, x := range cs {
		out = append(out, activity{Kind: "customer", Type: "created", Title: "New customer · " + x.Name, Detail: x.Email, At: x.At, Link: "/customers"})
	}
	var ps []struct {
		ID     string    `gorm:"column:id"`
		At     time.Time `gorm:"column:at"`
		Number string    `gorm:"column:number"`
		Total  float64   `gorm:"column:total"`
		Type   string    `gorm:"column:type"`
	}
	h.scan(&ps, `SELECT id, created_at AS at, COALESCE(transaction_number,'') AS number, total_amount AS total, type
		FROM pos_transactions WHERE organization_id = ? AND deleted_at IS NULL AND status = 'completed'
		ORDER BY created_at DESC LIMIT 6`, org)
	for _, p := range ps {
		t := p.Total
		out = append(out, activity{Kind: "pos", Type: p.Type, Title: "POS " + p.Type + " · " + p.Number, Detail: "Point of sale", At: p.At, Link: "/pos", Amount: &t})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func amountPtr(v float64) *float64 {
	if v == 0 {
		return nil
	}
	v = round2(v)
	return &v
}
