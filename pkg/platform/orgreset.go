package platform

// Reset organisation for handover (platform admins only).
//
// A super admin sets up and tests an organisation for a client, then resets
// it before handing it over: the test data is removed and the owner gets the
// setup wizard (and the getting-started steps of other packages, via
// pkg/orghooks) on the next sign-in.
//
//	POST /super-admin/platform/organizations/:id/reset   (alias /super-admin/organizations/:id/reset)
//	     {dry_run, keep_customers=false, keep_catalogue=true, keep_staff=true, confirm_name}
//	GET  /super-admin/platform/organizations/:id/export  (alias /super-admin/organizations/:id/export)
//	     ?keep_customers=…&keep_catalogue=…&keep_staff=…  → ZIP of JSON files
//
// What is cleared, updated or kept is declared in orgreset_tables.go. Both a
// dry run and a reset run the same statements in ONE transaction pinned to
// the ERP schema and guarded by an advisory lock (one reset per
// organisation at a time); the dry run rolls back, so its counts are exactly
// what the reset removes. A real reset additionally reopens the setup wizard
// (industry.ResetSetup), calls every orghooks reset hook and writes an audit
// entry — all inside the transaction.

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orghooks"
)

// OrgResetDone runs after a reset has committed (e.g. to drop cached
// organisation state such as the business-type settings).
var OrgResetDone []func(org string)

// resetLockClass is the first key of the per-organisation advisory lock.
const resetLockClass = 7340

// remainingExtra narrows "rows left" for soft-deleted tables.
var remainingExtra = map[string]string{"users": "deleted_at IS NULL"}

// none is a value no id has, so "IN @owners" / "IN @staff" never see an empty list.
const none = "~none~"

var (
	errResetDryRun = errors.New("dry run")
	errResetBusy   = errors.New("a reset of this organisation is already running")
	errNoOwner     = errors.New("no owner")
)

// ---------------------------------------------------------------- report

// ResetOptionsView is ResetOptions as JSON.
type ResetOptionsView struct {
	KeepCustomers bool `json:"keep_customers"`
	KeepCatalogue bool `json:"keep_catalogue"`
	KeepStaff     bool `json:"keep_staff"`
}

// ResetTable is one step's effect on a table.
type ResetTable struct {
	Table     string `json:"table"`
	Label     string `json:"label"`
	Action    string `json:"action"` // remove | update | keep
	Rows      int64  `json:"rows"`
	Remaining *int64 `json:"remaining,omitempty"` // the organisation's rows left in the table afterwards
	Note      string `json:"note,omitempty"`
}

// ResetGroupReport is one group of the report.
type ResetGroupReport struct {
	Key         string       `json:"key"`
	Label       string       `json:"label"`
	Description string       `json:"description"`
	Option      string       `json:"option,omitempty"` // customers | catalogue | staff for optional groups
	Action      string       `json:"action"`           // remove | keep
	Rows        int64        `json:"rows"`             // rows removed (kept, for a kept group)
	Updated     int64        `json:"updated"`          // rows updated
	Tables      []ResetTable `json:"tables"`
}

// ResetOtherTable is a table outside the groups.
type ResetOtherTable struct {
	Table  string `json:"table"`
	Column string `json:"column,omitempty"`
	Rows   int64  `json:"rows"`
	Reason string `json:"reason"`
}

// ResetHookRun is one orghooks reset hook.
type ResetHookRun struct {
	Name string `json:"name"`
	Ran  bool   `json:"ran"`
}

// ResetNumber is the next number of a series after the reset.
type ResetNumber struct {
	Label string `json:"label"`
	Next  string `json:"next"`
}

