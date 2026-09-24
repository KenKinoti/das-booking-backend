package platform

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type currencyTotal struct {
	Currency  string  `json:"currency"`
	Count     int64   `json:"count"`
	Invoiced  float64 `json:"invoiced"`
	Collected float64 `json:"collected"`
}

type monthPoint struct {
	Month         string `json:"month"`
	Organizations int64  `json:"organizations"`
	Users         int64  `json:"users"`
}

// Overview returns platform-wide statistics.
func (h *Handler) Overview(c *gin.Context) {
	db := h.DB
	now := time.Now()
	since30 := now.AddDate(0, 0, -30)
	out := gin.H{}

	var orgs, orgsNew, suspended int64
	if err := db.Table("organizations").Where("deleted_at IS NULL").Count(&orgs).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load platform statistics")
		return
	}
	db.Table("organizations").Where("deleted_at IS NULL AND created_at >= ?", since30).Count(&orgsNew)
	db.Table("platform_org_status s").Joins("JOIN organizations o ON o.id = s.organization_id AND o.deleted_at IS NULL").
		Where("s.status = ?", "suspended").Count(&suspended)

	var users, activeUsers, active30, usersNew, superAdmins int64
	db.Table("users").Where("deleted_at IS NULL").Count(&users)
	db.Table("users").Where("deleted_at IS NULL AND is_active").Count(&activeUsers)
	db.Table("users").Where("deleted_at IS NULL AND last_login_at >= ?", since30).Count(&active30)
	db.Table("users").Where("deleted_at IS NULL AND created_at >= ?", since30).Count(&usersNew)
	db.Table("users").Where("deleted_at IS NULL AND role = ?", "super_admin").Count(&superAdmins)

	var activeOrgs30 int64
	db.Table("users").Where("deleted_at IS NULL AND last_login_at >= ?", since30).
		Distinct("organization_id").Count(&activeOrgs30)

	out["organizations"] = gin.H{"total": orgs, "new_30d": orgsNew, "suspended": suspended, "active_30d": activeOrgs30}
	out["users"] = gin.H{"total": users, "active": activeUsers, "active_30d": active30, "new_30d": usersNew, "super_admins": superAdmins}

	// Invoicing across every organisation, grouped by currency.
	totals := []currencyTotal{}
	totals30 := []currencyTotal{}
	if hasTable(db, "billing_invoices") {
		base := "deleted_at IS NULL AND doc_type = 'invoice' AND status NOT IN ('draft','void')"
		db.Table("billing_invoices").Select("COALESCE(NULLIF(currency,''),'AUD') AS currency, COUNT(*) AS count, COALESCE(SUM(total),0) AS invoiced, COALESCE(SUM(amount_paid),0) AS collected").
			Where(base).Group("COALESCE(NULLIF(currency,''),'AUD')").Order("invoiced DESC").Scan(&totals)
		db.Table("billing_invoices").Select("COALESCE(NULLIF(currency,''),'AUD') AS currency, COUNT(*) AS count, COALESCE(SUM(total),0) AS invoiced, COALESCE(SUM(amount_paid),0) AS collected").
			Where(base+" AND issue_date >= ?", since30).Group("COALESCE(NULLIF(currency,''),'AUD')").Order("invoiced DESC").Scan(&totals30)
	}
	var invoiceCount, draftCount int64
	if hasTable(db, "billing_invoices") {
		db.Table("billing_invoices").Where("deleted_at IS NULL AND doc_type = 'invoice'").Count(&invoiceCount)
		db.Table("billing_invoices").Where("deleted_at IS NULL AND doc_type = 'invoice' AND status = 'draft'").Count(&draftCount)
	}
	out["invoicing"] = gin.H{"total_invoices": invoiceCount, "drafts": draftCount, "by_currency": totals, "last_30d_by_currency": totals30}

	// Sign-ups per month for the last 12 months.
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -11, 0)
	type row struct {
		M string
		N int64
	}
	var orgRows, userRows []row
	db.Raw(`SELECT to_char(date_trunc('month', created_at), 'YYYY-MM') AS m, COUNT(*) AS n FROM organizations
		WHERE deleted_at IS NULL AND created_at >= ? GROUP BY 1`, start).Scan(&orgRows)
	db.Raw(`SELECT to_char(date_trunc('month', created_at), 'YYYY-MM') AS m, COUNT(*) AS n FROM users
		WHERE deleted_at IS NULL AND created_at >= ? GROUP BY 1`, start).Scan(&userRows)
	om, um := map[string]int64{}, map[string]int64{}
	for _, r := range orgRows {
		om[r.M] = r.N
	}
	for _, r := range userRows {
		um[r.M] = r.N
	}
	series := make([]monthPoint, 0, 12)
	for i := 0; i < 12; i++ {
		k := start.AddDate(0, i, 0).Format("2006-01")
		series = append(series, monthPoint{Month: k, Organizations: om[k], Users: um[k]})
	}
	out["signups"] = series

	// Largest organisations by user count, with last activity.
	type topOrg struct {
		ID         string     `json:"id"`
		Name       string     `json:"name"`
		Users      int64      `json:"users"`
		LastActive *time.Time `json:"last_active"`
		CreatedAt  time.Time  `json:"created_at"`
		Status     string     `json:"status"`
	}
	top := []topOrg{}
	db.Raw(`SELECT o.id, o.name, o.created_at, COALESCE(s.status,'active') AS status,
			(SELECT COUNT(*) FROM users u WHERE u.organization_id = o.id AND u.deleted_at IS NULL) AS users,
			(SELECT MAX(u.last_login_at) FROM users u WHERE u.organization_id = o.id AND u.deleted_at IS NULL) AS last_active
		FROM organizations o LEFT JOIN platform_org_status s ON s.organization_id = o.id
		WHERE o.deleted_at IS NULL ORDER BY users DESC, o.created_at DESC LIMIT 6`).Scan(&top)
	out["top_organizations"] = top

	recent := []topOrg{}
	db.Raw(`SELECT o.id, o.name, o.created_at, COALESCE(s.status,'active') AS status,
			(SELECT COUNT(*) FROM users u WHERE u.organization_id = o.id AND u.deleted_at IS NULL) AS users,
			(SELECT MAX(u.last_login_at) FROM users u WHERE u.organization_id = o.id AND u.deleted_at IS NULL) AS last_active
		FROM organizations o LEFT JOIN platform_org_status s ON s.organization_id = o.id
		WHERE o.deleted_at IS NULL ORDER BY o.created_at DESC LIMIT 6`).Scan(&recent)
	out["recent_organizations"] = recent

	// Most recent logins.
	type login struct {
		ID          string     `json:"id"`
		Name        string     `json:"name"`
		Email       string     `json:"email"`
		Role        string     `json:"role"`
		OrgName     string     `json:"organization_name"`
		LastLoginAt *time.Time `json:"last_login_at"`
	}
	logins := []login{}
	db.Raw(`SELECT u.id, TRIM(u.first_name || ' ' || u.last_name) AS name, u.email, u.role, COALESCE(o.name,'') AS org_name, u.last_login_at
		FROM users u LEFT JOIN organizations o ON o.id = u.organization_id
		WHERE u.deleted_at IS NULL AND u.last_login_at IS NOT NULL ORDER BY u.last_login_at DESC LIMIT 8`).Scan(&logins)
	out["recent_logins"] = logins

	out["generated_at"] = now
	ok(c, http.StatusOK, out)
}
