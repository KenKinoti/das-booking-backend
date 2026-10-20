package onboarding

import (
	"strings"

	"gorm.io/gorm"
)

// A detector ticks a step by itself: each query is a cheap EXISTS-style
// check scoped to the organisation (@org) and, for "my_*" keys, the person
// (@user). The step is done when any query returns true. A query on a table
// that does not exist (a module that was never migrated) counts as false.
type detector struct {
	queries []string
}

func q(sql ...string) detector { return detector{queries: sql} }

const notDeleted = " AND deleted_at IS NULL"

// detectors by Step.DoneWhen key.
var detectors = map[string]detector{
	// organisation set-up
	"setup_done":       q(`SELECT EXISTS(SELECT 1 FROM industry_org_settings WHERE organization_id = @org AND setup_status = 'done')`),
	"type_chosen":      q(`SELECT EXISTS(SELECT 1 FROM industry_org_settings WHERE organization_id = @org AND confirmed = true)`),
	"business_details": q(`SELECT EXISTS(SELECT 1 FROM organizations WHERE id = @org AND (COALESCE(phone, '') <> '' OR COALESCE(address_street, '') <> '' OR COALESCE(address_suburb, '') <> ''))`),
	"logo": q(`SELECT EXISTS(SELECT 1 FROM org_logos WHERE organization_id = @org)`,
		`SELECT EXISTS(SELECT 1 FROM organization_brandings WHERE organization_id = @org AND COALESCE(logo_url, '') <> '')`),
	"staff":    q(`SELECT COUNT(*) > 1 FROM users WHERE organization_id = @org AND role <> 'super_admin'` + notDeleted),
	"my_phone": q(`SELECT EXISTS(SELECT 1 FROM users WHERE id = @user AND COALESCE(phone, '') <> '')`),

	// customers, invoicing
	"customer":   q(`SELECT EXISTS(SELECT 1 FROM customers WHERE organization_id = @org` + notDeleted + `)`),
	"invoice":    q(`SELECT EXISTS(SELECT 1 FROM billing_invoices WHERE organization_id = @org AND doc_type = 'invoice'` + notDeleted + `)`),
	"my_invoice": q(`SELECT EXISTS(SELECT 1 FROM billing_invoices WHERE organization_id = @org AND doc_type = 'invoice' AND created_by = @user` + notDeleted + `)`),
	"quote":      q(`SELECT EXISTS(SELECT 1 FROM billing_invoices WHERE organization_id = @org AND doc_type = 'quote'` + notDeleted + `)`),
	"payment":    q(`SELECT EXISTS(SELECT 1 FROM billing_payments WHERE organization_id = @org)`),
	"bill":       q(`SELECT EXISTS(SELECT 1 FROM bills WHERE organization_id = @org)`),

	// point of sale
	"pos_sale":    q(`SELECT EXISTS(SELECT 1 FROM pos_transactions WHERE organization_id = @org AND status = 'completed'` + notDeleted + `)`),
	"my_pos_sale": q(`SELECT EXISTS(SELECT 1 FROM pos_transactions WHERE organization_id = @org AND cashier_id = @user AND status = 'completed'` + notDeleted + `)`),
	"my_drawer":   q(`SELECT EXISTS(SELECT 1 FROM cash_drawers WHERE organization_id = @org AND opened_by = @user` + notDeleted + `)`),

	// bookings (job cards, appointments, shifts, reservations, sessions)
	"booking":             q(`SELECT EXISTS(SELECT 1 FROM bookings WHERE organization_id = @org` + notDeleted + `)`),
	"my_booking_progress": q(`SELECT EXISTS(SELECT 1 FROM bookings WHERE organization_id = @org AND staff_id = @user AND status IN ('in_progress', 'completed')` + notDeleted + `)`),
	"my_booking_done":     q(`SELECT EXISTS(SELECT 1 FROM bookings WHERE organization_id = @org AND staff_id = @user AND status = 'completed'` + notDeleted + `)`),
	"service":             q(`SELECT EXISTS(SELECT 1 FROM services WHERE organization_id = @org` + notDeleted + `)`),

	// stock, suppliers, production
	"product":            q(`SELECT EXISTS(SELECT 1 FROM products WHERE organization_id = @org` + notDeleted + `)`),
	"supplier":           q(`SELECT EXISTS(SELECT 1 FROM suppliers WHERE organization_id = @org` + notDeleted + `)`),
	"purchase_order":     q(`SELECT EXISTS(SELECT 1 FROM purchase_orders WHERE organization_id = @org` + notDeleted + `)`),
	"work_order":         q(`SELECT EXISTS(SELECT 1 FROM mrp_work_orders WHERE org_id = @org)`),
	"work_order_started": q(`SELECT EXISTS(SELECT 1 FROM mrp_work_orders WHERE org_id = @org AND started_at IS NOT NULL)`),
	"work_order_done":    q(`SELECT EXISTS(SELECT 1 FROM mrp_work_orders WHERE org_id = @org AND completed_at IS NOT NULL)`),

	// events, projects
	"event":      q(`SELECT EXISTS(SELECT 1 FROM events WHERE organization_id = @org` + notDeleted + `)`),
	"my_checkin": q(`SELECT EXISTS(SELECT 1 FROM event_registrations WHERE organization_id = @org AND checked_in_by = @user` + notDeleted + `)`),
	"project":    q(`SELECT EXISTS(SELECT 1 FROM pm_projects WHERE org_id = @org)`),
	"task":       q(`SELECT EXISTS(SELECT 1 FROM pm_tasks WHERE org_id = @org)`),

	// bar stock (clubs, bars, lounges — pkg/barstock)
	"menu_prices": q(`SELECT EXISTS(SELECT 1 FROM bar_recipes WHERE organization_id = @org)
		AND NOT EXISTS(SELECT 1 FROM bar_recipes r JOIN products p ON p.id = r.pos_product_id
			WHERE r.organization_id = @org AND p.deleted_at IS NULL AND COALESCE(p.selling_price, 0) <= 0)`),
	"delivery":            q(`SELECT EXISTS(SELECT 1 FROM bar_deliveries WHERE organization_id = @org)`),
	"my_delivery":         q(`SELECT EXISTS(SELECT 1 FROM bar_deliveries WHERE organization_id = @org AND created_by = @user)`),
	"count_approved":      q(`SELECT EXISTS(SELECT 1 FROM bar_counts WHERE organization_id = @org AND status = 'approved')`),
	"my_count":            q(`SELECT EXISTS(SELECT 1 FROM bar_counts WHERE organization_id = @org AND (created_by = @user OR submitted_by = @user))`),
	"my_transfer_request": q(`SELECT EXISTS(SELECT 1 FROM bar_transfers WHERE organization_id = @org AND requested_by = @user)`),
	"my_transfer_issued":  q(`SELECT EXISTS(SELECT 1 FROM bar_transfers WHERE organization_id = @org AND issued_by = @user)`),
	"my_wastage":          q(`SELECT EXISTS(SELECT 1 FROM bar_wastage WHERE organization_id = @org AND recorded_by = @user)`),
	"wastage_decided":     q(`SELECT EXISTS(SELECT 1 FROM bar_wastage WHERE organization_id = @org AND COALESCE(decided_by, '') <> '')`),
}

// Known reports whether a done_when key has a detector.
func Known(key string) bool {
	_, ok := detectors[key]
	return ok
}

// UserScoped reports whether a detector looks at the person (not the whole
// organisation).
func UserScoped(key string) bool {
	d, ok := detectors[key]
	if !ok {
		return false
	}
	for _, s := range d.queries {
		if strings.Contains(s, "@user") {
			return true
		}
	}
	return false
}

// Detect runs the detector named key for org and user (false when unknown
// or when its tables are missing). Call it outside a transaction: on
// Postgres a failing query would abort the caller's transaction.
func Detect(db *gorm.DB, key, org, user string) bool {
	d, ok := detectors[key]
	if !ok || org == "" {
		return false
	}
	args := map[string]interface{}{"org": org, "user": user}
	for _, sql := range d.queries {
		if strings.Contains(sql, "@user") && user == "" {
			continue
		}
		var yes bool
		if err := db.Session(&gorm.Session{NewDB: true}).Raw(sql, args).Scan(&yes).Error; err == nil && yes {
			return true
		}
	}
	return false
}
