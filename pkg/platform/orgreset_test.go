package platform

// DB tests for "reset organisation for handover". They need a FULLY migrated
// ERP schema (every table the production build creates):
//
//	TEST_DATABASE_URL=postgres://…/das_orgreset_test?sslmode=disable&search_path=erp
//
// (/home/claude/tools/gotest-orgreset.sh restores one from the schema of a
// migrated environment). Every organisation-scoped table — and the child
// tables hanging off them — is seeded generically for two organisations
// (A is reset, B must not change at all), plus rows with the values the
// reset rules depend on (owners vs staff, platform audit entries, built-in
// vendors, import link entities, numbering, balances …).

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/industry"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/onboarding"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orghooks"
)

const rtSecret = "orgreset-test-secret"

// ---------------------------------------------------------------- schema helpers

type rtCol struct {
	Name     string
	Type     string
	MaxLen   *int
	Nullable bool
	HasDef   bool
}

type rtSchema struct {
	cols   map[string][]rtCol
	orgCol map[string]string
	single map[string]bool // one row per organisation
	tables []string
}

func loadSchema(t *testing.T, db *gorm.DB) *rtSchema {
	s := &rtSchema{cols: map[string][]rtCol{}, orgCol: map[string]string{}, single: map[string]bool{}}
	var rows []struct {
		TableName              string
		ColumnName             string
		DataType               string
		CharacterMaximumLength *int
		IsNullable             string
		ColumnDefault          *string
	}
	if err := db.Raw(`SELECT c.table_name, c.column_name, c.data_type, c.character_maximum_length, c.is_nullable, c.column_default
		FROM information_schema.columns c JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
		WHERE c.table_schema = 'erp' ORDER BY c.table_name, c.ordinal_position`).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, ok := s.cols[r.TableName]; !ok {
			s.tables = append(s.tables, r.TableName)
		}
		s.cols[r.TableName] = append(s.cols[r.TableName], rtCol{Name: r.ColumnName, Type: r.DataType, MaxLen: r.CharacterMaximumLength,
			Nullable: r.IsNullable == "YES", HasDef: r.ColumnDefault != nil})
	}
	scoped, err := discoverOrgTables(db, "erp")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range scoped {
		s.orgCol[x.Name] = x.Col
	}
	uniq, err := discoverUniques(db, "erp", scoped)
	if err != nil {
		t.Fatal(err)
	}
	for tbl, idx := range uniq {
		for _, u := range idx {
			if len(u.Cols) == 1 && u.Cols[0] == s.orgCol[tbl] {
				s.single[tbl] = true
			}
		}
	}
	for tbl := range knownSingletons {
		s.single[tbl] = true
	}
	return s
}

func (s *rtSchema) col(table, name string) *rtCol {
	for i := range s.cols[table] {
		if s.cols[table][i].Name == name {
			return &s.cols[table][i]
		}
	}
	return nil
}

// rtCounter makes generated values unique across tests of one run (and runs).
var rtCounter = (time.Now().UnixNano() / 1e6) % 1e8

func (s *rtSchema) next() int64 { return atomic.AddInt64(&rtCounter, 1) }

// value generates a value for a NOT NULL column without a default.
func (s *rtSchema) value(c rtCol) interface{} {
	k := s.next()
	switch c.Type {
	case "character varying", "text", "character":
		v := strings.ReplaceAll(uuid.NewString(), "-", "")
		if c.Name == "id" {
			v = uuid.NewString()
		}
		if c.MaxLen != nil && len(v) > *c.MaxLen {
			v = v[:*c.MaxLen]
		}
		return v
	case "uuid":
		return uuid.NewString()
	case "smallint":
		return k%100 + 1
	case "integer", "bigint":
		if c.Name == "rating" {
			return k%4 + 1
		}
		return 100000000 + k
	case "numeric", "real", "double precision":
		return 2
	case "boolean":
		return false
	case "date", "timestamp with time zone", "timestamp without time zone":
		return time.Now().AddDate(0, 0, -int(k%3000))
	case "time without time zone", "time with time zone":
		return "10:00"
	case "json", "jsonb":
		return "{}"
	case "bytea":
		return []byte{1, 2, 3}
	case "interval":
		return "1 hour"
	}
	return "x"
}

// insert adds one row to table with generated values plus vals and returns
// the text value of the ret column ("" when ret is "").
func (s *rtSchema) insert(t *testing.T, db *gorm.DB, table string, vals map[string]interface{}, ret string) string {
	t.Helper()
	cols := []string{}
	args := []interface{}{}
	for _, c := range s.cols[table] {
		v, ok := vals[c.Name]
		if !ok {
			if c.Nullable || c.HasDef {
				continue
			}
			v = s.value(c)
		}
		if str, isStr := v.(string); isStr && (c.Type == "integer" || c.Type == "bigint") {
			if n, err := strconv.ParseInt(str, 10, 64); err == nil {
				v = n
			}
		}
		cols = append(cols, qi(c.Name))
		args = append(args, v)
	}
	q := "INSERT INTO " + qi(table)
	if len(cols) == 0 {
		q += " DEFAULT VALUES"
	} else {
		ph := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
		q += " (" + strings.Join(cols, ", ") + ") VALUES (" + ph + ")"
	}
	if ret != "" {
		var out string
		if err := db.Raw(q+" RETURNING "+qi(ret)+"::text", args...).Scan(&out).Error; err != nil {
			t.Fatalf("seed %s: %v", table, err)
		}
		return out
	}
	if err := db.Exec(q, args...).Error; err != nil {
		t.Fatalf("seed %s: %v", table, err)
	}
	return ""
}

// ---------------------------------------------------------------- seeding

type rtChild struct{ table, col, parent, pcol string }