// ResetOwner is a user who keeps access.
type ResetOwner struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// ResetReport is the dry run / reset result.
type ResetReport struct {
	DryRun       bool               `json:"dry_run"`
	Organization gin.H              `json:"organization"`
	Options      ResetOptionsView   `json:"options"`
	Groups       []ResetGroupReport `json:"groups"`
	NotTouched   []ResetOtherTable  `json:"not_touched"`
	HookTables   []ResetOtherTable  `json:"hook_tables"` // cleared by a module's orghooks reset hook
	Unclassified []ResetOtherTable  `json:"unclassified"`
	Missing      []string           `json:"missing_tables"` // declared tables this database does not have
	Hooks        []ResetHookRun     `json:"hooks"`
	Numbering    []ResetNumber      `json:"numbering"`
	Setup        gin.H              `json:"setup"`
	Owners       []ResetOwner       `json:"owners"`
	StaffUsers   int64              `json:"staff_users"` // users other than the owners (active)
	Totals       gin.H              `json:"totals"`
	AuditID      string             `json:"audit_id,omitempty"`
	At           time.Time          `json:"at"`
}

// ---------------------------------------------------------------- plan

type resetEnv struct {
	schema string
	tables map[string]bool   // every base table of the schema
	orgCol map[string]string // organisation column of the scoped tables
	params map[string]interface{}
	owners []ResetOwner
	staff  int64
}

func optionOff(opt string, o orghooks.ResetOptions) bool {
	switch opt {
	case optCustomers:
		return !o.KeepCustomers
	case optCatalogue:
		return !o.KeepCatalogue
	case optStaff:
		return !o.KeepStaff
	}
	return true
}

// prepare pins the schema and collects the tables, owners and staff of org.
func prepare(tx *gorm.DB, org string) (*resetEnv, error) {
	schema, err := pinSchema(tx)
	if err != nil {
		return nil, err
	}
	env := &resetEnv{schema: schema, tables: map[string]bool{}, orgCol: map[string]string{}}
	var names []string
	if err := tx.Raw(`SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE'`, schema).Scan(&names).Error; err != nil {
		return nil, err
	}
	for _, n := range names {
		env.tables[n] = true
	}
	scoped, err := discoverOrgTables(tx, schema)
	if err != nil {
		return nil, err
	}
	for _, t := range scoped {
		env.orgCol[t.Name] = t.Col
	}
	adminOf := "u.role IN ('admin', 'owner', 'super_admin')"
	if env.tables["industry_user_roles"] && env.tables["industry_org_roles"] {
		adminOf += ` OR u.id IN (SELECT ur.user_id FROM industry_user_roles ur JOIN industry_org_roles r ON r.id = ur.role_id
			WHERE ur.organization_id = @org AND r.base = 'admin')`
	}
	var owners []struct {
		ID, Email, FirstName, LastName, Role string
	}
	if err := tx.Raw(`SELECT u.id, u.email, COALESCE(u.first_name, '') AS first_name, COALESCE(u.last_name, '') AS last_name, COALESCE(u.role, '') AS role
		FROM users u WHERE u.organization_id = @org AND u.deleted_at IS NULL AND (`+adminOf+`) ORDER BY u.created_at`,
		map[string]interface{}{"org": org}).Scan(&owners).Error; err != nil {
		return nil, err
	}
	ownerIDs := []string{none}
	for _, o := range owners {
		ownerIDs = append(ownerIDs, o.ID)
		env.owners = append(env.owners, ResetOwner{ID: o.ID, Email: o.Email, Name: strings.TrimSpace(o.FirstName + " " + o.LastName), Role: o.Role})
	}
	staffIDs := []string{none}
	var staff []struct {
		ID      string
		Deleted bool
	}
	if err := tx.Raw(`SELECT id, deleted_at IS NOT NULL AS deleted FROM users WHERE organization_id = @org AND id NOT IN @owners`,
		map[string]interface{}{"org": org, "owners": ownerIDs}).Scan(&staff).Error; err != nil {
		return nil, err
	}
	for _, s := range staff {
		staffIDs = append(staffIDs, s.ID)
		if !s.Deleted {
			env.staff++
		}
	}
	env.params = map[string]interface{}{"org": org, "owners": ownerIDs, "staff": staffIDs}
	return env, nil
}

func stepSQL(kind string, s resetStep) string {
	switch kind {
	case "count":
		return "SELECT COUNT(*) FROM " + qi(s.Table) + " WHERE " + s.Where
	case "exec":
		if s.Set != "" {
			return "UPDATE " + qi(s.Table) + " SET " + s.Set + " WHERE " + s.Where
		}
		return "DELETE FROM " + qi(s.Table) + " WHERE " + s.Where
	}
	return ""
}

