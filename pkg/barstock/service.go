package barstock

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/mail"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Mailer sends email as an organisation (pkg/mail.Service).
type Mailer interface {
	SendAs(ctx context.Context, orgID string, msg mail.Message) (mail.Config, error)
}

// Service holds the bar stock logic. The handlers, the POS / purchase order
// hooks and the night report scheduler all go through it.
type Service struct {
	DB *gorm.DB
	// Can reports whether a user may perform act (view|create|edit|delete|
	// approve) on module (pkg/industry: plan → business type → role). When
	// nil the legacy users.role decides (admin/manager: everything; staff:
	// view + create).
	Can func(org, user, role, module, act string) bool
	// NextPONumber is the purchase order numbering of internal/handlers
	// (wired in cmd/production/routes_bar.go so it is not duplicated).
	NextPONumber func(tx *gorm.DB, org string) string
	Mail         Mailer
	AppURL       string
	LogoURL      func(org string) string
	// Now is the clock (tests replace it).
	Now func() time.Time
}

// New returns a service with the real clock.
func New(db *gorm.DB) *Service { return &Service{DB: db, Now: time.Now} }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------- errors

// Error is a user-facing failure with an HTTP status.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// HTTPStatus lets other packages (the POS hook) map the error.
func (e *Error) HTTPStatus() int { return e.Status }

func errf(status int, format string, a ...any) error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, a...)}
}

func badReq(format string, a ...any) error   { return errf(http.StatusBadRequest, format, a...) }
func notFound(format string, a ...any) error { return errf(http.StatusNotFound, format, a...) }
func conflict(format string, a ...any) error { return errf(http.StatusConflict, format, a...) }
func forbidden(format string, a ...any) error {
	return errf(http.StatusForbidden, format, a...)
}

// statusOf maps an error to an HTTP status (500 for unexpected ones).
func statusOf(err error) (int, string) {
	var e *Error
	if errors.As(err, &e) {
		return e.Status, e.Msg
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return http.StatusNotFound, "Not found"
	}
	return http.StatusInternalServerError, "Something went wrong: " + err.Error()
}

// ---------------------------------------------------------------- readiness

var tablesOK atomic.Bool

// tablesReady reports whether the bar tables exist (never raises an error,
// so it is safe inside a caller's transaction). Hooks do nothing until then.
func tablesReady(db *gorm.DB) bool {
	if tablesOK.Load() {
		return true
	}
	var ok bool
	if err := db.Raw("SELECT to_regclass('bar_items') IS NOT NULL AND to_regclass('bar_recipes') IS NOT NULL AND to_regclass('bar_sale_lines') IS NOT NULL").Row().Scan(&ok); err != nil {
		return false
	}
	if ok {
		tablesOK.Store(true)
	}
	return ok
}

func setReady(v bool) { tablesOK.Store(v) }

// ---------------------------------------------------------------- settings

// DefaultSettings are used until an organisation saves its own.
func DefaultSettings(org string) Settings {
	return Settings{OrganizationID: org, TotML: 25, DayCutoff: "06:00", VariancePctThreshold: 5, PourCostTargets: map[string]*float64{},
		NightReportSendAt: "07:00"}
}

// Settings returns the organisation's settings (defaults when none saved).
func (s *Service) Settings(db *gorm.DB, org string) Settings {
	st := DefaultSettings(org)
	var row Settings
	if err := db.Where("organization_id = ?", org).Limit(1).Find(&row).Error; err == nil && row.OrganizationID != "" {
		st = row
		if st.TotML <= 0 {
			st.TotML = 25
		}
		if _, _, ok := ParseHM(st.DayCutoff); !ok {
			st.DayCutoff = "06:00"
		}
		if _, _, ok := ParseHM(st.NightReportSendAt); !ok {
			st.NightReportSendAt = "07:00"
		}
		if st.PourCostTargets == nil {
			st.PourCostTargets = map[string]*float64{}
		}
	}
	return st
}

func (s *Service) saveSettings(db *gorm.DB, st Settings) error {
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "organization_id"}}, UpdateAll: true}).Create(&st).Error
}

var (
	locMu    sync.Mutex
	locCache = map[string]*time.Location{}
)