var rtChildren = []rtChild{
	{"pos_items", "transaction_id", "pos_transactions", "id"},
	{"pos_payments", "transaction_id", "pos_transactions", "id"},
	{"layby_items", "layby_payment_id", "layby_payments", "id"},
	{"layby_payment_entries", "layby_payment_id", "layby_payments", "id"},
	{"billing_invoice_items", "invoice_id", "billing_invoices", "id"},
	{"invoice_line_items", "invoice_id", "invoices", "id"},
	{"booking_services", "booking_id", "bookings", "id"},
	{"shifts", "participant_id", "participants", "id"},
	{"ticket_types", "event_id", "events", "id"},
	{"event_sessions", "event_id", "events", "id"},
	{"event_images", "event_id", "events", "id"},
	{"event_reviews", "event_id", "events", "id"},
	{"bill_line_items", "bill_id", "bills", "id"},
	{"journal_entry_lines", "journal_entry_id", "journal_entries", "id"},
	{"purchase_order_items", "purchase_order_id", "purchase_orders", "id"},
	{"messages", "thread_id", "message_threads", "id"},
	{"message_participants", "thread_id", "message_threads", "id"},
	{"typing_indicators", "thread_id", "message_threads", "id"},
	{"message_attachments", "message_id", "messages", "id"},
	{"message_reactions", "message_id", "messages", "id"},
	{"message_read_statuses", "message_id", "messages", "id"},
	{"web_rtc_signals", "session_id", "screen_shares", "session_id"},
	{"call_participants", "call_id", "video_calls", "id"},
	{"call_recordings", "call_id", "video_calls", "id"},
	{"stream_viewers", "stream_id", "live_streams", "id"},
	{"stream_recordings", "stream_id", "live_streams", "id"},
	{"hr_pay_run_lines", "pay_run_id", "hr_pay_runs", "id"},
	{"documents", "participant_id", "participants", "id"},
	{"care_plans", "participant_id", "participants", "id"},
	{"emergency_contacts", "participant_id", "participants", "id"},
	{"mrp_bom_components", "bom_id", "mrp_boms", "id"},
	{"role_permissions", "role_id", "roles", "id"},
}

// Tables whose rows belong to a user of the organisation (user_id column).
var rtUserTables = map[string]bool{
	"industry_user_roles": true, "booking_email_user_prefs": true, "email_recipient_prefs": true, "assistant_user_settings": true,
	"booking_calendar_feeds": true, "mcp_tokens": true, "mcp_oauth_grants": true, "bar_location_staff": true, "hr_employees": true,
}

// Child tables of users without an organisation column.
var rtUserChildren = []string{"refresh_tokens", "user_permissions", "user_identities"}

type rtOrg struct {
	ID, Name                string
	Owner, Staff1, Staff2   string
	DeletedStaff, OwnerMail string
}

func (s *rtSchema) seedChildren(t *testing.T, db *gorm.DB, parent, pcolVal string, pcol string) {
	for _, ch := range rtChildren {
		if ch.parent != parent || ch.pcol != pcol || s.cols[ch.table] == nil {
			continue
		}
		ret := ""
		for _, c2 := range rtChildren {
			if c2.parent == ch.table {
				ret = c2.pcol
			}
		}
		v := s.insert(t, db, ch.table, map[string]interface{}{ch.col: pcolVal}, ret)
		if ret != "" {
			s.seedChildren(t, db, ch.table, v, ret)
		}
	}
}

func (s *rtSchema) mkUser(t *testing.T, db *gorm.DB, org, role string, deleted bool) string {
	id := uuid.NewString()
	vals := map[string]interface{}{"id": id, "organization_id": org, "email": "u-" + id[:12] + "@reset.test", "role": role,
		"first_name": strings.ToUpper(role[:1]) + role[1:], "last_name": id[:6], "password_hash": "$2a$10$abcdefghijklmnopqrstuv", "is_active": true,
		"created_at": time.Now(), "updated_at": time.Now()}
	if deleted {
		vals["deleted_at"] = time.Now().Add(-time.Hour)
		vals["is_active"] = false
	}
	s.insert(t, db, "users", vals, "")
	for _, ct := range rtUserChildren {
		if s.cols[ct] != nil {
			s.insert(t, db, ct, map[string]interface{}{"user_id": id}, "")
		}
	}
	return id
}