// named passes the named parameters only to SQL that uses them (gorm treats
// a map given to SQL without "@" as a positional argument).
func named(q string, params map[string]interface{}) []interface{} {
	if strings.Contains(q, "@") {
		return []interface{}{params}
	}
	return nil
}

func countRows(tx *gorm.DB, q string, params map[string]interface{}) (int64, error) {
	var n int64
	err := tx.Raw(q, named(q, params)...).Scan(&n).Error
	return n, err
}

// runReset executes the plan for org inside one transaction. A dry run rolls
// back; a reset also reopens the setup wizard, runs the hooks and audits.
func (h *Handler) runReset(c *gin.Context, org *OrgRow, opts orghooks.ResetOptions, dry bool) (*ResetReport, error) {
	rep := &ResetReport{DryRun: dry, At: time.Now(),
		Organization: gin.H{"id": org.ID, "name": org.Name, "business_type": org.BusinessType},
		Options:      ResetOptionsView{KeepCustomers: opts.KeepCustomers, KeepCatalogue: opts.KeepCatalogue, KeepStaff: opts.KeepStaff},
		Groups:       []ResetGroupReport{}, NotTouched: []ResetOtherTable{}, HookTables: []ResetOtherTable{}, Unclassified: []ResetOtherTable{}, Missing: []string{},
		Hooks: []ResetHookRun{}, Numbering: []ResetNumber{}, Owners: []ResetOwner{}}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		var got bool
		if err := tx.Raw("SELECT pg_try_advisory_xact_lock("+strconv.Itoa(resetLockClass)+", hashtext(?))", org.ID).Row().Scan(&got); err != nil {
			return err
		}
		if !got {
			return errResetBusy
		}
		env, err := prepare(tx, org.ID)
		if err != nil {
			return err
		}
		rep.Owners = append(rep.Owners, env.owners...)
		rep.StaffUsers = env.staff
		if !opts.KeepStaff && len(env.owners) == 0 {
			return errNoOwner
		}
		classified, missing := map[string]bool{}, map[string]bool{}
		var removed, updated, kept int64
		for _, g := range resetGroups {
			active := g.Option == "" || optionOff(g.Option, opts)
			gr := ResetGroupReport{Key: g.Key, Label: g.Label, Description: g.Description, Option: g.Option, Action: "remove", Tables: []ResetTable{}}
			if !active {
				gr.Action = "keep"
			}
			for _, s := range g.Steps {
				classified[s.Table] = true
				if !env.tables[s.Table] {
					if !missing[s.Table] {
						missing[s.Table] = true
						rep.Missing = append(rep.Missing, s.Table)
					}
					continue
				}
				t := ResetTable{Table: s.Table, Label: s.Label, Note: s.Note, Action: "remove"}
				if s.Set != "" && !s.Soft {
					t.Action = "update"
				}
				if active {
					q := stepSQL("exec", s)
					res := tx.Exec(q, named(q, env.params)...)
					if res.Error != nil {
						return fmt.Errorf("%s: %w", s.Table, res.Error)
					}
					t.Rows = res.RowsAffected
					if s.Set != "" && !s.Soft {
						gr.Updated += t.Rows
						updated += t.Rows
					} else {
						gr.Rows += t.Rows
						removed += t.Rows
					}
				} else {
					t.Action = "keep"
					n, err := countRows(tx, stepSQL("count", s), env.params)
					if err != nil {
						return fmt.Errorf("%s: %w", s.Table, err)
					}
					t.Rows = n
					if s.Set == "" || s.Soft {
						gr.Rows += n
						kept += n
					}
				}
				gr.Tables = append(gr.Tables, t)
			}
			rep.Groups = append(rep.Groups, gr)
		}
		// What is left of the organisation in each classified table.
		left := map[string]int64{}
		for i := range rep.Groups {
			for j := range rep.Groups[i].Tables {
				t := &rep.Groups[i].Tables[j]
				col, ok := env.orgCol[t.Table]
				if !ok {
					continue
				}
				n, seen := left[t.Table]
				if !seen {
					q := "SELECT COUNT(*) FROM " + qi(t.Table) + " WHERE " + qi(col) + " = @org"
					if extra := remainingExtra[t.Table]; extra != "" {
						q += " AND " + extra
					}
					if n, err = countRows(tx, q, env.params); err != nil {
						return fmt.Errorf("%s: %w", t.Table, err)
					}
					left[t.Table] = n
				}
				v := n
				t.Remaining = &v
			}
		}
		// Everything else of the organisation.
		registered := map[string]bool{}
		for _, hk := range orghooks.ResetHooks() {
			registered[hk.Name] = true
		}
		names := make([]string, 0, len(env.orgCol))
		for n := range env.orgCol {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if classified[n] {
				continue
			}
			col := env.orgCol[n]
			cnt, err := countRows(tx, "SELECT COUNT(*) FROM "+qi(n)+" WHERE "+qi(col)+" = @org", env.params)
			if err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
			if reason, ok := resetNotTouched[n]; ok {
				rep.NotTouched = append(rep.NotTouched, ResetOtherTable{Table: n, Column: col, Rows: cnt, Reason: reason})
			} else if hook, ok := resetHookTables[n]; ok && registered[hook] {
				rep.HookTables = append(rep.HookTables, ResetOtherTable{Table: n, Column: col, Rows: cnt, Reason: "Cleared by the " + hook + " module"})
			} else {
				rep.Unclassified = append(rep.Unclassified, ResetOtherTable{Table: n, Column: col, Rows: cnt, Reason: "Not touched (unclassified)"})
			}
		}
		rep.Numbering = numbering(tx, env, org.ID)
		rep.Setup = gin.H{"status": industry.SetupPending, "note": "The owner gets the setup wizard on the next sign-in"}
		for _, hk := range orghooks.ResetHooks() {
			rep.Hooks = append(rep.Hooks, ResetHookRun{Name: hk.Name})
		}
		rep.Totals = gin.H{"removed": removed, "updated": updated, "kept": kept, "unclassified": len(rep.Unclassified)}
		if dry {
			return errResetDryRun
		}

		if env.tables["industry_org_settings"] {
			if err := industry.ResetSetup(tx, org.ID); err != nil {
				return fmt.Errorf("setup wizard: %w", err)
			}
		}
		for i, hk := range orghooks.ResetHooks() {
			if err := runHook(tx, hk, org.ID, opts); err != nil {
				return fmt.Errorf("reset hook %q: %w", hk.Name, err)
			}
			if i < len(rep.Hooks) {
				rep.Hooks[i].Ran = true
			}
		}
		groups := gin.H{}
		for _, g := range rep.Groups {
			if g.Action == "remove" && (g.Rows > 0 || g.Updated > 0) {
				groups[g.Key] = gin.H{"removed": g.Rows, "updated": g.Updated}
			}
		}
		id, err := auditTx(tx, c, org.ID, "organization.reset", gin.H{"name": org.Name},
			gin.H{"options": rep.Options, "totals": rep.Totals, "groups": groups, "hooks": rep.Hooks, "unclassified": len(rep.Unclassified)})
		if err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		rep.AuditID = id
		return nil
	})
	if errors.Is(err, errResetDryRun) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if !dry {
		for _, fn := range OrgResetDone {
			fn(org.ID)
		}
	}
	return rep, nil
}