func loadLoc(tz string) *time.Location {
	locMu.Lock()
	defer locMu.Unlock()
	if l, ok := locCache[tz]; ok {
		return l
	}
	l, err := time.LoadLocation(tz)
	if err != nil {
		l = time.UTC
	}
	locCache[tz] = l
	return l
}

// Location is the organisation's time zone (pkg/orgprefs).
func (s *Service) Location(db *gorm.DB, org string) *time.Location {
	return loadLoc(orgprefs.Get(db, org).Timezone)
}

// Currency is the organisation's home currency.
func (s *Service) Currency(db *gorm.DB, org string) string { return orgprefs.Currency(db, org) }

// BusinessDateAt is the business date of t for the organisation.
func (s *Service) BusinessDateAt(db *gorm.DB, org string, t time.Time) string {
	st := s.Settings(db, org)
	return BusinessDate(t, s.Location(db, org), st.DayCutoff)
}

// Today is the current business date.
func (s *Service) Today(db *gorm.DB, org string) string { return s.BusinessDateAt(db, org, s.now()) }

// ---------------------------------------------------------------- items

// ItemRow is a stock item joined with its product.
type ItemRow struct {
	Item
	Name         string
	SKU          string
	Barcode      string
	SellingPrice float64
	CostPrice    float64
	IsActive     bool
}

const itemSelect = `bar_items.*, products.name AS name, COALESCE(products.sku,'') AS sku, COALESCE(products.barcode,'') AS barcode,
	COALESCE(products.selling_price,0) AS selling_price, COALESCE(products.cost_price,0) AS cost_price, COALESCE(products.is_active,true) AS is_active`

func itemQuery(db *gorm.DB, org string) *gorm.DB {
	return db.Table("bar_items").Select(itemSelect).
		Joins("JOIN products ON products.id = bar_items.product_id AND products.deleted_at IS NULL").
		Where("bar_items.organization_id = ?", org)
}

// Items loads stock items by product id (all of the organisation when ids is nil).
func (s *Service) Items(db *gorm.DB, org string, ids []string) (map[string]*ItemRow, error) {
	q := itemQuery(db, org)
	if ids != nil {
		if len(ids) == 0 {
			return map[string]*ItemRow{}, nil
		}
		q = q.Where("bar_items.product_id IN ?", ids)
	}
	var rows []ItemRow
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]*ItemRow, len(rows))
	for i := range rows {
		out[rows[i].ProductID] = &rows[i]
	}
	return out, nil
}

func (s *Service) item(db *gorm.DB, org, id string) (*ItemRow, error) {
	m, err := s.Items(db, org, []string{id})
	if err != nil {
		return nil, err
	}
	it, ok := m[id]
	if !ok {
		return nil, notFound("Stock item not found")
	}
	return it, nil
}

// SpecOf is the unit maths view of an item.
func SpecOf(it *Item, st Settings) Spec {
	tot := st.TotML
	if it.TotML != nil && *it.TotML > 0 {
		tot = *it.TotML
	}
	return Spec{BaseUnit: it.BaseUnit, UnitSize: it.UnitSize, UnitLabel: it.UnitLabel, PackSize: it.PackSize, PackLabel: it.PackLabel, TotML: tot}
}

// ---------------------------------------------------------------- locations

// LocationRow is an inventory location.
type LocationRow struct {
	ID          string
	Name        string
	Type        string
	Description string
	IsActive    bool
}

func (s *Service) locations(db *gorm.DB, org string) ([]LocationRow, error) {
	var rows []LocationRow
	err := db.Table("inventory_locations").Select("id, name, COALESCE(type,'') AS type, COALESCE(description,'') AS description, COALESCE(is_active,true) AS is_active").
		Where("organization_id = ? AND deleted_at IS NULL", org).Order("name").Scan(&rows).Error
	return rows, err
}

func (s *Service) location(db *gorm.DB, org, id string) (*LocationRow, error) {
	if strings.TrimSpace(id) == "" {
		return nil, badReq("Choose a location")
	}
	var rows []LocationRow
	if err := db.Table("inventory_locations").Select("id, name, COALESCE(type,'') AS type, COALESCE(description,'') AS description, COALESCE(is_active,true) AS is_active").
		Where("id = ? AND organization_id = ? AND deleted_at IS NULL", id, org).Limit(1).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, notFound("Location not found")
	}
	return &rows[0], nil
}

