package platform

// Duplicate organisations: detection and merge.
//
// Some deployments ended up with several organisations for one business (for
// example several "DASYIN" rows created by start-up code while the database
// sat behind a transaction pooler), with users and data spread across them.
// A super admin can list likely duplicates and merge them into the one to keep.
//
// A merge runs in ONE transaction pinned to the ERP schema:
//
//  1. Every table in the schema with an organization_id / org_id column is
//     discovered from information_schema (nothing is hard-coded), and each
//     duplicate's rows are re-pointed at the kept organisation.
//  2. Per-organisation singletons (settings rows, subscriptions, logos …: a
//     unique index on the organisation column alone, plus a few known tables)
//     keep the target's row; the duplicate's row is dropped — or moved when the
//     target has none.
//  3. Per-organisation numbers (invoice / quote / bill / PO / order numbers,
//     SKUs, account codes, and any other unique index that includes the
//     organisation column) that collide with the target's are renumbered with
//     a "-M1", "-M2" … suffix and reported. Collisions on a unique index
//     without a renameable column drop the duplicate's row (reported).
//  4. Empty details on the kept organisation (email, phone, address, country,
//     currency …) are filled from the duplicate.
//  5. The duplicate is archived (soft-deleted, status "merged"), its users'
//     refresh tokens are revoked (they sign in again into the kept
//     organisation) and an audit log row is written for both organisations.
//
// A dry run performs exactly the same statements and rolls back, so the
// preview is what the real merge will do.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------- schema

// orgTable is a table in the ERP schema scoped by an organisation column.
type orgTable struct {
	Name       string
	Col        string
	HasID      bool
	HasDeleted bool
}

type uniqueIndex struct {
	Name string
	Cols []string
}

func qi(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// erpSchema is the Postgres schema that holds ERP tables (DB_SCHEMA, default
// "erp"; "" when the deployment uses the default search_path).
func erpSchema() string {
	s := strings.TrimSpace(os.Getenv("DB_SCHEMA"))
	if s == "" {
		s = "erp"
	}
	if strings.EqualFold(s, "public") || s == "-" {
		return ""
	}
	return s
}

// pinSchema pins tx to the ERP schema (when it exists) so every statement of
// the merge hits the ERP tables, even behind a transaction pooler. It returns
// the schema the tables live in.
func pinSchema(tx *gorm.DB) (string, error) {
	if s := erpSchema(); s != "" {
		var exists bool
		tx.Raw("SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = ?)", s).Row().Scan(&exists)
		if exists {
			if err := tx.Exec("SET LOCAL search_path TO " + qi(s) + ", public").Error; err != nil {
				return "", err
			}
		}
	}
	var cur string
	if err := tx.Raw("SELECT current_schema()").Row().Scan(&cur); err != nil || cur == "" {
		return "", fmt.Errorf("could not determine the database schema: %v", err)
	}
	return cur, nil
}

// discoverOrgTables lists every base table in schema with a column named
// organization_id or org_id.
func discoverOrgTables(tx *gorm.DB, schema string) ([]orgTable, error) {
	var rows []struct {
		TableName  string
		ColumnName string
		HasID      bool
		HasDeleted bool
	}
	err := tx.Raw(`SELECT c.table_name, c.column_name,
			EXISTS (SELECT 1 FROM information_schema.columns x WHERE x.table_schema = c.table_schema AND x.table_name = c.table_name AND x.column_name = 'id') AS has_id,
			EXISTS (SELECT 1 FROM information_schema.columns x WHERE x.table_schema = c.table_schema AND x.table_name = c.table_name AND x.column_name = 'deleted_at') AS has_deleted
		FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
		WHERE c.table_schema = ? AND c.column_name IN ('organization_id', 'org_id')
		ORDER BY c.table_name, c.column_name DESC`, schema).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := []orgTable{}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.TableName == "organizations" || seen[r.TableName] {
			continue // a table with both columns uses organization_id (sorted first)
		}
		seen[r.TableName] = true
		out = append(out, orgTable{Name: r.TableName, Col: r.ColumnName, HasID: r.HasID, HasDeleted: r.HasDeleted})
	}
	return out, nil
}