func runHook(tx *gorm.DB, hk orghooks.ResetHook, org string, opts orghooks.ResetOptions) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return hk.Fn(tx, org, opts)
}

// numbering lists the first numbers after a reset.
func numbering(tx *gorm.DB, env *resetEnv, org string) []ResetNumber {
	out := []ResetNumber{}
	if env.tables["billing_settings"] {
		var s struct {
			InvoicePrefix, QuotePrefix, ReceiptPrefix string
			NumberPadding                             int
		}
		tx.Raw(`SELECT COALESCE(invoice_prefix, 'INV-') AS invoice_prefix, COALESCE(quote_prefix, 'QUO-') AS quote_prefix,
			COALESCE(receipt_prefix, 'RCT-') AS receipt_prefix, COALESCE(number_padding, 4) AS number_padding
			FROM billing_settings WHERE organization_id = ?`, org).Scan(&s)
		if s.InvoicePrefix == "" && s.QuotePrefix == "" {
			s.InvoicePrefix, s.QuotePrefix, s.ReceiptPrefix, s.NumberPadding = "INV-", "QUO-", "RCT-", 4
		}
		pad := s.NumberPadding
		if pad < 1 || pad > 10 {
			pad = 4
		}
		out = append(out,
			ResetNumber{"Invoices", fmt.Sprintf("%s%0*d", s.InvoicePrefix, pad, 1001)},
			ResetNumber{"Quotes", fmt.Sprintf("%s%0*d", s.QuotePrefix, pad, 1001)},
			ResetNumber{"Receipts", fmt.Sprintf("%s%0*d", s.ReceiptPrefix, pad, 1)})
	}
	for _, n := range []struct{ table, label, next string }{
		{"purchase_orders", "Purchase orders", "PO-00001"},
		{"bar_counts", "Stock counts", "CNT-00001"},
		{"bar_transfers", "Stock transfers", "TR-00001"},
		{"bills", "Bills", "BILL-0001"},
		{"journal_entries", "Journal entries", "JE-0001"},
		{"hr_pay_runs", "Pay runs", "PR-0001"},
		{"mrp_work_orders", "Work orders", "WO-0001"},
	} {
		if env.tables[n.table] {
			out = append(out, ResetNumber{n.label, n.next})
		}
	}
	return out
}