func (s *Service) locationNames(db *gorm.DB, org string) map[string]string {
	rows, _ := s.locations(db, org)
	m := map[string]string{}
	for _, r := range rows {
		m[r.ID] = r.Name
	}
	return m
}

// mainStore is the receiving default: the setting, else the first store.
func (s *Service) mainStore(db *gorm.DB, org string) (string, error) {
	st := s.Settings(db, org)
	if st.MainStoreID != nil && *st.MainStoreID != "" {
		if _, err := s.location(db, org, *st.MainStoreID); err == nil {
			return *st.MainStoreID, nil
		}
	}
	locs, err := s.locations(db, org)
	if err != nil {
		return "", err
	}
	for _, l := range locs {
		if l.Type == "store" && l.IsActive {
			return l.ID, nil
		}
	}
	return "", badReq("Set up a main store first")
}

// defaultBar is the bar POS sells from when nothing else says.
func (s *Service) defaultBar(db *gorm.DB, org string) string {
	st := s.Settings(db, org)
	if st.DefaultBarID != nil && *st.DefaultBarID != "" {
		if _, err := s.location(db, org, *st.DefaultBarID); err == nil {
			return *st.DefaultBarID
		}
	}
	locs, _ := s.locations(db, org)
	for _, l := range locs {
		if l.Type == "bar" && l.IsActive {
			return l.ID
		}
	}
	return ""
}

// ---------------------------------------------------------------- ledger

// mv is one stock change to post.
type mv struct {
	Loc, Product string
	Qty          float64
	Kind         string
	RefType      string
	RefID        string
	RefLabel     string
	Note         string
	UnitCost     float64
	User         string
	BD           string
	Oversold     bool
	At           time.Time
}

// post applies a movement: bar_stock += qty and a ledger row. It returns the
// new quantity at the location.
func (s *Service) post(tx *gorm.DB, org string, m mv) (float64, error) {
	q := round3(m.Qty)
	if q == 0 {
		return 0, nil
	}
	at := m.At
	if at.IsZero() {
		at = s.now()
	}
	var newQty float64
	if err := tx.Raw(`INSERT INTO bar_stock (organization_id, location_id, product_id, qty, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (organization_id, location_id, product_id) DO UPDATE SET qty = bar_stock.qty + EXCLUDED.qty, updated_at = EXCLUDED.updated_at
		RETURNING qty`, org, m.Loc, m.Product, q, at).Row().Scan(&newQty); err != nil {
		return 0, err
	}
	row := Movement{ID: uuid.NewString(), OrganizationID: org, LocationID: m.Loc, ProductID: m.Product, Qty: q, Kind: m.Kind,
		RefType: m.RefType, RefID: m.RefID, RefLabel: m.RefLabel, UnitCost: m.UnitCost, UserID: m.User, BusinessDate: m.BD,
		Note: m.Note, Oversold: m.Oversold, CreatedAt: at}
	if err := tx.Create(&row).Error; err != nil {
		return 0, err
	}
	return newQty, nil
}

// stockAt returns qty per product at a location ("" = total over locations).
func (s *Service) stockAt(db *gorm.DB, org, loc string, ids []string) (map[string]float64, error) {
	type r struct {
		ProductID string
		Qty       float64
	}
	var rows []r
	q := db.Table("bar_stock").Select("product_id, SUM(qty) AS qty").Where("organization_id = ?", org)
	if loc != "" {
		q = q.Where("location_id = ?", loc)
	}
	if ids != nil {
		if len(ids) == 0 {
			return map[string]float64{}, nil
		}
		q = q.Where("product_id IN ?", ids)
	}
	if err := q.Group("product_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, x := range rows {
		out[x.ProductID] = round3(x.Qty)
	}
	return out, nil
}