// discoverUniques returns, per table, the unique indexes (primary keys
// included) that contain the table's organisation column.
func discoverUniques(tx *gorm.DB, schema string, tables []orgTable) (map[string][]uniqueIndex, error) {
	col := map[string]string{}
	for _, t := range tables {
		col[t.Name] = t.Col
	}
	var rows []struct {
		TableName string
		IndexName string
		Cols      string
	}
	err := tx.Raw(`SELECT t.relname AS table_name, i.relname AS index_name,
			string_agg(a.attname, ',' ORDER BY array_position(ix.indkey::int[], a.attnum::int)) AS cols
		FROM pg_index ix
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_class t ON t.oid = ix.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY(ix.indkey)
		WHERE n.nspname = ? AND ix.indisunique
		GROUP BY t.relname, i.relname`, schema).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := map[string][]uniqueIndex{}
	for _, r := range rows {
		c, ok := col[r.TableName]
		if !ok {
			continue
		}
		cols := strings.Split(r.Cols, ",")
		has := false
		for _, x := range cols {
			if x == c {
				has = true
			}
		}
		if has {
			out[r.TableName] = append(out[r.TableName], uniqueIndex{Name: r.IndexName, Cols: cols})
		}
	}
	return out, nil
}

// knownSingletons are one-row-per-organisation tables that have no unique
// index on the organisation column (those with one are detected).
var knownSingletons = map[string]bool{
	"billing_settings": true, "organization_settings": true, "organization_subscriptions": true, "organization_modules": true,
	"organization_brandings": true, "plan_subscriptions": true, "org_logos": true, "smallbiz_settings": true,
	"integration_settings": true, "message_settings": true, "video_settings": true, "pm_jira_connections": true,
}

// numberColumns are document numbers that must be unique within an
// organisation even where the database doesn't enforce it. scope columns
// must match too (an invoice and a quote may share a number).
var numberColumns = map[string][]struct {
	Col   string
	Scope []string
}{
	"billing_invoices":  {{"number", []string{"doc_type"}}},
	"bills":             {{"bill_number", nil}},
	"invoices":          {{"invoice_number", nil}},
	"journal_entries":   {{"entry_number", nil}},
	"purchase_orders":   {{"order_number", nil}},
	"hr_pay_runs":       {{"number", nil}},
	"mrp_work_orders":   {{"number", nil}},
	"layby_payments":    {{"layby_number", nil}},
	"ecom_orders":       {{"number", nil}},
	"products":          {{"sku", nil}},
	"chart_of_accounts": {{"code", nil}},
}

// renameable reports whether a colliding column can take a suffix (a code or
// a label) rather than being an identifier or a date.
var renameableRe = regexp.MustCompile(`(^|_)(number|no|code|sku|slug|name|title|key|reference|label|barcode)$`)

// tablesSkipped are handled separately by the merge.
var tablesSkipped = map[string]bool{"platform_org_status": true}

// ---------------------------------------------------------------- report

// Renumbered is one record whose number/code was changed to avoid a clash.
type Renumbered struct {
	Table  string `json:"table"`
	Label  string `json:"label"`
	Column string `json:"column"`
	ID     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// TableResult is what happened to one table.
type TableResult struct {
	Table      string `json:"table"`
	Label      string `json:"label"`
	Moved      int64  `json:"moved"`
	Dropped    int64  `json:"dropped"`
	Renumbered int    `json:"renumbered"`
	Note       string `json:"note,omitempty"`
}

// MergedOrg is one organisation merged into the kept one.
type MergedOrg struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Tables []TableResult `json:"tables"`
	Moved  int64         `json:"moved"`
	Users  int64         `json:"users"`
}

// MergeReport describes a merge (or its dry run).
type MergeReport struct {
	DryRun     bool         `json:"dry_run"`
	Keep       gin.H        `json:"keep"`
	Merged     []MergedOrg  `json:"merged"`
	TotalMoved int64        `json:"total_moved"`
	UsersMoved int64        `json:"users_moved"`
	Renumbered []Renumbered `json:"renumbered"`
	Dropped    []gin.H      `json:"dropped"`
	Filled     []string     `json:"filled_fields"`
	Warnings   []string     `json:"warnings"`
	Tables     int          `json:"tables_scanned"`
}

// tableLabels names the tables people recognise.
var tableLabels = map[string]string{
	"users": "Users", "customers": "Customers", "billing_invoices": "Invoices & quotes", "billing_payments": "Invoice payments",
	"billing_activities": "Invoice activity", "bookings": "Bookings", "services": "Services", "products": "Products",
	"product_categories": "Product categories", "pos_transactions": "POS sales", "suppliers": "Suppliers", "purchase_orders": "Purchase orders",
	"bills": "Bills", "vendors": "Vendors", "events": "Events", "event_registrations": "Event registrations", "hr_employees": "Employees",
	"hr_pay_runs": "Pay runs", "chart_of_accounts": "Chart of accounts", "journal_entries": "Journal entries", "bank_accounts": "Bank accounts",
	"bank_transactions": "Bank transactions", "smallbiz_entries": "Business hub entries", "leads": "Leads", "opportunities": "Opportunities",
	"audit_logs": "Audit log", "pm_projects": "Projects", "pm_tasks": "Tasks", "meetings": "Meetings", "vehicles": "Vehicles",
	"participants": "Participants", "invoices": "Finance invoices", "payments": "Finance payments", "ecom_orders": "Online orders",
	"inventory_items": "Inventory items", "inventory_movements": "Stock movements", "mrp_boms": "Bills of materials",
	"mrp_work_orders": "Work orders", "billing_settings": "Invoicing settings", "organization_settings": "Organisation settings",
	"plan_subscriptions": "Plan subscription", "org_logos": "Company logo", "smallbiz_settings": "Business hub settings",
	"billing_tax_rates": "Tax rates", "customer_contacts": "Customer contacts", "document_blobs": "Documents",
	"organization_brandings": "Organisation branding", "organization_modules": "Module settings",
	"organization_subscriptions": "Legacy subscription", "organization_invitations": "Invitations", "video_settings": "Video settings",
	"integration_settings": "Integration settings", "message_settings": "Messaging settings", "roles": "Roles",
}

