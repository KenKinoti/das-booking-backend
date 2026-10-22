package mpos

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/reqtz"
)

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// DeviceTotals is what one device sold (for a person, or overall).
type DeviceTotals struct {
	DeviceID    string   `json:"device_id"`
	DeviceLabel string   `json:"device_label"`
	SalesCount  int64    `json:"sales_count"`
	SalesTotal  float64  `json:"sales_total"`
	VoidedCount int64    `json:"voided_count"`
	VoidedTotal float64  `json:"voided_total"`
	People      []string `json:"people,omitempty"` // names (overall list only)
	LastSale    *string  `json:"last_sale,omitempty"`
}

// PersonTotals is the accountability line for one person.
type PersonTotals struct {
	UserID        string         `json:"user_id"`
	Name          string         `json:"name"`
	Email         string         `json:"email"`
	SalesCount    int64          `json:"sales_count"`
	SalesTotal    float64        `json:"sales_total"`
	AverageSale   float64        `json:"average_sale"`
	ItemsSold     int64          `json:"items_sold"`
	VoidedCount   int64          `json:"voided_count"` // their sales that were voided
	VoidedTotal   float64        `json:"voided_total"`
	VoidsDone     int64          `json:"voids_done"` // sales they voided (anyone's)
	DiscountCount int64          `json:"discount_count"`
	DiscountTotal float64        `json:"discount_total"`
	CashTotal     float64        `json:"cash_total"`   // cash kept (cash paid − change)
	MobileTotal   float64        `json:"mobile_total"` // M-Pesa / mobile money
	CardTotal     float64        `json:"card_total"`   // card + EFTPOS
	OtherTotal    float64        `json:"other_total"`
	PhotoCount    int64          `json:"photo_count"`
	FirstSale     *time.Time     `json:"first_sale,omitempty"`
	LastSale      *time.Time     `json:"last_sale,omitempty"`
	Devices       []DeviceTotals `json:"devices"`
}

// Summary is GET /pos/people-summary.
type Summary struct {
	From    string         `json:"from"`
	To      string         `json:"to"`
	People  []PersonTotals `json:"people"`
	Devices []DeviceTotals `json:"devices"`
	Totals  PersonTotals   `json:"totals"`
}