// auditTx writes the audit entry inside tx (so it commits with the reset).
func auditTx(tx *gorm.DB, c *gin.Context, org, action string, oldV, newV interface{}) (string, error) {
	enc := func(v interface{}) string {
		b, err := json.Marshal(v)
		if err != nil || string(b) == "null" {
			return "{}"
		}
		return string(b)
	}
	var uid *string
	if u := actorID(c); u != "" {
		uid = &u
	}
	ua := c.Request.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}
	row := models.AuditLog{ID: uuid.NewString(), OrganizationID: org, UserID: uid, Action: action, EntityType: "organization", EntityID: org,
		OldValues: enc(oldV), NewValues: enc(newV), IPAddress: c.ClientIP(), UserAgent: ua, CreatedAt: time.Now()}
	return row.ID, tx.Omit("Organization", "User").Create(&row).Error
}

// ---------------------------------------------------------------- guards

// impersonating reports whether the request uses a "log in as organisation"
// session (a token scoped to another organisation than the user's own).
func (h *Handler) impersonating(c *gin.Context) bool {
	if v, ok := c.Get("impersonating"); ok {
		if b, _ := v.(bool); b {
			return true
		}
	}
	parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(parts) == 2 && parts[1] != "" {
		claims := jwt.MapClaims{}
		var err error
		if h.Cfg != nil && h.Cfg.JWTSecret != "" {
			_, err = jwt.ParseWithClaims(parts[1], claims, func(t *jwt.Token) (interface{}, error) { return []byte(h.Cfg.JWTSecret), nil })
		} else {
			_, _, err = jwt.NewParser().ParseUnverified(parts[1], claims)
		}
		if err == nil {
			if b, _ := claims["impersonating"].(bool); b {
				return true
			}
		}
	}
	if home := h.homeOrg(c); home != "" && actorOrg(c) != "" && home != actorOrg(c) {
		return true
	}
	return false
}

// homeOrg is the organisation the signed-in user belongs to ("" if unknown).
func (h *Handler) homeOrg(c *gin.Context) string {
	var org string
	if u := actorID(c); u != "" {
		h.DB.Table("users").Select("organization_id").Where("id = ?", u).Limit(1).Scan(&org)
	}
	return org
}