func tableLabel(t string) string {
	if l, ok := tableLabels[t]; ok {
		return l
	}
	s := strings.ReplaceAll(t, "_", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------------------------------------------------------------- detection

var nameSuffixes = []string{"ptyltd", "pty", "ltd", "limited", "incorporated", "inc", "llc", "plc", "gmbh", "company", "co", "corp", "corporation"}

// NormalizeOrgName folds an organisation name for duplicate detection:
// case, accents-insensitive-ish, punctuation and spacing removed, "&" = "and",
// and a trailing legal suffix (Pty Ltd, Inc, LLC …) dropped.
func NormalizeOrgName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "&", "and")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	n := b.String()
	for changed := true; changed; {
		changed = false
		for _, suf := range nameSuffixes {
			if len(n) > len(suf)+1 && strings.HasSuffix(n, suf) {
				n = strings.TrimSuffix(n, suf)
				changed = true
			}
		}
	}
	return n
}

type dupOrg struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Email     string           `json:"email"`
	ABN       string           `json:"abn"`
	Country   string           `json:"country_code"`
	Currency  string           `json:"currency"`
	CreatedAt time.Time        `json:"created_at"`
	Status    string           `json:"status"`
	Users     int64            `json:"users"`
	Admins    []string         `json:"admin_emails"`
	Counts    map[string]int64 `json:"counts"`
	Total     int64            `json:"total_records"`
	IsOwn     bool             `json:"is_own"`
}

type dupGroup struct {
	Key       string   `json:"key"`
	Reasons   []string `json:"reasons"`
	Orgs      []dupOrg `json:"organizations"`
	SuggestID string   `json:"suggested_keep_id"`
}

// orgCounts counts live rows per org-scoped table for the given orgs.
func orgCounts(db *gorm.DB, tables []orgTable, ids []string) map[string]map[string]int64 {
	out := map[string]map[string]int64{}
	for _, id := range ids {
		out[id] = map[string]int64{}
	}
	for _, t := range tables {
		if tablesSkipped[t.Name] {
			continue
		}
		q := "SELECT " + qi(t.Col) + "::text AS org, COUNT(*) AS n FROM " + qi(t.Name) + " WHERE " + qi(t.Col) + "::text IN ?"
		if t.HasDeleted {
			q += " AND deleted_at IS NULL"
		}
		q += " GROUP BY 1"
		var rows []struct {
			Org string
			N   int64
		}
		if err := db.Raw(q, ids).Scan(&rows).Error; err != nil {
			continue
		}
		for _, r := range rows {
			if m, ok := out[r.Org]; ok && r.N > 0 {
				m[t.Name] = r.N
			}
		}
	}
	return out
}