// rowsFor returns the per-row values to seed into table for o.
func (s *rtSchema) rowsFor(table string, o rtOrg) []map[string]interface{} {
	col := s.orgCol[table]
	base := func(extra map[string]interface{}) map[string]interface{} {
		m := map[string]interface{}{col: o.ID}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	if rtUserTables[table] {
		out := []map[string]interface{}{base(map[string]interface{}{"user_id": o.Owner}), base(map[string]interface{}{"user_id": o.Staff1})}
		if s.single[table] {
			return out[:1]
		}
		if table == "hr_employees" {
			out = append(out, base(map[string]interface{}{"user_id": nil}))
		}
		if table == "industry_user_roles" || table == "assistant_user_settings" || table == "booking_email_user_prefs" {
			out = append(out, base(map[string]interface{}{"user_id": o.Staff2}))
		}
		return out
	}
	switch table {
	case "audit_logs":
		return []map[string]interface{}{
			base(map[string]interface{}{"action": "organization.create", "entity_type": "organization"}),
			base(map[string]interface{}{"action": "organization.impersonate", "entity_type": "organization"}),
			base(map[string]interface{}{"action": "email_recipients.mode", "entity_type": "settings"}),
			base(map[string]interface{}{"action": "auth.sso_login", "entity_type": "user"}),
		}
	case "import_external_refs":
		return []map[string]interface{}{
			base(map[string]interface{}{"entity": "invoices"}), base(map[string]interface{}{"entity": "payments"}),
			base(map[string]interface{}{"entity": "contacts"}), base(map[string]interface{}{"entity": "items"}),
			base(map[string]interface{}{"entity": "settings"}),
		}
	case "expense_vendors":
		return []map[string]interface{}{base(map[string]interface{}{"builtin": true}), base(map[string]interface{}{"builtin": false})}
	case "pricebook_suppliers":
		return []map[string]interface{}{base(map[string]interface{}{"system": "synergy"}), base(map[string]interface{}{"system": ""})}
	case "products":
		return []map[string]interface{}{base(map[string]interface{}{"current_stock": 5}), base(map[string]interface{}{"current_stock": 0})}
	case "discounts":
		return []map[string]interface{}{base(map[string]interface{}{"usage_count": 3}), base(nil)}
	case "chart_of_accounts":
		return []map[string]interface{}{base(map[string]interface{}{"debit_balance": 100, "credit_balance": 0}), base(nil)}
	case "bank_accounts":
		return []map[string]interface{}{base(map[string]interface{}{"balance": 500, "opening_balance": 100}), base(map[string]interface{}{"balance": 0, "opening_balance": 0})}
	case "billing_settings":
		return []map[string]interface{}{base(map[string]interface{}{"next_invoice_number": 1050, "next_quote_number": 1020, "next_receipt_number": 7,
			"invoice_prefix": "INV-", "quote_prefix": "QUO-", "receipt_prefix": "RCT-", "number_padding": 4, "currency": "KES"})}
	case "bar_settings":
		return []map[string]interface{}{base(map[string]interface{}{"default_bar_id": "bar-1", "main_store_id": "store-1"})}
	case "industry_org_settings":
		return []map[string]interface{}{base(map[string]interface{}{"type_key": "nightclub", "confirmed": true, "setup_status": "done", "setup_step": "", "extra_modules": "[]"})}
	case "users":
		return nil
	}
	if s.single[table] {
		return []map[string]interface{}{base(nil)}
	}
	return []map[string]interface{}{base(nil), base(nil)}
}

func (s *rtSchema) seedOrg(t *testing.T, db *gorm.DB, name string) rtOrg {
	o := rtOrg{ID: uuid.NewString(), Name: name}
	s.insert(t, db, "organizations", map[string]interface{}{"id": o.ID, "name": name, "business_type": "nightclub", "email": "", "phone": "",
		"enabled_modules": "[]", "modules_config": "{}", "created_at": time.Now(), "updated_at": time.Now()}, "")
	o.Owner = s.mkUser(t, db, o.ID, "admin", false)
	o.Staff1 = s.mkUser(t, db, o.ID, "manager", false)
	o.Staff2 = s.mkUser(t, db, o.ID, "staff", false)
	o.DeletedStaff = s.mkUser(t, db, o.ID, "staff", true)
	db.Raw("SELECT email FROM users WHERE id = ?", o.Owner).Scan(&o.OwnerMail)
	tables := make([]string, 0, len(s.orgCol))
	for tbl := range s.orgCol {
		tables = append(tables, tbl)
	}
	sort.Strings(tables)
	for _, tbl := range tables {
		ret := ""
		for _, ch := range rtChildren {
			if ch.parent == tbl {
				ret = ch.pcol
			}
		}
		for _, vals := range s.rowsFor(tbl, o) {
			v := s.insert(t, db, tbl, vals, ret)
			if ret != "" {
				s.seedChildren(t, db, tbl, v, ret)
			}
		}
	}
	return o
}

// ---------------------------------------------------------------- snapshots

type rtSnap struct {
	total map[string]int64             // every table's row count
	sum   map[string]map[string]string // table → org → checksum of its rows
	cnt   map[string]map[string]int64
}

func snapshot(t *testing.T, db *gorm.DB, s *rtSchema, orgs ...string) rtSnap {
	sn := rtSnap{total: map[string]int64{}, sum: map[string]map[string]string{}, cnt: map[string]map[string]int64{}}
	for _, tbl := range s.tables {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM " + qi(tbl)).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		sn.total[tbl] = n
		col, ok := s.orgCol[tbl]
		if !ok {
			continue
		}
		sn.sum[tbl], sn.cnt[tbl] = map[string]string{}, map[string]int64{}
		for _, o := range orgs {
			var r struct {
				N   int64
				Sum string
			}
			if err := db.Raw("SELECT COUNT(*) AS n, COALESCE(md5(string_agg(x.t::text, '|' ORDER BY x.t::text)), '') AS sum FROM (SELECT t FROM "+qi(tbl)+" t WHERE "+qi(col)+" = ?) x", o).Scan(&r).Error; err != nil {
				t.Fatalf("%s: %v", tbl, err)
			}
			sn.sum[tbl][o], sn.cnt[tbl][o] = r.Sum, r.N
		}
	}
	// Child tables (no organisation column), counted through their parents.
	for _, ch := range append(rtChildren, rtChild{"refresh_tokens", "user_id", "users", "id"},
		rtChild{"user_permissions", "user_id", "users", "id"}, rtChild{"user_identities", "user_id", "users", "id"}) {
		if s.cols[ch.table] == nil {
			continue
		}
		key := "child:" + ch.table
		sn.cnt[key], sn.sum[key] = map[string]int64{}, map[string]string{}
		for _, o := range orgs {
			var r struct {
				N   int64
				Sum string
			}
			if err := db.Raw("SELECT COUNT(*) AS n, COALESCE(md5(string_agg(x.t::text, '|' ORDER BY x.t::text)), '') AS sum FROM (SELECT t FROM " + qi(ch.table) + " t WHERE " +
				s.parentCond(ch, o) + ") x").Scan(&r).Error; err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			sn.cnt[key][o], sn.sum[key][o] = r.N, r.Sum
		}
	}
	return sn
}

// parentCond selects the rows of ch whose parent belongs to org.
func (s *rtSchema) parentCond(ch rtChild, org string) string {
	lit := "'" + strings.ReplaceAll(org, "'", "''") + "'"
	if col, ok := s.orgCol[ch.parent]; ok {
		return qi(ch.col) + " IN (SELECT p." + qi(ch.pcol) + " FROM " + qi(ch.parent) + " p WHERE p." + qi(col) + " = " + lit + ")"
	}
	for _, up := range rtChildren {
		if up.table == ch.parent {
			return qi(ch.col) + " IN (SELECT t2." + qi(ch.pcol) + " FROM " + qi(ch.parent) + " t2 WHERE t2." + s.parentCond(up, org) + ")"
		}
	}
	return "false"
}

// ---------------------------------------------------------------- harness

type rtEnv struct {
	db     *gorm.DB
	s      *rtSchema
	r      *gin.Engine
	home   string // the super admin's own organisation
	super  string
	svc    *industry.Service
	hookMu sync.Mutex
	hooks  []string
}

var (
	rtOnce sync.Once
	rtCur  *rtEnv
)

// testFixtureTable: tables other tests of this package create in the schema
// (merge tests: mt_*) and rt_unknown, an organisation table no rule knows.
func testFixtureTable(name string) bool {
	return strings.HasPrefix(name, "mt_") || name == "rt_unknown"
}

func orgresetSetup(t *testing.T) *rtEnv {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'erp' AND table_name IN ('organizations','bar_movements','billing_invoices','pos_transactions')").Scan(&n)
	if n < 4 {
		t.Skip("TEST_DATABASE_URL is not a fully migrated ERP schema")
	}
	// An organisation table no reset rule knows: it must be reported as
	// "not touched (unclassified)" and keep its rows.
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS rt_unknown (id varchar(64) PRIMARY KEY, organization_id varchar(255) NOT NULL, note text)`).Error; err != nil {
		t.Fatal(err)
	}
	e := &rtEnv{db: db, s: loadSchema(t, db)}
	e.home = uuid.NewString()
	e.s.insert(t, db, "organizations", map[string]interface{}{"id": e.home, "name": "Platform Home " + e.home[:6], "business_type": "general",
		"enabled_modules": "[]", "modules_config": "{}", "created_at": time.Now(), "updated_at": time.Now()}, "")
	e.super = e.s.mkUser(t, db, e.home, "super_admin", false)
	e.svc = industry.NewService(db)
	gin.SetMode(gin.TestMode)
	e.r = gin.New()
	g := e.r.Group("/api", func(c *gin.Context) {
		c.Set("org_id", c.GetHeader("X-Org"))
		c.Set("user_id", c.GetHeader("X-User"))
		c.Set("user_role", c.GetHeader("X-Role"))
		c.Next()
	})
	NewHandler(db, &config.Config{JWTSecret: rtSecret}).RegisterRoutes(g)
	invoicing.NewHandler(db).RegisterRoutes(g.Group("/invoicing"))
	(&industry.Handler{Svc: e.svc}).RegisterRoutes(g)
	rtCur = e
	rtOnce.Do(func() {
		onboarding.RegisterResetHook() // the real "getting started" hook (onboarding_progress)
		orghooks.RegisterReset("orgreset-test", func(tx *gorm.DB, org string, opts orghooks.ResetOptions) error {
			// Prove the hook runs inside the reset transaction: the row is
			// visible to tx and committed with it.
			var name string
			if err := tx.Raw("SELECT name FROM organizations WHERE id = ?", org).Scan(&name).Error; err != nil {
				return err
			}
			if err := tx.Exec("INSERT INTO org_notices (organization_id, key, dismissed_at) VALUES (?, ?, now())", org, "orgreset-test-hook").Error; err != nil {
				return err
			}
			rtCur.hookMu.Lock()
			rtCur.hooks = append(rtCur.hooks, fmt.Sprintf("%s|%v|%v|%v", org, opts.KeepCustomers, opts.KeepCatalogue, opts.KeepStaff))
			rtCur.hookMu.Unlock()
			if name == "FAIL HOOK" {
				return fmt.Errorf("refused by test hook")
			}
			return nil
		})
	})
	OrgResetDone = []func(string){func(org string) { rtCur.svc.Invalidate(org) }}
	return e
}

func (e *rtEnv) call(t *testing.T, method, path string, body interface{}, hdr map[string]string) (int, map[string]interface{}, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Org", e.home)
	req.Header.Set("X-User", e.super)
	req.Header.Set("X-Role", "super_admin")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Body.Bytes()
}

func (e *rtEnv) reset(t *testing.T, org string, body map[string]interface{}) (int, ResetReport, map[string]interface{}) {
	t.Helper()
	code, out, raw := e.call(t, http.MethodPost, "/api/super-admin/platform/organizations/"+org+"/reset", body, nil)
	var env struct {
		Data ResetReport `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	return code, env.Data, out
}

func errCode(out map[string]interface{}) string {
	if e, ok := out["error"].(map[string]interface{}); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

// perTable sums a report's removed / updated rows per table.
func perTable(rep ResetReport) (removed, updated map[string]int64) {
	removed, updated = map[string]int64{}, map[string]int64{}
	for _, g := range rep.Groups {
		for _, tb := range g.Tables {
			switch tb.Action {
			case "remove":
				removed[tb.Table] += tb.Rows
			case "update":
				updated[tb.Table] += tb.Rows
			}
		}
	}
	return
}

func groupOf(rep ResetReport, key string) *ResetGroupReport {
	for i := range rep.Groups {
		if rep.Groups[i].Key == key {
			return &rep.Groups[i]
		}
	}
	return nil
}

// tablesOf lists the tables of the given groups that delete rows.
func tablesOf(keys ...string) map[string]bool {
	out := map[string]bool{}
	for _, g := range resetGroups {
		for _, k := range keys {
			if g.Key == k {
				for _, s := range g.Steps {
					if s.Set == "" {
						out[s.Table] = true
					}
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- tests

func TestOrgResetClassifiesEveryTable(t *testing.T) {
	e := orgresetSetup(t)
	missing := []string{}
	for tbl := range e.s.orgCol {
		if tbl == "organizations" || testFixtureTable(tbl) {
			continue
		}
		found := resetNotTouched[tbl] != "" || resetHookTables[tbl] != ""
		for _, g := range resetGroups {
			for _, s := range g.Steps {
				if s.Table == tbl {
					found = true
				}
			}
		}
		if !found {
			missing = append(missing, tbl)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("organisation tables without a reset rule (reported as unclassified): %v", missing)
	}
	for _, g := range resetGroups {
		for _, s := range g.Steps {
			if e.s.cols[s.Table] == nil {
				t.Errorf("group %s: table %s does not exist in the migrated schema", g.Key, s.Table)
			}
		}
	}
	for tbl := range resetNotTouched {
		if e.s.cols[tbl] == nil {
			t.Errorf("not-touched table %s does not exist", tbl)
		}
	}
}

func TestOrgResetDefaults(t *testing.T) {
	e := orgresetSetup(t)
	db, s := e.db, e.s
	a := s.seedOrg(t, db, "Club Reset A "+uuid.NewString()[:6])
	b := s.seedOrg(t, db, "Club Other B "+uuid.NewString()[:6])

	// The owner's view before the reset: setup done, no wizard.
	code, me, _ := e.call(t, http.MethodGet, "/api/industry/me", nil, map[string]string{"X-Org": a.ID, "X-User": a.Owner, "X-Role": "admin"})
	if code != 200 || me["data"].(map[string]interface{})["needs_setup"] != false {
		t.Fatalf("before reset /industry/me: %d %v", code, me)
	}

	before := snapshot(t, db, s, a.ID, b.ID)

	// Dry run with the defaults: nothing changes.
	code, dry, out := e.reset(t, a.ID, map[string]interface{}{"dry_run": true})
	if code != 200 {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if !dry.DryRun || dry.Options != (ResetOptionsView{KeepCustomers: false, KeepCatalogue: true, KeepStaff: true}) {
		t.Fatalf("dry run options: %+v", dry.Options)
	}
	unknown := false
	for _, u := range dry.Unclassified {
		if u.Table == "rt_unknown" {
			unknown = u.Rows == before.cnt["rt_unknown"][a.ID] && u.Rows > 0
		} else if !testFixtureTable(u.Table) {
			t.Errorf("unclassified table: %+v", u)
		}
	}
	if !unknown {
		t.Errorf("rt_unknown should be reported as unclassified with its rows: %+v", dry.Unclassified)
	}
	if len(dry.Missing) > 0 {
		t.Errorf("missing tables: %v", dry.Missing)
	}
	afterDry := snapshot(t, db, s, a.ID, b.ID)
	for tbl, n := range before.total {
		if afterDry.total[tbl] != n {
			t.Errorf("dry run changed %s: %d → %d", tbl, n, afterDry.total[tbl])
		}
	}
	if len(e.hooks) != 0 {
		t.Errorf("dry run called hooks: %v", e.hooks)
	}
	if g := groupOf(dry, "catalogue"); g == nil || g.Action != "keep" || g.Rows == 0 {
		t.Errorf("catalogue should be reported as kept with rows: %+v", g)
	}
	if g := groupOf(dry, "customers"); g == nil || g.Action != "remove" || g.Rows == 0 {
		t.Errorf("customers should be removed: %+v", g)
	}

	// Wrong name: refused, nothing changed.
	if code, _, out := e.reset(t, a.ID, map[string]interface{}{"confirm_name": strings.ToLower(a.Name)}); code != 400 || errCode(out) != "CONFIRMATION_REQUIRED" {
		t.Fatalf("wrong name: %d %v", code, out)
	}

	// The reset.
	code, rep, out := e.reset(t, a.ID, map[string]interface{}{"confirm_name": a.Name})
	if code != 200 || rep.DryRun {
		t.Fatalf("reset: %d %v", code, out)
	}
	after := snapshot(t, db, s, a.ID, b.ID)

	// 1. Dry run counts == reset counts == rows actually gone.
	dRem, dUpd := perTable(dry)
	rRem, rUpd := perTable(rep)
	for tbl, n := range dRem {
		if rRem[tbl] != n {
			t.Errorf("%s: dry run said %d removed, reset removed %d", tbl, n, rRem[tbl])
		}
	}
	for tbl, n := range dUpd {
		if rUpd[tbl] != n {
			t.Errorf("%s: dry run said %d updated, reset updated %d", tbl, n, rUpd[tbl])
		}
	}
	hookRows := map[string]int64{}
	for _, h := range rep.HookTables {
		hookRows[h.Table] = h.Rows
	}
	for _, tbl := range s.tables {
		want := before.total[tbl]
		if tbl != "users" {
			want -= rRem[tbl] + hookRows[tbl]
		}
		switch tbl {
		case "audit_logs":
			want++ // the reset's own entry
		case "org_notices":
			want++ // the test hook's marker
		}
		if after.total[tbl] != want {
			t.Errorf("%s: %d rows before, %d removed by the report, %d after (want %d)", tbl, before.total[tbl], rRem[tbl], after.total[tbl], want)
		}
	}
	var totalRemoved int64
	for _, n := range rRem {
		totalRemoved += n
	}
	t.Logf("reset removed %d rows in %d tables, updated %d tables; %d not-touched tables; numbering %v", totalRemoved, len(rRem), len(rUpd), len(rep.NotTouched), rep.Numbering)
	for _, g := range rep.Groups {
		t.Logf("  %-14s %-6s removed %3d updated %2d", g.Key, g.Action, g.Rows, g.Updated)
	}
	if totalRemoved < 150 {
		t.Errorf("suspiciously few rows removed: %d", totalRemoved)
	}

	if after.sum["rt_unknown"][a.ID] != before.sum["rt_unknown"][a.ID] {
		t.Errorf("an unclassified table was touched")
	}

	// 2. The other organisation is byte-for-byte unchanged (every org table
	// and every child table reached through its parents).
	for tbl := range before.sum {
		if before.sum[tbl][b.ID] != after.sum[tbl][b.ID] {
			t.Errorf("other organisation changed in %s (%d → %d rows)", tbl, before.cnt[tbl][b.ID], after.cnt[tbl][b.ID])
		}
		if before.cnt[tbl][b.ID] == 0 {
			t.Errorf("seed: other organisation has no rows in %s", tbl)
		}
	}
	for _, ch := range rtChildren {
		key := "child:" + ch.table
		if before.cnt[key][a.ID] == 0 {
			t.Errorf("seed: no %s rows for A", ch.table)
		}
		if ch.table == "role_permissions" || ch.table == "mrp_bom_components" {
			if after.sum[key][a.ID] != before.sum[key][a.ID] {
				t.Errorf("%s of A changed", ch.table)
			}
		} else if after.cnt[key][a.ID] != 0 {
			t.Errorf("%s: %d child rows of A left", ch.table, after.cnt[key][a.ID])
		}
	}

	// 3. Business records and customers of A are gone; kept groups intact.
	gone := tablesOf("sales", "invoicing", "bookings", "events", "crm", "cashbook", "bills", "accounting", "purchasing", "bar_stock",
		"inventory", "communication", "meetings", "hr_records", "projects", "production", "online_orders", "insights", "assistant", "documents", "customers")
	for tbl := range gone {
		if _, ok := s.orgCol[tbl]; !ok || tbl == "import_external_refs" {
			continue
		}
		if after.cnt[tbl][a.ID] != 0 {
			t.Errorf("%s: %d rows of A left", tbl, after.cnt[tbl][a.ID])
		}
	}
	for tbl := range tablesOf("catalogue", "staff") {
		if _, ok := s.orgCol[tbl]; !ok {
			continue
		}
		if tbl == "import_external_refs" {
			continue // links of removed documents go, item links stay (checked below)
		}
		if tbl == "products" || tbl == "discounts" || tbl == "bar_settings" || tbl == "users" {
			if after.cnt[tbl][a.ID] != before.cnt[tbl][a.ID] {
				t.Errorf("%s: row count of A changed %d → %d", tbl, before.cnt[tbl][a.ID], after.cnt[tbl][a.ID])
			}
			continue
		}
		if before.sum[tbl][a.ID] != after.sum[tbl][a.ID] {
			t.Errorf("kept table %s of A changed (%d → %d rows)", tbl, before.cnt[tbl][a.ID], after.cnt[tbl][a.ID])
		}
	}
	for tbl, why := range resetNotTouched {
		if tbl == "industry_org_settings" {
			continue
		}
		if before.sum[tbl][a.ID] != after.sum[tbl][a.ID] {
			t.Errorf("never-touched table %s (%s) of A changed", tbl, why)
		}
	}
	var refs []string
	db.Raw("SELECT entity FROM import_external_refs WHERE organization_id = ? ORDER BY entity", a.ID).Scan(&refs)
	if strings.Join(refs, ",") != "items,settings" {
		t.Errorf("import links left: %v (want items, settings)", refs)
	}
	var stock int64
	db.Raw("SELECT COALESCE(SUM(current_stock), 0) FROM products WHERE organization_id = ?", a.ID).Scan(&stock)
	if stock != 0 {
		t.Errorf("product stock not zeroed: %d", stock)
	}
	var bal struct{ Balance, Opening, Debit float64 }
	db.Raw(`SELECT (SELECT SUM(balance) FROM bank_accounts WHERE organization_id = @o) AS balance, (SELECT SUM(opening_balance) FROM bank_accounts WHERE organization_id = @o) AS opening,
		(SELECT SUM(debit_balance) FROM chart_of_accounts WHERE organization_id = @o) AS debit`, map[string]interface{}{"o": a.ID}).Scan(&bal)
	if bal.Balance != bal.Opening || bal.Debit != 0 {
		t.Errorf("balances not reset: %+v", bal)
	}
	var plat, other int64
	db.Raw("SELECT COUNT(*) FROM audit_logs WHERE organization_id = ? AND action IN ('organization.create','organization.impersonate')", a.ID).Scan(&plat)
	db.Raw("SELECT COUNT(*) FROM audit_logs WHERE organization_id = ? AND action IN ('email_recipients.mode','auth.sso_login')", a.ID).Scan(&other)
	if plat != 2 || other != 0 {
		t.Errorf("activity log: %d platform entries (want 2), %d others (want 0)", plat, other)
	}
	var audit struct{ NewValues string }
	db.Raw("SELECT new_values FROM audit_logs WHERE organization_id = ? AND action = 'organization.reset'", a.ID).Scan(&audit)
	if !strings.Contains(audit.NewValues, `"removed"`) || rep.AuditID == "" {
		t.Errorf("reset audit entry missing or without counts: %q", audit.NewValues)
	}

	// 4. Numbering restarts: the next invoice / quote get the first numbers.
	hdr := map[string]string{"X-Org": a.ID, "X-User": a.Owner, "X-Role": "admin"}
	for _, dt := range []struct{ typ, want string }{{"invoice", "INV-1001"}, {"quote", "QUO-1001"}} {
		code, out, _ := e.call(t, http.MethodPost, "/api/invoicing/invoices", map[string]interface{}{"doc_type": dt.typ, "client_name": "Walk-in",
			"items": []map[string]interface{}{{"description": "Tusker", "quantity": 2, "unit_price": 300}}}, hdr)
		if code != 201 {
			t.Fatalf("create %s after reset: %d %v", dt.typ, code, out)
		}
		if got := out["data"].(map[string]interface{})["number"]; got != dt.want {
			t.Errorf("first %s after reset is %v, want %s", dt.typ, got, dt.want)
		}
	}
	var rcpt int
	db.Raw("SELECT next_receipt_number FROM billing_settings WHERE organization_id = ?", a.ID).Scan(&rcpt)
	if rcpt != 1 {
		t.Errorf("next receipt number %d, want 1", rcpt)
	}
	foundNum := map[string]string{}
	for _, n := range rep.Numbering {
		foundNum[n.Label] = n.Next
	}
	if foundNum["Invoices"] != "INV-1001" || foundNum["Receipts"] != "RCT-0001" || foundNum["Purchase orders"] != "PO-00001" {
		t.Errorf("numbering report: %v", rep.Numbering)
	}

	// 5. Setup wizard again: pending with no step, and the owner's next /industry/me says so.
	var st struct{ SetupStatus, SetupStep, TypeKey string }
	db.Raw("SELECT setup_status, COALESCE(setup_step, '') AS setup_step, type_key FROM industry_org_settings WHERE organization_id = ?", a.ID).Scan(&st)
	if st.SetupStatus != industry.SetupPending || st.SetupStep != "" || st.TypeKey != "nightclub" {
		t.Errorf("setup after reset: %+v", st)
	}
	code, me, _ = e.call(t, http.MethodGet, "/api/industry/me", nil, hdr)
	if code != 200 || me["data"].(map[string]interface{})["needs_setup"] != true {
		t.Errorf("owner's /industry/me after reset: %d %v", code, me["data"])
	}

	if _, ok := s.orgCol["onboarding_progress"]; ok {
		if after.cnt["onboarding_progress"][a.ID] != 0 || before.cnt["onboarding_progress"][a.ID] == 0 {
			t.Errorf("onboarding progress of A: %d → %d (the onboarding hook clears it)", before.cnt["onboarding_progress"][a.ID], after.cnt["onboarding_progress"][a.ID])
		}
		if len(rep.HookTables) == 0 {
			t.Errorf("hook tables not reported")
		}
	}

	// 6. Registered hooks ran once, inside the transaction, with the options.
	if len(e.hooks) != 1 || e.hooks[0] != a.ID+"|false|true|true" {
		t.Errorf("hooks: %v", e.hooks)
	}
	var marker int64
	db.Raw("SELECT COUNT(*) FROM org_notices WHERE organization_id = ? AND key = 'orgreset-test-hook'", a.ID).Scan(&marker)
	if marker != 1 || len(rep.Hooks) == 0 || !rep.Hooks[len(rep.Hooks)-1].Ran {
		t.Errorf("hook marker %d, report %+v", marker, rep.Hooks)
	}

	// 7. Staff were kept (default) and still sign in.
	var active int64
	db.Raw("SELECT COUNT(*) FROM users WHERE organization_id = ? AND deleted_at IS NULL", a.ID).Scan(&active)
	if active != 3 {
		t.Errorf("active users after default reset: %d, want 3", active)
	}

	// A second reset of a clean organisation works and removes only the new rows.
	code, rep2, out := e.reset(t, a.ID, map[string]interface{}{"confirm_name": a.Name, "dry_run": true})
	if code != 200 {
		t.Fatalf("second dry run: %d %v", code, out)
	}
	r2, _ := perTable(rep2)
	if r2["billing_invoices"] != 2 || r2["pos_transactions"] != 0 {
		t.Errorf("second dry run: %v", r2)
	}
}

func TestOrgResetEverything(t *testing.T) {
	e := orgresetSetup(t)
	db, s := e.db, e.s
	a := s.seedOrg(t, db, "Club Reset All "+uuid.NewString()[:6])
	b := s.seedOrg(t, db, "Club Keep All "+uuid.NewString()[:6])
	before := snapshot(t, db, s, a.ID, b.ID)
	body := map[string]interface{}{"keep_customers": false, "keep_catalogue": false, "keep_staff": false}
	body["dry_run"] = true
	code, dry, out := e.reset(t, a.ID, body)
	if code != 200 {
		t.Fatalf("dry run: %d %v", code, out)
	}
	body["dry_run"] = false
	body["confirm_name"] = "  " + a.Name + " "
	code, rep, out := e.reset(t, a.ID, body)
	if code != 200 {
		t.Fatalf("reset: %d %v", code, out)
	}
	after := snapshot(t, db, s, a.ID, b.ID)
	dRem, _ := perTable(dry)
	rRem, _ := perTable(rep)
	for tbl, n := range dRem {
		if rRem[tbl] != n {
			t.Errorf("%s: dry run %d vs reset %d", tbl, n, rRem[tbl])
		}
		if tbl == "users" {
			continue
		}
		if before.total[tbl]-n != after.total[tbl] && tbl != "audit_logs" && tbl != "org_notices" && resetHookTables[tbl] == "" {
			t.Errorf("%s: %d - %d != %d", tbl, before.total[tbl], n, after.total[tbl])
		}
	}
	for tbl := range before.sum {
		if before.sum[tbl][b.ID] != after.sum[tbl][b.ID] {
			t.Errorf("other organisation changed in %s", tbl)
		}
	}
	for tbl := range tablesOf("catalogue") {
		if _, ok := s.orgCol[tbl]; !ok {
			continue
		}
		want := int64(0)
		switch tbl {
		case "pricebook_suppliers", "expense_vendors", "import_external_refs": // built-in / settings rows stay
			want = 1
		}
		if after.cnt[tbl][a.ID] != want {
			t.Errorf("catalogue table %s: %d rows of A left, want %d", tbl, after.cnt[tbl][a.ID], want)
		}
	}
	// Staff: soft-deleted like a deleted user, sessions revoked; the owner untouched.
	var users []struct {
		ID       string
		Deleted  bool
		IsActive bool
	}
	db.Raw("SELECT id, deleted_at IS NOT NULL AS deleted, is_active FROM users WHERE organization_id = ?", a.ID).Scan(&users)
	if len(users) != 4 {
		t.Fatalf("users rows: %d", len(users))
	}
	for _, u := range users {
		if u.ID == a.Owner {
			if u.Deleted || !u.IsActive {
				t.Errorf("owner was removed: %+v", u)
			}
		} else if !u.Deleted || u.IsActive {
			t.Errorf("staff user not removed: %+v", u)
		}
	}
	var revoked, ownerRevoked int64
	db.Raw("SELECT COUNT(*) FROM refresh_tokens WHERE user_id IN ? AND is_revoked", []string{a.Staff1, a.Staff2}).Scan(&revoked)
	db.Raw("SELECT COUNT(*) FROM refresh_tokens WHERE user_id = ? AND is_revoked", a.Owner).Scan(&ownerRevoked)
	if revoked != 2 || ownerRevoked != 0 {
		t.Errorf("refresh tokens revoked: staff %d (want 2), owner %d (want 0)", revoked, ownerRevoked)
	}
	var ownerRole, ownerEmp, emp int64
	db.Raw("SELECT COUNT(*) FROM industry_user_roles WHERE user_id = ?", a.Owner).Scan(&ownerRole)
	db.Raw("SELECT COUNT(*) FROM hr_employees WHERE org_id = ? AND user_id = ?", a.ID, a.Owner).Scan(&ownerEmp)
	db.Raw("SELECT COUNT(*) FROM hr_employees WHERE org_id = ?", a.ID).Scan(&emp)
	if ownerRole != 1 || ownerEmp != 1 || emp != 1 {
		t.Errorf("owner's role %d / employee %d, employees left %d", ownerRole, ownerEmp, emp)
	}
	if g := groupOf(rep, "staff"); g == nil || g.Rows == 0 {
		t.Errorf("staff group: %+v", g)
	}
	var bs struct{ DefaultBarID, MainStoreID *string }
	db.Raw("SELECT default_bar_id, main_store_id FROM bar_settings WHERE organization_id = ?", a.ID).Scan(&bs)
	if bs.DefaultBarID != nil || bs.MainStoreID != nil {
		t.Errorf("bar settings still point at deleted locations: %+v", bs)
	}
}

func TestOrgResetRefusals(t *testing.T) {
	e := orgresetSetup(t)
	db, s := e.db, e.s
	a := s.seedOrg(t, db, "Club Refuse "+uuid.NewString()[:6])
	before := snapshot(t, db, s, a.ID)
	check := func(name string, code int, out map[string]interface{}, wantCode int, want string) {
		t.Helper()
		if code != wantCode || errCode(out) != want {
			t.Errorf("%s: got %d %v, want %d %s", name, code, out, wantCode, want)
		}
	}
	// The super admin's own (home) organisation.
	code, _, out := e.reset(t, e.home, map[string]interface{}{"dry_run": true})
	check("own organisation", code, out, 409, "OWN_ORGANIZATION")
	// An organisation with a super admin member.
	withSuper := s.seedOrg(t, db, "Has Super "+uuid.NewString()[:6])
	s.mkUser(t, db, withSuper.ID, "super_admin", false)
	code, _, out = e.reset(t, withSuper.ID, map[string]interface{}{"dry_run": true})
	check("super admin member", code, out, 409, "HAS_SUPER_ADMINS")
	// Wrong / missing name.
	code, _, out = e.reset(t, a.ID, map[string]interface{}{})
	check("no name", code, out, 400, "CONFIRMATION_REQUIRED")
	code, _, out = e.reset(t, a.ID, map[string]interface{}{"confirm_name": a.Name + "x"})
	check("wrong name", code, out, 400, "CONFIRMATION_REQUIRED")
	// While impersonating ("log in as" token, scoped to the organisation).
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"user_id": e.super, "role": "super_admin", "org_id": a.ID,
		"impersonating": true, "home_org_id": e.home, "exp": time.Now().Add(time.Hour).Unix()}).SignedString([]byte(rtSecret))
	code, outI, _ := e.call(t, http.MethodPost, "/api/super-admin/platform/organizations/"+a.ID+"/reset", map[string]interface{}{"confirm_name": a.Name},
		map[string]string{"Authorization": "Bearer " + tok, "X-Org": a.ID})
	check("impersonating", code, outI, 403, "PLATFORM_SESSION_REQUIRED")
	// Not a super admin.
	code, outR, _ := e.call(t, http.MethodPost, "/api/super-admin/platform/organizations/"+a.ID+"/reset", map[string]interface{}{"dry_run": true},
		map[string]string{"X-Role": "admin"})
	if code != 403 {
		t.Errorf("admin: %d %v", code, outR)
	}
	// One reset per organisation at a time (advisory lock).
	lock := db.Begin()
	if err := lock.Exec(fmt.Sprintf("SELECT pg_advisory_xact_lock(%d, hashtext(?))", resetLockClass), a.ID).Error; err != nil {
		t.Fatal(err)
	}
	code, _, out = e.reset(t, a.ID, map[string]interface{}{"confirm_name": a.Name})
	lock.Rollback()
	check("busy", code, out, 409, "RESET_RUNNING")
	// Removing staff needs someone to hand over to.
	noOwner := s.seedOrg(t, db, "No Owner "+uuid.NewString()[:6])
	db.Exec("UPDATE users SET role = 'staff' WHERE id = ?", noOwner.Owner)
	db.Exec("DELETE FROM industry_user_roles WHERE organization_id = ?", noOwner.ID)
	code, _, out = e.reset(t, noOwner.ID, map[string]interface{}{"confirm_name": noOwner.Name, "keep_staff": false})
	check("no owner", code, out, 409, "NO_OWNER")
	// A failing hook rolls the whole reset back.
	failing := s.seedOrg(t, db, "FAIL HOOK")
	fb := snapshot(t, db, s, failing.ID)
	code, _, out = e.reset(t, failing.ID, map[string]interface{}{"confirm_name": "FAIL HOOK"})
	check("failing hook", code, out, 500, "RESET_FAILED")
	fa := snapshot(t, db, s, failing.ID)
	for tbl := range fb.sum {
		if fb.sum[tbl][failing.ID] != fa.sum[tbl][failing.ID] {
			t.Errorf("failed reset changed %s", tbl)
		}
	}
	db.Exec("UPDATE organizations SET name = 'FAILED HOOK ORG' WHERE id = ?", failing.ID)

	after := snapshot(t, db, s, a.ID)
	for tbl := range before.sum {
		if before.sum[tbl][a.ID] != after.sum[tbl][a.ID] {
			t.Errorf("a refused reset changed %s", tbl)
		}
	}
}

func TestOrgResetExport(t *testing.T) {
	e := orgresetSetup(t)
	db, s := e.db, e.s
	a := s.seedOrg(t, db, "Club Export "+uuid.NewString()[:6])
	s.seedOrg(t, db, "Club Export Other "+uuid.NewString()[:6])
	code, dry, out := e.reset(t, a.ID, map[string]interface{}{"dry_run": true, "keep_staff": false})
	if code != 200 {
		t.Fatalf("dry run: %d %v", code, out)
	}
	code, _, raw := e.call(t, http.MethodGet, "/api/super-admin/platform/organizations/"+a.ID+"/export?keep_staff=false", nil, nil)
	if code != 200 {
		t.Fatalf("export: %d %s", code, raw)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	files := map[string][]map[string]interface{}{}
	var manifest map[string]interface{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "manifest.json" {
			if err := json.Unmarshal(b, &manifest); err != nil {
				t.Fatalf("manifest: %v", err)
			}
			continue
		}
		var rows []map[string]interface{}
		if err := json.Unmarshal(b, &rows); err != nil {
			t.Fatalf("%s: invalid JSON: %v", f.Name, err)
		}
		files[f.Name] = rows
	}
	if manifest == nil || manifest["error"] != nil {
		t.Fatalf("manifest: %v", manifest)
	}
	for _, g := range dry.Groups {
		if g.Action != "remove" {
			continue
		}
		for _, tb := range g.Tables {
			name := g.Key + "/" + tb.Table + ".json"
			if tb.Action == "update" {
				name = g.Key + "/" + tb.Table + ".before-update.json"
			}
			rows, ok := files[name]
			if !ok {
				t.Errorf("backup has no %s", name)
				continue
			}
			if int64(len(rows)) != tb.Rows {
				t.Errorf("%s: %d rows in the backup, the reset changes %d", name, len(rows), tb.Rows)
			}
			if col := s.orgCol[tb.Table]; col != "" {
				for _, r := range rows {
					if r[col] != a.ID {
						t.Errorf("%s: row of another organisation in the backup: %v", name, r[col])
					}
				}
			}
		}
	}
	users := files["staff/users.json"]
	if len(users) != 2 {
		t.Errorf("staff users in backup: %d", len(users))
	}
	for _, u := range users {
		if _, has := u["password_hash"]; has {
			t.Error("password hashes must not be exported")
		}
	}
	// Refused like the reset itself.
	code, _, _ = e.call(t, http.MethodGet, "/api/super-admin/platform/organizations/"+e.home+"/export", nil, nil)
	if code != 409 {
		t.Errorf("export of own organisation: %d", code)
	}
}
