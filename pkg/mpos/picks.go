package mpos

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Pick is one product on the till's quick grid.
type Pick struct {
	ProductID string    `json:"product_id"`
	Qty       float64   `json:"qty"`   // units sold in the window
	Sales     int64     `json:"sales"` // sales it was on
	LastSold  time.Time `json:"last_sold"`
	Source    string    `json:"source"` // mine | bar | top
}

// PickWindowDays is how far back the quick grid learns from.
const PickWindowDays = 30

type pickRow struct {
	ProductID string
	Qty       float64
	Sales     int64
	LastSold  time.Time
}

// pickQuery: products sold in completed sales since `since`, most sales first.
func pickQuery(db *gorm.DB, org, user, loc string, since time.Time, limit int) ([]pickRow, error) {
	q := `SELECT pi.product_id AS product_id, SUM(pi.quantity) AS qty, COUNT(DISTINCT pt.id) AS sales, MAX(pt.created_at) AS last_sold
		FROM pos_items pi
		JOIN pos_transactions pt ON pt.id = pi.transaction_id
		JOIN products p ON p.id = pi.product_id AND p.organization_id = pt.organization_id
		WHERE pt.organization_id = ? AND pt.status = 'completed' AND pt.deleted_at IS NULL AND pt.created_at >= ?
		  AND pi.product_id IS NOT NULL AND p.deleted_at IS NULL AND p.is_active`
	args := []interface{}{org, since}
	if user != "" {
		q += ` AND pt.cashier_id = ?`
		args = append(args, user)
	}
	if loc != "" {
		q += ` AND pt.location_id = ?`
		args = append(args, loc)
	}
	q += ` GROUP BY pi.product_id ORDER BY sales DESC, qty DESC, last_sold DESC LIMIT ?`
	args = append(args, limit)
	var rows []pickRow
	err := db.Raw(q, args...).Scan(&rows).Error
	return rows, err
}

// QuickPicks fills `limit` places: what this person sold most at this bar
// in the last 30 days, then the bar's top sellers, then the organisation's.
func QuickPicks(db *gorm.DB, org, user, loc string, now time.Time, limit int) ([]Pick, error) {
	since := now.AddDate(0, 0, -PickWindowDays)
	out := []Pick{}
	seen := map[string]bool{}
	add := func(rows []pickRow, src string) {
		for _, r := range rows {
			if len(out) >= limit || seen[r.ProductID] {
				continue
			}
			seen[r.ProductID] = true
			out = append(out, Pick{ProductID: r.ProductID, Qty: r.Qty, Sales: r.Sales, LastSold: r.LastSold, Source: src})
		}
	}
	type step struct{ user, loc, src string }
	steps := []step{{user, loc, "mine"}}
	if loc != "" {
		steps = append(steps, step{"", loc, "bar"})
	}
	steps = append(steps, step{"", "", "top"})
	for _, s := range steps {
		if len(out) >= limit {
			break
		}
		if s.src == "mine" && user == "" {
			continue
		}
		rows, err := pickQuery(db, org, s.user, s.loc, since, limit)
		if err != nil {
			return nil, err
		}
		add(rows, s.src)
	}
	return out, nil
}

func (h *Handler) quickPicks(c *gin.Context) {
	w, ok := h.who(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "12"))
	if limit < 1 || limit > 40 {
		limit = 12
	}
	loc := strings.TrimSpace(c.Query("location_id"))
	picks, err := QuickPicks(h.DB, w.Org, w.User, loc, time.Now(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not load quick picks"})
		return
	}
	mine := 0
	for _, p := range picks {
		if p.Source == "mine" {
			mine++
		}
	}
	c.JSON(http.StatusOK, gin.H{"picks": picks, "mine": mine, "days": PickWindowDays})
}