// Duplicates lists groups of organisations that are probably the same
// business: same normalised name, same ABN or same contact email.
func (h *Handler) Duplicates(c *gin.Context) {
	var groups []dupGroup
	var scanned int
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		schema, err := pinSchema(tx)
		if err != nil {
			return err
		}
		var orgs []struct {
			ID, Name, Email, ABN, CountryCode, Currency, Status string
			CreatedAt                                           time.Time
		}
		cols := "o.id, o.name, COALESCE(o.email,'') AS email, COALESCE(o.abn,'') AS abn, o.created_at, COALESCE(s.status,'active') AS status"
		if hasColumn(tx, "organizations", "country_code") {
			cols += ", COALESCE(o.country_code,'') AS country_code, COALESCE(o.currency,'') AS currency"
		}
		q := tx.Table("organizations o").Where("o.deleted_at IS NULL")
		if hasTable(tx, "platform_org_status") {
			q = q.Joins("LEFT JOIN platform_org_status s ON s.organization_id = o.id")
		} else {
			cols = strings.Replace(cols, "COALESCE(s.status,'active')", "'active'", 1)
		}
		if err := q.Select(cols).Order("o.created_at ASC").Scan(&orgs).Error; err != nil {
			return err
		}
		// Union-find over the matching keys.
		parent := make([]int, len(orgs))
		for i := range parent {
			parent[i] = i
		}
		var find func(int) int
		find = func(i int) int {
			if parent[i] != i {
				parent[i] = find(parent[i])
			}
			return parent[i]
		}
		reasons := map[int]map[string]bool{}
		first := map[string]int{}
		link := func(key, reason string, i int) {
			if j, ok := first[key]; ok {
				a, b := find(i), find(j)
				if a != b {
					parent[a] = b
				}
				root := find(i)
				if reasons[root] == nil {
					reasons[root] = map[string]bool{}
				}
				reasons[root][reason] = true
				return
			}
			first[key] = i
		}
		for i, o := range orgs {
			if n := NormalizeOrgName(o.Name); n != "" {
				link("n:"+n, "Same name", i)
			}
			if a := strings.ReplaceAll(strings.TrimSpace(o.ABN), " ", ""); a != "" {
				link("a:"+a, "Same ABN", i)
			}
			if e := strings.ToLower(strings.TrimSpace(o.Email)); e != "" {
				link("e:"+e, "Same email", i)
			}
		}
		members := map[int][]int{}
		for i := range orgs {
			members[find(i)] = append(members[find(i)], i)
		}
		// Reasons were recorded on intermediate roots; gather them per final root.
		finalReasons := map[int]map[string]bool{}
		for r, set := range reasons {
			fr := find(r)
			if finalReasons[fr] == nil {
				finalReasons[fr] = map[string]bool{}
			}
			for k := range set {
				finalReasons[fr][k] = true
			}
		}
		tables, err := discoverOrgTables(tx, schema)
		if err != nil {
			return err
		}
		scanned = len(tables)
		var ids []string
		for _, m := range members {
			if len(m) > 1 {
				for _, i := range m {
					ids = append(ids, orgs[i].ID)
				}
			}
		}
		counts := map[string]map[string]int64{}
		admins := map[string][]string{}
		if len(ids) > 0 {
			counts = orgCounts(tx, tables, ids)
			var rows []struct{ OrganizationID, Email string }
			tx.Raw("SELECT organization_id, email FROM users WHERE organization_id IN ? AND deleted_at IS NULL AND role IN ('admin','super_admin','owner') ORDER BY created_at", ids).Scan(&rows)
			for _, r := range rows {
				admins[r.OrganizationID] = append(admins[r.OrganizationID], r.Email)
			}
		}
		own := actorOrg(c)
		for root, m := range members {
			if len(m) < 2 {
				continue
			}
			g := dupGroup{Key: orgs[root].ID}
			for k := range finalReasons[root] {
				g.Reasons = append(g.Reasons, k)
			}
			sort.Strings(g.Reasons)
			for _, i := range m {
				o := orgs[i]
				d := dupOrg{ID: o.ID, Name: o.Name, Email: o.Email, ABN: o.ABN, Country: o.CountryCode, Currency: o.Currency,
					CreatedAt: o.CreatedAt, Status: o.Status, Counts: counts[o.ID], Admins: admins[o.ID], IsOwn: o.ID == own}
				if d.Counts == nil {
					d.Counts = map[string]int64{}
				}
				if d.Admins == nil {
					d.Admins = []string{}
				}
				d.Users = d.Counts["users"]
				for _, n := range d.Counts {
					d.Total += n
				}
				g.Orgs = append(g.Orgs, d)
			}
			sort.SliceStable(g.Orgs, func(a, b int) bool { return g.Orgs[a].CreatedAt.Before(g.Orgs[b].CreatedAt) })
			g.SuggestID = suggestKeep(g.Orgs)
			groups = append(groups, g)
		}
		return nil
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not look for duplicates: "+err.Error())
		return
	}
	sort.Slice(groups, func(a, b int) bool { return groups[a].Orgs[0].Name < groups[b].Orgs[0].Name })
	if groups == nil {
		groups = []dupGroup{}
	}
	labels := map[string]string{}
	for _, g := range groups {
		for _, o := range g.Orgs {
			for t := range o.Counts {
				labels[t] = tableLabel(t)
			}
		}
	}
	ok(c, http.StatusOK, gin.H{"groups": groups, "tables_scanned": scanned, "labels": labels})
}