// stockByLocation returns product → location → qty.
func (s *Service) stockByLocation(db *gorm.DB, org string, ids []string) (map[string]map[string]float64, error) {
	var rows []Stock
	q := db.Where("organization_id = ?", org)
	if ids != nil {
		if len(ids) == 0 {
			return map[string]map[string]float64{}, nil
		}
		q = q.Where("product_id IN ?", ids)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]map[string]float64{}
	for _, r := range rows {
		if out[r.ProductID] == nil {
			out[r.ProductID] = map[string]float64{}
		}
		out[r.ProductID][r.LocationID] = round3(r.Qty)
	}
	return out, nil
}

// sync keeps products.current_stock in step for the stock items given:
// floor(total base qty ÷ unit size) (never below 0), and menu items made from
// them: how many could be made from the total stock.
func (s *Service) sync(tx *gorm.DB, org string, ids []string) error {
	ids = uniq(ids)
	if len(ids) == 0 {
		return nil
	}
	if err := tx.Exec(`UPDATE products SET current_stock = GREATEST(0, FLOOR(COALESCE((SELECT SUM(st.qty) FROM bar_stock st
			WHERE st.organization_id = bar_items.organization_id AND st.product_id = bar_items.product_id), 0) / NULLIF(bar_items.unit_size, 0)))::int,
			updated_at = ?
		FROM bar_items WHERE bar_items.product_id = products.id AND bar_items.organization_id = ? AND products.organization_id = ? AND products.id IN ?`,
		s.now(), org, org, ids).Error; err != nil {
		return err
	}
	// Menu items (recipes that are not stock items themselves).
	return tx.Exec(`UPDATE products SET current_stock = sub.avail, updated_at = ?
		FROM (
			SELECT l.pos_product_id, GREATEST(0, MIN(FLOOR(COALESCE(t.total, 0) / l.qty)))::int AS avail
			FROM bar_recipe_lines l
			JOIN bar_items bi ON bi.product_id = l.product_id AND bi.organization_id = l.organization_id AND bi.track
			LEFT JOIN (SELECT product_id, SUM(qty) AS total FROM bar_stock WHERE organization_id = ? GROUP BY product_id) t ON t.product_id = l.product_id
			WHERE l.organization_id = ? AND l.qty > 0 AND l.pos_product_id IN (SELECT pos_product_id FROM bar_recipe_lines WHERE organization_id = ? AND product_id IN ?)
			  AND l.pos_product_id NOT IN (SELECT product_id FROM bar_items WHERE organization_id = ?)
			GROUP BY l.pos_product_id
		) sub
		WHERE products.id = sub.pos_product_id AND products.organization_id = ?`, s.now(), org, org, org, ids, org, org).Error
}

// ---------------------------------------------------------------- users

func (s *Service) userNames(db *gorm.DB, ids []string) map[string]string {
	out := map[string]string{}
	ids = uniq(ids)
	if len(ids) == 0 {
		return out
	}
	var rows []models.POSUserRef
	db.Where("id IN ?", ids).Find(&rows)
	for _, u := range rows {
		n := strings.TrimSpace(u.FirstName + " " + u.LastName)
		if n == "" {
			n = u.Email
		}
		out[u.ID] = n
	}
	return out
}

// ---------------------------------------------------------------- numbering

// nextNumber returns PREFIX-00001 style numbers per organisation.
func nextNumber(tx *gorm.DB, table, org, prefix string) string {
	var n int64
	tx.Table(table).Where("organization_id = ?", org).Count(&n)
	for i := n + 1; i < n+1000; i++ {
		num := fmt.Sprintf("%s-%05d", prefix, i)
		var exists int64
		tx.Table(table).Where("organization_id = ? AND number = ?", org, num).Count(&exists)
		if exists == 0 {
			return num
		}
	}
	return prefix + "-" + strings.ToUpper(uuid.NewString()[:8])
}

// ---------------------------------------------------------------- helpers

func uniq(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ptr[T any](v T) *T { return &v }

func validCategory(c string) bool {
	for _, x := range Categories {
		if x == c {
			return true
		}
	}
	return false
}

func validLocationType(t string) bool {
	for _, x := range LocationTypes {
		if x == t {
			return true
		}
	}
	return false
}

func validReason(r string) bool {
	for _, x := range WastageReasons {
		if x == r {
			return true
		}
	}
	return false
}

func money6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