// resetGuard loads the organisation and refuses what may not be reset.
// It writes the error response and returns nil when refused.
func (h *Handler) resetGuard(c *gin.Context) *OrgRow {
	if h.impersonating(c) {
		fail(c, http.StatusForbidden, "PLATFORM_SESSION_REQUIRED", "Return to your platform session first: an organisation cannot be reset while you are signed in as an organisation")
		return nil
	}
	id := c.Param("id")
	org, err := h.loadOrg(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Organisation not found")
		return nil
	} else if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load organisation")
		return nil
	}
	if id == actorOrg(c) || id == h.homeOrg(c) {
		fail(c, http.StatusConflict, "OWN_ORGANIZATION", "You cannot reset your own organisation ("+org.Name+"): it is the platform's home organisation")
		return nil
	}
	var supers int64
	h.DB.Table("users").Where("organization_id = ? AND role = 'super_admin' AND deleted_at IS NULL", id).Count(&supers)
	if supers > 0 {
		accounts := "1 super admin account"
		if supers != 1 {
			accounts = fmt.Sprintf("%d super admin accounts", supers)
		}
		fail(c, http.StatusConflict, "HAS_SUPER_ADMINS", org.Name+" has "+accounts+": an organisation with platform administrators cannot be reset. Move or demote them first.")
		return nil
	}
	return org
}

// ---------------------------------------------------------------- handlers