// suggestKeep picks the organisation to keep: the super admin's own, else
// the one with the most users, then the most data, then the oldest.
func suggestKeep(orgs []dupOrg) string {
	best := -1
	for i, o := range orgs {
		if o.IsOwn {
			return o.ID
		}
		if best < 0 {
			best = i
			continue
		}
		b := orgs[best]
		if o.Users > b.Users || (o.Users == b.Users && (o.Total > b.Total || (o.Total == b.Total && o.CreatedAt.Before(b.CreatedAt)))) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return orgs[best].ID
}

// ---------------------------------------------------------------- merge

type mergeRequest struct {
	KeepID      string   `json:"keep_id"`
	MergeIDs    []string `json:"merge_ids"`
	DryRun      bool     `json:"dry_run"`
	ConfirmName string   `json:"confirm_name"`
}

var errDryRun = errors.New("dry run")

// Merge merges organisations into one (see the package comment above).
// Body: keep_id, merge_ids, dry_run, confirm_name (the kept organisation's
// exact name; required unless dry_run).
func (h *Handler) Merge(c *gin.Context) {
	var req mergeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	req.KeepID = strings.TrimSpace(req.KeepID)
	ids := []string{}
	seen := map[string]bool{req.KeepID: true}
	for _, id := range req.MergeIDs {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if req.KeepID == "" || len(ids) == 0 {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose the organisation to keep and at least one to merge into it")
		return
	}
	if len(ids) > 20 {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Merge at most 20 organisations at a time")
		return
	}
	for _, id := range ids {
		if id == actorOrg(c) {
			fail(c, http.StatusBadRequest, "OWN_ORGANIZATION", "You can't merge away the organisation you are signed in to — keep it instead")
			return
		}
	}
	var report *MergeReport
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		r, err := h.merge(tx, c, req.KeepID, ids, req.DryRun, req.ConfirmName)
		if err != nil {
			return err
		}
		report = r
		if req.DryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		var ae *mergeError
		if errors.As(err, &ae) {
			fail(c, ae.status, ae.code, ae.msg)
			return
		}
		fail(c, http.StatusInternalServerError, "MERGE_FAILED", "The merge was rolled back: "+err.Error())
		return
	}
	ok(c, http.StatusOK, report)
}

type mergeError struct {
	status    int
	code, msg string
}

func (e *mergeError) Error() string { return e.msg }

type orgBrief struct {
	ID, Name                                                                    string
	Email, Phone, Website, Description                                          string
	AddressStreet, AddressSuburb, AddressState, AddressPostcode, AddressCountry string
	ABN                                                                         *string
	CountryCode, Currency, Timezone, RegionSource                               string
}

func loadBrief(tx *gorm.DB, id string, withRegion bool) (*orgBrief, error) {
	cols := `id, name, COALESCE(email,'') AS email, COALESCE(phone,'') AS phone, COALESCE(website,'') AS website,
		COALESCE(description,'') AS description, COALESCE(address_street,'') AS address_street, COALESCE(address_suburb,'') AS address_suburb,
		COALESCE(address_state,'') AS address_state, COALESCE(address_postcode,'') AS address_postcode,
		COALESCE(address_country,'') AS address_country, abn`
	if withRegion {
		cols += `, COALESCE(country_code,'') AS country_code, COALESCE(currency,'') AS currency, COALESCE(timezone,'') AS timezone, COALESCE(region_source,'') AS region_source`
	}
	var o orgBrief
	res := tx.Raw("SELECT "+cols+" FROM organizations WHERE id = ? AND deleted_at IS NULL FOR UPDATE", id).Scan(&o)
	if res.Error != nil {
		return nil, res.Error
	}
	if o.ID == "" {
		return nil, gorm.ErrRecordNotFound
	}
	return &o, nil
}

func (h *Handler) merge(tx *gorm.DB, c *gin.Context, keepID string, dupIDs []string, dryRun bool, confirm string) (*MergeReport, error) {
	schema, err := pinSchema(tx)
	if err != nil {
		return nil, err
	}
	withRegion := hasColumn(tx, "organizations", "country_code")
	keep, err := loadBrief(tx, keepID, withRegion)
	if err != nil {
		return nil, &mergeError{http.StatusNotFound, "NOT_FOUND", "The organisation to keep was not found"}
	}
	if !dryRun && strings.TrimSpace(confirm) != keep.Name {
		return nil, &mergeError{http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Type the kept organisation's exact name to confirm the merge"}
	}
	dups := []*orgBrief{}
	for _, id := range dupIDs {
		d, err := loadBrief(tx, id, withRegion)
		if err != nil {
			return nil, &mergeError{http.StatusNotFound, "NOT_FOUND", "Organisation " + id + " was not found (already merged or deleted?)"}
		}
		dups = append(dups, d)
	}
	tables, err := discoverOrgTables(tx, schema)
	if err != nil {
		return nil, err
	}
	uniques, err := discoverUniques(tx, schema, tables)
	if err != nil {
		return nil, err
	}
	rep := &MergeReport{DryRun: dryRun, Keep: gin.H{"id": keep.ID, "name": keep.Name}, Tables: len(tables),
		Renumbered: []Renumbered{}, Dropped: []gin.H{}, Filled: []string{}, Warnings: []string{}, Merged: []MergedOrg{}}
	for _, d := range dups {
		m, err := mergeOne(tx, schema, tables, uniques, keep, d, rep)
		if err != nil {
			return nil, err
		}
		rep.Merged = append(rep.Merged, *m)
		rep.TotalMoved += m.Moved
		rep.UsersMoved += m.Users
		if err := fillDetails(tx, keep, d, withRegion, rep); err != nil {
			return nil, err
		}
		if err := archive(tx, keep, d); err != nil {
			return nil, err
		}
	}
	if rep.UsersMoved > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d user(s) moved to %s will be asked to sign in again.", rep.UsersMoved, keep.Name))
	}
	names := []string{}
	for _, d := range dups {
		names = append(names, d.Name+" ("+d.ID+")")
	}
	if !dryRun {
		summary := gin.H{"merged": names, "rows_moved": rep.TotalMoved, "users_moved": rep.UsersMoved, "renumbered": len(rep.Renumbered), "dropped": rep.Dropped}
		Audit(tx, c, keep.ID, "organization.merge", "organization", keep.ID, gin.H{"organizations": names}, summary)
		for _, d := range dups {
			Audit(tx, c, d.ID, "organization.merged_into", "organization", d.ID, gin.H{"name": d.Name}, gin.H{"kept": keep.Name, "kept_id": keep.ID})
		}
	}
	return rep, nil
}

// mergeOne moves one duplicate's data into keep.
func mergeOne(tx *gorm.DB, schema string, tables []orgTable, uniques map[string][]uniqueIndex, keep, dup *orgBrief, rep *MergeReport) (*MergedOrg, error) {
	m := &MergedOrg{ID: dup.ID, Name: dup.Name, Tables: []TableResult{}}
	var movedUsers []string
	tx.Raw("SELECT id FROM users WHERE organization_id = ?", dup.ID).Scan(&movedUsers)
	for _, t := range tables {
		if tablesSkipped[t.Name] {
			continue
		}
		T, C := qi(schema)+"."+qi(t.Name), qi(t.Col)
		var n int64
		if err := tx.Raw("SELECT COUNT(*) FROM "+T+" WHERE "+C+"::text = ?", dup.ID).Row().Scan(&n); err != nil {
			return nil, fmt.Errorf("%s: %w", t.Name, err)
		}
		if n == 0 {
			continue
		}
		res := TableResult{Table: t.Name, Label: tableLabel(t.Name)}
		idx := uniques[t.Name]
		singleton := knownSingletons[t.Name]
		for _, u := range idx {
			if len(u.Cols) == 1 {
				singleton = true
			}
		}
		if singleton {
			var k int64
			tx.Raw("SELECT COUNT(*) FROM "+T+" WHERE "+C+"::text = ?", keep.ID).Row().Scan(&k)
			if k > 0 {
				if err := tx.Exec("DELETE FROM "+T+" WHERE "+C+"::text = ?", dup.ID).Error; err != nil {
					return nil, fmt.Errorf("%s: %w", t.Name, err)
				}
				res.Dropped = n
				res.Note = "Kept " + keep.Name + "'s; dropped the duplicate's"
				rep.Dropped = append(rep.Dropped, gin.H{"table": t.Name, "label": res.Label, "count": n, "from": dup.Name, "reason": "one per organisation — kept the target's"})
				m.Tables = append(m.Tables, res)
				continue
			}
			if n > 1 {
				// Keep one (the newest) so the unique rule still holds.
				if t.HasID {
					if err := tx.Exec("DELETE FROM "+T+" WHERE "+C+"::text = ? AND id NOT IN (SELECT id FROM "+T+" WHERE "+C+"::text = ? ORDER BY id DESC LIMIT 1)", dup.ID, dup.ID).Error; err != nil {
						return nil, fmt.Errorf("%s: %w", t.Name, err)
					}
					res.Dropped = n - 1
				}
			}
			res.Note = "Moved (" + keep.Name + " had none)"
		} else {
			// Document numbers and other per-organisation unique values.
			done := map[string]bool{}
			for _, nc := range numberColumns[t.Name] {
				if !hasColumn(tx, t.Name, nc.Col) {
					continue
				}
				cnt, err := renumber(tx, T, t, C, nc.Col, nc.Scope, keep, dup, rep)
				if err != nil {
					return nil, fmt.Errorf("%s.%s: %w", t.Name, nc.Col, err)
				}
				res.Renumbered += cnt
				done[nc.Col] = true
			}
			for _, u := range idx {
				others := []string{}
				for _, col := range u.Cols {
					if col != t.Col {
						others = append(others, col)
					}
				}
				rename := ""
				for _, col := range others {
					if done[col] {
						rename = "-" // already handled
						break
					}
					if rename == "" && renameableRe.MatchString(col) && isTextColumn(tx, schema, t.Name, col) {
						rename = col
					}
				}
				if rename == "-" {
					continue
				}
				if rename != "" {
					scope := []string{}
					for _, col := range others {
						if col != rename {
							scope = append(scope, col)
						}
					}
					cnt, err := renumber(tx, T, t, C, rename, scope, keep, dup, rep)
					if err != nil {
						return nil, fmt.Errorf("%s.%s: %w", t.Name, rename, err)
					}
					res.Renumbered += cnt
					done[rename] = true
					continue
				}
				// No renameable column: the duplicate's clashing rows go.
				match := []string{}
				for _, col := range others {
					match = append(match, "k."+qi(col)+" IS NOT DISTINCT FROM d."+qi(col))
				}
				del := tx.Exec("DELETE FROM "+T+" d WHERE d."+C+"::text = ? AND EXISTS (SELECT 1 FROM "+T+" k WHERE k."+C+"::text = ? AND "+strings.Join(match, " AND ")+")", dup.ID, keep.ID)
				if del.Error != nil {
					return nil, fmt.Errorf("%s: %w", t.Name, del.Error)
				}
				if del.RowsAffected > 0 {
					res.Dropped += del.RowsAffected
					rep.Dropped = append(rep.Dropped, gin.H{"table": t.Name, "label": res.Label, "count": del.RowsAffected, "from": dup.Name,
						"reason": "already present in the kept organisation (" + strings.Join(others, ", ") + ")"})
				}
			}
		}
		upd := tx.Exec("UPDATE "+T+" SET "+C+" = ? WHERE "+C+"::text = ?", keep.ID, dup.ID)
		if upd.Error != nil {
			return nil, fmt.Errorf("%s: %w", t.Name, upd.Error)
		}
		res.Moved = upd.RowsAffected
		m.Moved += res.Moved
		if t.Name == "users" {
			m.Users = res.Moved
		}
		m.Tables = append(m.Tables, res)
	}
	if len(movedUsers) > 0 && hasTable(tx, "refresh_tokens") && hasColumn(tx, "refresh_tokens", "is_revoked") {
		if err := tx.Exec("UPDATE refresh_tokens SET is_revoked = true WHERE user_id IN ?", movedUsers).Error; err != nil {
			return nil, fmt.Errorf("refresh_tokens: %w", err)
		}
	}
	sort.SliceStable(m.Tables, func(a, b int) bool { return m.Tables[a].Moved > m.Tables[b].Moved })
	return m, nil
}

func isTextColumn(tx *gorm.DB, schema, table, col string) bool {
	var typ string
	tx.Raw("SELECT data_type FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = ?", schema, table, col).Row().Scan(&typ)
	return typ == "character varying" || typ == "text" || typ == "character"
}

// renumber gives the duplicate's rows whose col (within scope) clashes with
// one of keep's a free "-M<n>" suffix. Returns how many were changed.
func renumber(tx *gorm.DB, T string, t orgTable, C, col string, scope []string, keep, dup *orgBrief, rep *MergeReport) (int, error) {
	if !t.HasID {
		return 0, nil
	}
	Q := qi(col)
	match := []string{"k." + Q + " = d." + Q}
	for _, s := range scope {
		match = append(match, "k."+qi(s)+" IS NOT DISTINCT FROM d."+qi(s))
	}
	sel := "d.id::text AS id, d." + Q + "::text AS val"
	if len(scope) > 0 {
		parts := []string{}
		for _, s := range scope {
			parts = append(parts, "COALESCE(d."+qi(s)+"::text,'')")
		}
		sel += ", concat_ws('|', " + strings.Join(parts, ", ") + ") AS scope"
	} else {
		sel += ", '' AS scope"
	}
	var rows []struct{ ID, Val, Scope string }
	err := tx.Raw("SELECT "+sel+" FROM "+T+" d WHERE d."+C+"::text = ? AND d."+Q+" IS NOT NULL AND EXISTS (SELECT 1 FROM "+T+" k WHERE k."+C+"::text = ? AND "+strings.Join(match, " AND ")+") ORDER BY d."+Q, dup.ID, keep.ID).Scan(&rows).Error
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		var cand string
		for i := 1; i < 1000; i++ {
			cand = fmt.Sprintf("%s-M%d", r.Val, i)
			where := "(" + C + "::text = ? OR " + C + "::text = ?) AND " + Q + " = ?"
			args := []interface{}{keep.ID, dup.ID, cand}
			for _, s := range scope {
				where += " AND " + qi(s) + " IS NOT DISTINCT FROM (SELECT " + qi(s) + " FROM " + T + " WHERE id::text = ?)"
				args = append(args, r.ID)
			}
			var n int64
			if err := tx.Raw("SELECT COUNT(*) FROM "+T+" WHERE "+where, args...).Row().Scan(&n); err != nil {
				return 0, err
			}
			if n == 0 {
				break
			}
		}
		if err := tx.Exec("UPDATE "+T+" SET "+Q+" = ? WHERE id::text = ?", cand, r.ID).Error; err != nil {
			return 0, err
		}
		rep.Renumbered = append(rep.Renumbered, Renumbered{Table: t.Name, Label: tableLabel(t.Name), Column: col, ID: r.ID, From: r.Val, To: cand})
	}
	return len(rows), nil
}