// PeopleSummary adds up sales per person and per device in [from, to).
func PeopleSummary(db *gorm.DB, org string, from, to time.Time) (Summary, error) {
	out := Summary{People: []PersonTotals{}, Devices: []DeviceTotals{}}
	base := `FROM pos_transactions pt WHERE pt.organization_id = ? AND pt.deleted_at IS NULL AND pt.created_at >= ? AND pt.created_at < ?`
	args := []interface{}{org, from, to}

	type prow struct {
		CashierID     string
		SalesCount    int64
		SalesTotal    float64
		VoidedCount   int64
		VoidedTotal   float64
		DiscountCount int64
		DiscountTotal float64
		PhotoCount    int64
		FirstSale     *time.Time
		LastSale      *time.Time
	}
	var prows []prow
	if err := db.Raw(`SELECT pt.cashier_id AS cashier_id,
			COUNT(*) FILTER (WHERE pt.status = 'completed') AS sales_count,
			COALESCE(SUM(pt.total_amount) FILTER (WHERE pt.status = 'completed'), 0) AS sales_total,
			COUNT(*) FILTER (WHERE pt.status = 'voided') AS voided_count,
			COALESCE(SUM(pt.total_amount) FILTER (WHERE pt.status = 'voided'), 0) AS voided_total,
			COUNT(*) FILTER (WHERE pt.status = 'completed' AND (pt.discount_amount > 0 OR EXISTS (SELECT 1 FROM pos_items d WHERE d.transaction_id = pt.id AND d.discount_amount > 0))) AS discount_count,
			COALESCE(SUM(pt.discount_amount + COALESCE((SELECT SUM(d.discount_amount) FROM pos_items d WHERE d.transaction_id = pt.id), 0)) FILTER (WHERE pt.status = 'completed'), 0) AS discount_total,
			COALESCE(SUM(pt.photo_count), 0) AS photo_count,
			MIN(pt.created_at) AS first_sale, MAX(pt.created_at) AS last_sale
		`+base+` GROUP BY pt.cashier_id`, args...).Scan(&prows).Error; err != nil {
		return out, err
	}

	type irow struct {
		CashierID string
		Items     int64
	}
	var irows []irow
	if err := db.Raw(`SELECT pt.cashier_id AS cashier_id, COALESCE(SUM(pi.quantity), 0) AS items
		FROM pos_items pi JOIN pos_transactions pt ON pt.id = pi.transaction_id
		WHERE pt.organization_id = ? AND pt.deleted_at IS NULL AND pt.created_at >= ? AND pt.created_at < ? AND pt.status = 'completed'
		GROUP BY pt.cashier_id`, args...).Scan(&irows).Error; err != nil {
		return out, err
	}
	items := map[string]int64{}
	for _, r := range irows {
		items[r.CashierID] = r.Items
	}

	type mrow struct {
		CashierID string
		Method    string
		Amount    float64
	}
	var mrows []mrow
	if err := db.Raw(`SELECT pt.cashier_id AS cashier_id, pp.method AS method, COALESCE(SUM(pp.amount), 0) AS amount
		FROM pos_payments pp JOIN pos_transactions pt ON pt.id = pp.transaction_id
		WHERE pt.organization_id = ? AND pt.deleted_at IS NULL AND pt.created_at >= ? AND pt.created_at < ? AND pt.status = 'completed'
		GROUP BY pt.cashier_id, pp.method`, args...).Scan(&mrows).Error; err != nil {
		return out, err
	}
	type crow struct {
		CashierID string
		Change    float64
	}
	var crows []crow
	if err := db.Raw(`SELECT pt.cashier_id AS cashier_id, COALESCE(SUM(pt.change_amount), 0) AS change `+base+` AND pt.status = 'completed' GROUP BY pt.cashier_id`, args...).Scan(&crows).Error; err != nil {
		return out, err
	}

	type vrow struct {
		VoidedBy string
		N        int64
	}
	var vrows []vrow
	if err := db.Raw(`SELECT pt.voided_by AS voided_by, COUNT(*) AS n FROM pos_transactions pt
		WHERE pt.organization_id = ? AND pt.deleted_at IS NULL AND pt.status = 'voided' AND pt.voided_by IS NOT NULL
		  AND pt.voided_at >= ? AND pt.voided_at < ? GROUP BY pt.voided_by`, args...).Scan(&vrows).Error; err != nil {
		return out, err
	}

	type drow struct {
		CashierID   string
		DeviceID    string
		DeviceLabel string
		SalesCount  int64
		SalesTotal  float64
		VoidedCount int64
		VoidedTotal float64
		LastSale    time.Time
	}
	var drows []drow
	if err := db.Raw(`SELECT pt.cashier_id AS cashier_id, COALESCE(pt.device_id, '') AS device_id,
			(ARRAY_AGG(COALESCE(pt.device_label, '') ORDER BY pt.created_at DESC))[1] AS device_label,
			COUNT(*) FILTER (WHERE pt.status = 'completed') AS sales_count,
			COALESCE(SUM(pt.total_amount) FILTER (WHERE pt.status = 'completed'), 0) AS sales_total,
			COUNT(*) FILTER (WHERE pt.status = 'voided') AS voided_count,
			COALESCE(SUM(pt.total_amount) FILTER (WHERE pt.status = 'voided'), 0) AS voided_total,
			MAX(pt.created_at) AS last_sale
		`+base+` GROUP BY pt.cashier_id, COALESCE(pt.device_id, '')`, args...).Scan(&drows).Error; err != nil {
		return out, err
	}

	// names of everyone involved
	ids := map[string]bool{}
	for _, r := range prows {
		ids[r.CashierID] = true
	}
	for _, r := range vrows {
		ids[r.VoidedBy] = true
	}
	type urow struct {
		ID, FirstName, LastName, Email string
	}
	var urows []urow
	if len(ids) > 0 {
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		db.Raw(`SELECT id, first_name, last_name, email FROM users WHERE id IN ?`, list).Scan(&urows)
	}
	users := map[string]urow{}
	for _, u := range urows {
		users[u.ID] = u
	}
	nameOf := func(id string) (string, string) {
		u, ok := users[id]
		if !ok {
			return "Unknown user", ""
		}
		n := strings.TrimSpace(u.FirstName + " " + u.LastName)
		if n == "" {
			n = u.Email
		}
		return n, u.Email
	}

	people := map[string]*PersonTotals{}
	get := func(id string) *PersonTotals {
		p := people[id]
		if p == nil {
			n, e := nameOf(id)
			p = &PersonTotals{UserID: id, Name: n, Email: e, Devices: []DeviceTotals{}}
			people[id] = p
		}
		return p
	}
	for _, r := range prows {
		p := get(r.CashierID)
		p.SalesCount, p.SalesTotal = r.SalesCount, round2(r.SalesTotal)
		p.VoidedCount, p.VoidedTotal = r.VoidedCount, round2(r.VoidedTotal)
		p.DiscountCount, p.DiscountTotal = r.DiscountCount, round2(r.DiscountTotal)
		p.PhotoCount = r.PhotoCount
		p.FirstSale, p.LastSale = r.FirstSale, r.LastSale
		p.ItemsSold = items[r.CashierID]
		if p.SalesCount > 0 {
			p.AverageSale = round2(p.SalesTotal / float64(p.SalesCount))
		}
	}
	for _, r := range mrows {
		p := get(r.CashierID)
		switch r.Method {
		case "cash":
			p.CashTotal += r.Amount
		case "mobile_money":
			p.MobileTotal += r.Amount
		case "card", "eftpos":
			p.CardTotal += r.Amount
		default:
			p.OtherTotal += r.Amount
		}
	}
	for _, r := range crows {
		get(r.CashierID).CashTotal -= r.Change
	}
	for _, r := range vrows {
		get(r.VoidedBy).VoidsDone = r.N
	}

	devs := map[string]*DeviceTotals{}
	devPeople := map[string]map[string]bool{}
	for _, r := range drows {
		p := get(r.CashierID)
		ls := r.LastSale.UTC().Format(time.RFC3339)
		p.Devices = append(p.Devices, DeviceTotals{DeviceID: r.DeviceID, DeviceLabel: r.DeviceLabel, SalesCount: r.SalesCount, SalesTotal: round2(r.SalesTotal),
			VoidedCount: r.VoidedCount, VoidedTotal: round2(r.VoidedTotal), LastSale: &ls})
		d := devs[r.DeviceID]
		if d == nil {
			d = &DeviceTotals{DeviceID: r.DeviceID}
			devs[r.DeviceID] = d
			devPeople[r.DeviceID] = map[string]bool{}
		}
		if d.LastSale == nil || ls > *d.LastSale {
			d.DeviceLabel = r.DeviceLabel
			d.LastSale = &ls
		}
		d.SalesCount += r.SalesCount
		d.SalesTotal = round2(d.SalesTotal + r.SalesTotal)
		d.VoidedCount += r.VoidedCount
		d.VoidedTotal = round2(d.VoidedTotal + r.VoidedTotal)
		n, _ := nameOf(r.CashierID)
		devPeople[r.DeviceID][n] = true
	}

	for _, p := range people {
		p.CashTotal, p.MobileTotal, p.CardTotal, p.OtherTotal = round2(p.CashTotal), round2(p.MobileTotal), round2(p.CardTotal), round2(p.OtherTotal)
		sort.Slice(p.Devices, func(i, j int) bool { return p.Devices[i].SalesTotal > p.Devices[j].SalesTotal })
		out.People = append(out.People, *p)
		t := &out.Totals
		t.SalesCount += p.SalesCount
		t.SalesTotal = round2(t.SalesTotal + p.SalesTotal)
		t.ItemsSold += p.ItemsSold
		t.VoidedCount += p.VoidedCount
		t.VoidedTotal = round2(t.VoidedTotal + p.VoidedTotal)
		t.VoidsDone += p.VoidsDone
		t.DiscountCount += p.DiscountCount
		t.DiscountTotal = round2(t.DiscountTotal + p.DiscountTotal)
		t.CashTotal = round2(t.CashTotal + p.CashTotal)
		t.MobileTotal = round2(t.MobileTotal + p.MobileTotal)
		t.CardTotal = round2(t.CardTotal + p.CardTotal)
		t.OtherTotal = round2(t.OtherTotal + p.OtherTotal)
		t.PhotoCount += p.PhotoCount
	}
	out.Totals.Name = "Everyone"
	out.Totals.Devices = []DeviceTotals{}
	if out.Totals.SalesCount > 0 {
		out.Totals.AverageSale = round2(out.Totals.SalesTotal / float64(out.Totals.SalesCount))
	}
	sort.Slice(out.People, func(i, j int) bool {
		if out.People[i].SalesTotal != out.People[j].SalesTotal {
			return out.People[i].SalesTotal > out.People[j].SalesTotal
		}
		return out.People[i].Name < out.People[j].Name
	})
	for id, d := range devs {
		for n := range devPeople[id] {
			d.People = append(d.People, n)
		}
		sort.Strings(d.People)
		out.Devices = append(out.Devices, *d)
	}
	sort.Slice(out.Devices, func(i, j int) bool {
		if out.Devices[i].SalesTotal != out.Devices[j].SalesTotal {
			return out.Devices[i].SalesTotal > out.Devices[j].SalesTotal
		}
		return out.Devices[i].DeviceLabel < out.Devices[j].DeviceLabel
	})
	return out, nil
}