type resetInput struct {
	DryRun        bool   `json:"dry_run"`
	KeepCustomers *bool  `json:"keep_customers"`
	KeepCatalogue *bool  `json:"keep_catalogue"`
	KeepStaff     *bool  `json:"keep_staff"`
	ConfirmName   string `json:"confirm_name"`
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// DefaultResetOptions: customers are cleared, catalogue and staff kept.
func DefaultResetOptions() orghooks.ResetOptions {
	return orghooks.ResetOptions{KeepCustomers: false, KeepCatalogue: true, KeepStaff: true}
}

// ResetOrganization previews (dry_run) or performs a reset for handover.
func (h *Handler) ResetOrganization(c *gin.Context) {
	var in resetInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	org := h.resetGuard(c)
	if org == nil {
		return
	}
	def := DefaultResetOptions()
	opts := orghooks.ResetOptions{KeepCustomers: boolOr(in.KeepCustomers, def.KeepCustomers),
		KeepCatalogue: boolOr(in.KeepCatalogue, def.KeepCatalogue), KeepStaff: boolOr(in.KeepStaff, def.KeepStaff)}
	if !in.DryRun && strings.TrimSpace(in.ConfirmName) != org.Name {
		fail(c, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Type the organisation's exact name to confirm the reset")
		return
	}
	rep, err := h.runReset(c, org, opts, in.DryRun)
	switch {
	case errors.Is(err, errResetBusy):
		fail(c, http.StatusConflict, "RESET_RUNNING", "A reset of this organisation is already running. Try again in a moment.")
		return
	case errors.Is(err, errNoOwner):
		fail(c, http.StatusConflict, "NO_OWNER", "This organisation has no admin user to hand over to, so its staff cannot be removed. Keep staff or add an admin first.")
		return
	case err != nil:
		log.Printf("platform: reset %s: %v", org.ID, err)
		fail(c, http.StatusInternalServerError, "RESET_FAILED", "The reset failed and nothing was changed: "+err.Error())
		return
	}
	ok(c, http.StatusOK, rep)
}

// sensitive columns never written to a backup.
var exportStrip = []string{"password_hash", "token", "token_hash", "secret_enc", "api_key_enc", "api_secret_enc", "client_secret",
	"refresh_token", "access_token", "secret_key", "encryption_key", "webhook_hash"}

var fileSafe = regexp.MustCompile(`[^a-z0-9]+`)

func queryBool(c *gin.Context, key string, def bool) bool {
	v := strings.TrimSpace(c.Query(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// ExportOrganization streams a ZIP backup (one JSON file per table and step,
// only this organisation's rows, child rows included) of everything a reset
// with the given options would remove or change.
func (h *Handler) ExportOrganization(c *gin.Context) {
	org := h.resetGuard(c)
	if org == nil {
		return
	}
	def := DefaultResetOptions()
	opts := orghooks.ResetOptions{KeepCustomers: queryBool(c, "keep_customers", def.KeepCustomers),
		KeepCatalogue: queryBool(c, "keep_catalogue", def.KeepCatalogue), KeepStaff: queryBool(c, "keep_staff", def.KeepStaff)}

	tx := h.DB.Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if tx.Error != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not start the backup")
		return
	}
	defer tx.Rollback()
	env, err := prepare(tx, org.ID)
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not start the backup: "+err.Error())
		return
	}
	now := time.Now()
	slug := strings.Trim(fileSafe.ReplaceAllString(strings.ToLower(org.Name), "-"), "-")
	if slug == "" {
		slug = "organisation"
	}
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-backup-%s.zip"`, slug, now.Format("20060102-1504")))
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
	zw := zip.NewWriter(c.Writer)

	type fileInfo struct {
		File  string `json:"file"`
		Group string `json:"group"`
		Table string `json:"table"`
		Kind  string `json:"kind"` // removed | before_update
		Rows  int64  `json:"rows"`
	}
	files := []fileInfo{}
	var total int64
	failed := ""
	for _, g := range resetGroups {
		if !(g.Option == "" || optionOff(g.Option, opts)) {
			continue
		}
		for _, s := range g.Steps {
			if !env.tables[s.Table] {
				continue
			}
			kind, name := "removed", g.Key+"/"+s.Table+".json"
			if s.Set != "" && !s.Soft {
				kind, name = "before_update", g.Key+"/"+s.Table+".before-update.json"
			}
			strip := ""
			for _, k := range exportStrip {
				strip += " - '" + k + "'"
			}
			if strings.HasPrefix(s.Table, "mcp_") {
				strip += " - 'hash'"
			}
			n, err := writeJSONRows(zw, name, tx, "SELECT (to_jsonb(t)"+strip+")::text FROM "+qi(s.Table)+" t WHERE "+s.Where, env.params)
			if err != nil {
				failed = s.Table + ": " + err.Error()
				break
			}
			files = append(files, fileInfo{File: name, Group: g.Key, Table: s.Table, Kind: kind, Rows: n})
			total += n
		}
		if failed != "" {
			break
		}
	}
	manifest := gin.H{
		"organization": gin.H{"id": org.ID, "name": org.Name, "business_type": org.BusinessType},
		"exported_at":  now, "exported_by": actorID(c), "options": ResetOptionsView{opts.KeepCustomers, opts.KeepCatalogue, opts.KeepStaff},
		"files": files, "rows": total, "schema": env.schema,
		"note": "Each file is a JSON array of this organisation's rows of one table (column → value). " +
			"Files named *.before-update.json hold the rows a reset changes (e.g. stock on hand, numbering) as they were. " +
			"Passwords, tokens and secrets are not included.",
	}
	if failed != "" {
		manifest["error"] = "The backup is incomplete: " + failed
		log.Printf("platform: export %s: %s", org.ID, failed)
	}
	if w, err := zw.Create("manifest.json"); err == nil {
		b, _ := json.MarshalIndent(manifest, "", "  ")
		w.Write(b)
	}
	if err := zw.Close(); err != nil {
		log.Printf("platform: export %s: %v", org.ID, err)
	}
	if failed == "" {
		Audit(h.DB, c, org.ID, "organization.export", "organization", org.ID, nil,
			gin.H{"rows": total, "files": len(files), "options": ResetOptionsView{opts.KeepCustomers, opts.KeepCatalogue, opts.KeepStaff}})
	}
}

// writeJSONRows writes the rows of q (one JSON text per row) as a JSON array.
func writeJSONRows(zw *zip.Writer, name string, tx *gorm.DB, q string, params map[string]interface{}) (int64, error) {
	rows, err := tx.Raw(q, named(q, params)...).Rows()
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	w, err := zw.Create(name)
	if err != nil {
		return 0, err
	}
	var n int64
	if _, err := w.Write([]byte("[")); err != nil {
		return 0, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return n, err
		}
		sep := ",\n"
		if n == 0 {
			sep = "\n"
		}
		if _, err := w.Write([]byte(sep + s)); err != nil {
			return n, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	_, err = w.Write([]byte("\n]\n"))
	return n, err
}