// fillDetails copies details the kept organisation lacks from a duplicate.
func fillDetails(tx *gorm.DB, keep, dup *orgBrief, withRegion bool, rep *MergeReport) error {
	upd := map[string]interface{}{}
	fill := func(col string, k *string, d string, label string) {
		if strings.TrimSpace(*k) == "" && strings.TrimSpace(d) != "" {
			upd[col] = d
			*k = d
			rep.Filled = append(rep.Filled, label+" (from "+dup.Name+")")
		}
	}
	fill("email", &keep.Email, dup.Email, "Email")
	fill("phone", &keep.Phone, dup.Phone, "Phone")
	fill("website", &keep.Website, dup.Website, "Website")
	fill("description", &keep.Description, dup.Description, "Description")
	if keep.AddressStreet == "" && keep.AddressSuburb == "" && (dup.AddressStreet != "" || dup.AddressSuburb != "") {
		upd["address_street"], upd["address_suburb"], upd["address_state"] = dup.AddressStreet, dup.AddressSuburb, dup.AddressState
		upd["address_postcode"], upd["address_country"] = dup.AddressPostcode, dup.AddressCountry
		keep.AddressStreet, keep.AddressSuburb = dup.AddressStreet, dup.AddressSuburb
		rep.Filled = append(rep.Filled, "Address (from "+dup.Name+")")
	}
	if withRegion && dup.RegionSource == "user" && keep.RegionSource != "user" && dup.CountryCode != "" {
		upd["country_code"], upd["currency"], upd["timezone"], upd["region_source"] = dup.CountryCode, dup.Currency, dup.Timezone, dup.RegionSource
		keep.CountryCode, keep.Currency, keep.RegionSource = dup.CountryCode, dup.Currency, dup.RegionSource
		rep.Filled = append(rep.Filled, "Country & currency (from "+dup.Name+")")
	}
	if (keep.ABN == nil || *keep.ABN == "") && dup.ABN != nil && *dup.ABN != "" {
		// ABN is unique: release it from the duplicate first.
		if err := tx.Exec("UPDATE organizations SET abn = NULL WHERE id = ?", dup.ID).Error; err != nil {
			return err
		}
		upd["abn"] = *dup.ABN
		keep.ABN = dup.ABN
		rep.Filled = append(rep.Filled, "ABN (from "+dup.Name+")")
	}
	if len(upd) == 0 {
		return nil
	}
	upd["updated_at"] = time.Now()
	return tx.Table("organizations").Where("id = ?", keep.ID).Updates(upd).Error
}

// archive soft-deletes the merged organisation and marks it "merged".
func archive(tx *gorm.DB, keep, dup *orgBrief) error {
	now := time.Now()
	note := strings.TrimSpace(dup.Description + "\n\nMerged into " + keep.Name + " (" + keep.ID + ") on " + now.UTC().Format("2006-01-02 15:04 MST") + ".")
	if err := tx.Table("organizations").Where("id = ?", dup.ID).Updates(map[string]interface{}{"deleted_at": now, "description": note, "updated_at": now}).Error; err != nil {
		return err
	}
	if hasTable(tx, "platform_org_status") {
		reason := "Merged into " + keep.Name + " (" + keep.ID + ")"
		if err := tx.Exec(`INSERT INTO platform_org_status (organization_id, status, reason, suspended_users, updated_by, updated_at)
			VALUES (?, 'merged', ?, '', '', ?)
			ON CONFLICT (organization_id) DO UPDATE SET status = 'merged', reason = EXCLUDED.reason, suspended_users = '', updated_at = EXCLUDED.updated_at`,
			dup.ID, reason, now).Error; err != nil {
			return err
		}
	}
	return nil
}

func hasColumn(db *gorm.DB, table, col string) bool { return db.Migrator().HasColumn(table, col) }