// dayRange reads start_date / end_date (YYYY-MM-DD, the caller's time zone);
// both default to today.
func dayRange(c *gin.Context) (time.Time, time.Time, string, string, bool) {
	loc := reqtz.Location(c)
	today := reqtz.DateString(c)
	fs := c.DefaultQuery("start_date", today)
	ts := c.DefaultQuery("end_date", fs)
	from, err1 := time.ParseInLocation("2006-01-02", fs, loc)
	to, err2 := time.ParseInLocation("2006-01-02", ts, loc)
	if err1 != nil || err2 != nil || to.Before(from) {
		return time.Time{}, time.Time{}, "", "", false
	}
	return from, to.AddDate(0, 0, 1), fs, ts, true
}

func (h *Handler) peopleSummary(c *gin.Context) {
	w, ok := h.who(c)
	if !ok {
		return
	}
	if !h.manager(w) {
		forbidden(c, "Only owners and managers can see sales by person.")
		return
	}
	from, to, fs, ts, ok := dayRange(c)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Dates must be YYYY-MM-DD, end on or after start"})
		return
	}
	s, err := PeopleSummary(h.DB, w.Org, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not add up sales by person"})
		return
	}
	s.From, s.To = fs, ts
	c.JSON(http.StatusOK, gin.H{"summary": s})
}
