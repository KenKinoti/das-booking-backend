package pricebook

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/expenses"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/fxrates"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/payments"
)

// Service is the price book.
type Service struct {
	DB  *gorm.DB
	FX  *fxrates.Service
	Now func() time.Time
}

// New builds the service.
func New(db *gorm.DB, fx *fxrates.Service) *Service {
	if fx == nil {
		fx = &fxrates.Service{DB: db}
	}
	return &Service{DB: db, FX: fx, Now: time.Now}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ValidationError is a user-facing input problem.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, a...)} }

// ErrNotFound is returned for a record that is not the organisation's.
var ErrNotFound = errors.New("not found")

// Home is the organisation's home currency.
func (s *Service) Home(org string) string { return orgprefs.Currency(s.DB, org) }

// ---------------------------------------------------------------- settings & seed

func (s *Service) settings(org string) *Settings {
	var st Settings
	if err := s.DB.Where("organization_id = ?", org).Take(&st).Error; err != nil {
		st = Settings{OrganizationID: org}
	}
	if len(st.Tiers) == 0 {
		st.Tiers = DefaultTiers()
	}
	return &st
}

// Tiers returns the organisation's hosting tiers.
func (s *Service) Tiers(org string) []Tier { return s.settings(org).Tiers }

// SaveTiers replaces the hosting tiers (keys stay starter / standard /
// business; labels and thresholds change).
func (s *Service) SaveTiers(org string, tiers []Tier) ([]Tier, error) {
	out := []Tier{}
	seen := map[string]bool{}
	for _, t := range tiers {
		t.Key = token(t.Key)
		t.Label = strings.TrimSpace(t.Label)
		if t.Key == "" {
			t.Key = token(t.Label)
		}
		if t.Key == "" || seen[t.Key] {
			continue
		}
		if t.Label == "" {
			t.Label = strings.ToUpper(t.Key[:1]) + t.Key[1:]
		}
		if t.MinDiskGB < 0 || t.MinDiskGB > 100000 {
			return nil, invalid("%s: the disk space must be zero or more", t.Label)
		}
		if t.MinSites < 1 {
			t.MinSites = 1
		}
		seen[t.Key] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, invalid("keep at least one tier")
	}
	st := s.settings(org)
	st.Tiers, st.UpdatedAt = out, s.now()
	if err := s.DB.Save(st).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// EnsureSeed gives a new organisation its three suppliers once: Synergy
// Wholesale (AUD), hosting.com (USD) and a self-hosted server in the home
// currency. Deleting them later does not bring them back.
func (s *Service) EnsureSeed(org string) {
	st := s.settings(org)
	if st.SeededAt != nil {
		return
	}
	var n int64
	s.DB.Model(&Supplier{}).Where("organization_id = ?", org).Count(&n)
	now := s.now()
	if n == 0 {
		home := s.Home(org)
		for _, sp := range []Supplier{
			{Name: "Synergy Wholesale", Type: TypeWholesale, Currency: "AUD", Website: "https://synergywholesale.com", System: "synergy"},
			{Name: "hosting.com", Type: TypeWholesale, Currency: "USD", Website: "https://hosting.com", System: "hosting_com"},
			{Name: "Self-hosted server", Type: TypeSelfHosted, Currency: home, System: "self_hosted", Costs: DefaultCostLines()},
		} {
			sp.ID, sp.OrganizationID, sp.NameKey, sp.CreatedAt, sp.UpdatedAt = uuid.NewString(), org, NameKey(sp.Name), now, now
			if sp.Costs == nil {
				sp.Costs = []CostLine{}
			}
			// A second request racing this one hits the unique name index.
			s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&sp)
			now = now.Add(time.Millisecond)
		}
	}
	st.SeededAt = &now
	st.UpdatedAt = now
	s.DB.Save(st)
}

// ---------------------------------------------------------------- suppliers

// SupplierInput creates or changes a supplier (nil = unchanged).
type SupplierInput struct {
	Name         *string     `json:"name"`
	Type         *string     `json:"type"`
	Currency     *string     `json:"currency"`
	Website      *string     `json:"website"`
	Notes        *string     `json:"notes"`
	Costs        *[]CostLine `json:"costs"`
	Capacity     *int        `json:"capacity"`
	CurrentSites *int        `json:"current_sites"`
	DiskGB       *float64    `json:"disk_gb"`
}

// SupplierView is a supplier with its price list summary.
type SupplierView struct {
	Supplier
	Items    int    `json:"items"`
	Stale    int    `json:"stale"`
	LatestAt string `json:"latest_as_of,omitempty"`
	OldestAt string `json:"oldest_as_of,omitempty"`
}

var typeOrder = map[string]int{TypeWholesale: 0, TypeSelfHosted: 1, TypeCompetitor: 2}

// Suppliers lists the organisation's suppliers: wholesale, self-hosted,
// then competitors, each in the order they were added.
func (s *Service) Suppliers(org string) []Supplier {
	out := []Supplier{}
	s.DB.Where("organization_id = ?", org).Order("created_at, name").Find(&out)
	sort.SliceStable(out, func(i, j int) bool { return typeOrder[out[i].Type] < typeOrder[out[j].Type] })
	for i := range out {
		if out[i].Costs == nil {
			out[i].Costs = []CostLine{}
		}
	}
	return out
}

func (s *Service) supplier(org, id string) (*Supplier, error) {
	var sp Supplier
	if err := s.DB.Where("organization_id = ? AND id = ?", org, id).Take(&sp).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if sp.Costs == nil {
		sp.Costs = []CostLine{}
	}
	return &sp, nil
}

func cleanCosts(lines []CostLine, supplierCur string) ([]CostLine, error) {
	out := []CostLine{}
	for i, l := range lines {
		l.Label = strings.TrimSpace(l.Label)
		if l.Label == "" {
			if LineMonthly(l) == 0 {
				continue
			}
			return nil, invalid("give cost line %d a name", i+1)
		}
		if len(l.Label) > 80 {
			l.Label = l.Label[:80]
		}
		if l.Kind != "time" {
			l.Kind = "fixed"
			l.Hours, l.Rate = 0, 0
		} else {
			l.Amount = 0
		}
		if l.Amount < 0 || l.Hours < 0 || l.Rate < 0 {
			return nil, invalid("%s: amounts cannot be negative", l.Label)
		}
		l.Currency = strings.ToUpper(strings.TrimSpace(l.Currency))
		if l.Currency == supplierCur {
			l.Currency = ""
		}
		if l.Currency != "" && !currency.IsSupported(l.Currency) {
			return nil, invalid("%s: choose a supported currency", l.Label)
		}
		if l.Key = token(l.Key); l.Key == "" {
			l.Key = "c" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		}
		if len(l.Note) > 200 {
			l.Note = l.Note[:200]
		}
		out = append(out, l)
	}
	if len(out) > 40 {
		return nil, invalid("too many cost lines")
	}
	return out, nil
}

// SaveSupplier creates (id "") or updates a supplier.
func (s *Service) SaveSupplier(org, id string, in SupplierInput) (*Supplier, error) {
	now := s.now()
	var sp *Supplier
	if id == "" {
		sp = &Supplier{ID: uuid.NewString(), OrganizationID: org, Type: TypeWholesale, Currency: s.Home(org), Costs: []CostLine{}, CreatedAt: now}
		if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
			return nil, invalid("give the supplier a name")
		}
	} else {
		var err error
		if sp, err = s.supplier(org, id); err != nil {
			return nil, err
		}
	}
	if in.Name != nil {
		name := reSpaces.ReplaceAllString(strings.TrimSpace(*in.Name), " ")
		if name == "" {
			return nil, invalid("give the supplier a name")
		}
		if len(name) > 160 {
			return nil, invalid("the name is too long")
		}
		sp.Name, sp.NameKey = name, NameKey(name)
	}
	if in.Type != nil {
		t, ok := ParseType(*in.Type)
		if !ok {
			return nil, invalid("the type must be wholesale, self-hosted or competitor")
		}
		sp.Type = t
	}
	if in.Currency != nil {
		c, ok := ParseCurrency(*in.Currency)
		if !ok {
			return nil, invalid("choose a supported currency")
		}
		sp.Currency = c
	}
	if in.Website != nil {
		w, ok := CleanURL(*in.Website)
		if !ok || len(w) > 255 {
			return nil, invalid("the website must be a web address (https://…)")
		}
		sp.Website = w
	}
	if in.Notes != nil {
		sp.Notes = strings.TrimSpace(*in.Notes)
	}
	if in.Capacity != nil {
		if *in.Capacity < 0 || *in.Capacity > 100000 {
			return nil, invalid("the number of sites per server must be zero or more")
		}
		sp.Capacity = *in.Capacity
	}
	if in.CurrentSites != nil {
		if *in.CurrentSites < 0 || *in.CurrentSites > 100000 {
			return nil, invalid("the current number of sites must be zero or more")
		}
		sp.CurrentSites = *in.CurrentSites
	}
	if in.DiskGB != nil {
		if *in.DiskGB < 0 {
			return nil, invalid("the disk space must be zero or more")
		}
		sp.DiskGB = *in.DiskGB
	}
	if in.Costs != nil {
		lines, err := cleanCosts(*in.Costs, sp.Currency)
		if err != nil {
			return nil, err
		}
		sp.Costs = lines
	}
	if sp.Type == TypeSelfHosted && len(sp.Costs) == 0 && id == "" {
		sp.Costs = DefaultCostLines()
	}
	var dup int64
	s.DB.Model(&Supplier{}).Where("organization_id = ? AND name_key = ? AND id <> ?", org, sp.NameKey, sp.ID).Count(&dup)
	if dup > 0 {
		return nil, invalid("there is already a supplier called %s", sp.Name)
	}
	sp.UpdatedAt = now
	if err := s.DB.Save(sp).Error; err != nil {
		return nil, err
	}
	return sp, nil
}

// DeleteSupplier removes a supplier with its price list and history.
func (s *Service) DeleteSupplier(org, id string) error {
	sp, err := s.supplier(org, id)
	if err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("organization_id = ? AND item_id IN (?)", org, tx.Model(&Item{}).Select("id").Where("organization_id = ? AND supplier_id = ?", org, sp.ID)).Delete(&PriceChange{}).Error; err != nil {
			return err
		}
		if err := tx.Where("organization_id = ? AND supplier_id = ?", org, sp.ID).Delete(&Item{}).Error; err != nil {
			return err
		}
		return tx.Where("organization_id = ? AND id = ?", org, sp.ID).Delete(&Supplier{}).Error
	})
}

// ---------------------------------------------------------------- items

// Opt is an optional JSON field that tells "absent" from "null".
type Opt[T any] struct {
	Set  bool
	Null bool
	V    T
}

// UnmarshalJSON records that the field was sent.
func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.V)
}

// Of builds a set Opt (for callers in Go).
func Of[T any](v T) Opt[T] { return Opt[T]{Set: true, V: v} }

// Null builds an Opt that clears the field.
func Null[T any]() Opt[T] { return Opt[T]{Set: true, Null: true} }

// ItemInput creates or changes a price (fields not sent stay as they are;
// null clears the optional ones).
type ItemInput struct {
	SupplierID    Opt[string]  `json:"supplier_id"`
	Kind          Opt[string]  `json:"kind"`
	Name          Opt[string]  `json:"name"`
	TLD           Opt[string]  `json:"tld"`
	Period        Opt[string]  `json:"period"`
	Price         Opt[float64] `json:"price"`
	Currency      Opt[string]  `json:"currency"`
	SetupFee      Opt[float64] `json:"setup_fee"`
	RegularPrice  Opt[float64] `json:"regular_price"`
	DiskGB        Opt[float64] `json:"disk_gb"`
	Sites         Opt[int]     `json:"sites"`
	Bandwidth     Opt[string]  `json:"bandwidth"`
	EmailAccounts Opt[int]     `json:"email_accounts"`
	AsOf          Opt[string]  `json:"as_of"`
	SourceURL     Opt[string]  `json:"source_url"`
	Notes         Opt[string]  `json:"notes"`
	Active        Opt[bool]    `json:"active"`
}

// ItemFilter narrows a price list.
type ItemFilter struct {
	SupplierID string
	Type       string
	Kind       string
	Q          string
	Stale      bool
	Active     string // "" all | "1" active | "0" inactive
}

var kindOrder = func() map[string]int {
	m := map[string]int{}
	for i, k := range Kinds {
		m[k.Key] = i
	}
	return m
}()

// Items lists prices with their supplier and age.
func (s *Service) Items(org string, f ItemFilter, today time.Time) []Item {
	sups := s.Suppliers(org)
	byID := map[string]Supplier{}
	order := map[string]int{}
	for i, sp := range sups {
		byID[sp.ID] = sp
		order[sp.ID] = i
	}
	var rows []Item
	q := s.DB.Where("organization_id = ?", org)
	if f.SupplierID != "" {
		q = q.Where("supplier_id = ?", f.SupplierID)
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	switch f.Active {
	case "1":
		q = q.Where("active")
	case "0":
		q = q.Where("NOT active")
	}
	q.Find(&rows)
	needle := strings.ToLower(strings.TrimSpace(f.Q))
	out := []Item{}
	for _, it := range rows {
		sp, ok := byID[it.SupplierID]
		if !ok {
			continue
		}
		if f.Type != "" && sp.Type != f.Type {
			continue
		}
		it.SupplierName, it.SupplierType = sp.Name, sp.Type
		it.Age(today)
		if f.Stale && !it.Stale {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(it.Name+" "+it.TLD+" "+sp.Name+" "+it.Notes+" "+KindLabel(it.Kind)+" "+it.Kind), needle) {
			continue
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if order[a.SupplierID] != order[b.SupplierID] {
			return order[a.SupplierID] < order[b.SupplierID]
		}
		if kindOrder[a.Kind] != kindOrder[b.Kind] {
			return kindOrder[a.Kind] < kindOrder[b.Kind]
		}
		if a.Kind == KindHosting {
			ma, mb := a.Regular()/float64(max1(PeriodMonths(a.Period))), b.Regular()/float64(max1(PeriodMonths(b.Period)))
			if ma != mb {
				return ma < mb
			}
		}
		if a.TLD != b.TLD {
			return a.TLD < b.TLD
		}
		if a.NameKey != b.NameKey {
			return a.NameKey < b.NameKey
		}
		return PeriodMonths(a.Period) < PeriodMonths(b.Period)
	})
	return out
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func (s *Service) item(org, id string) (*Item, error) {
	var it Item
	if err := s.DB.Where("organization_id = ? AND id = ?", org, id).Take(&it).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &it, nil
}

func samePtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return abs(*a-*b) < 0.00005
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// SaveItem creates (id "") or updates a price. When the price, the regular
// price or the setup fee changes, the old values are kept in the history.
func (s *Service) SaveItem(org, user, id string, in ItemInput, source string, today time.Time) (*Item, error) {
	var out *Item
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		it, err := s.saveItem(tx, org, user, id, in, source, today)
		out = it
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.decorate(org, out, today), nil
}

func (s *Service) decorate(org string, it *Item, today time.Time) *Item {
	if sp, err := s.supplier(org, it.SupplierID); err == nil {
		it.SupplierName, it.SupplierType = sp.Name, sp.Type
	}
	it.Age(today)
	return it
}

func (s *Service) saveItem(tx *gorm.DB, org, user, id string, in ItemInput, source string, today time.Time) (*Item, error) {
	now := s.now()
	var it *Item
	creating := id == ""
	if creating {
		it = &Item{ID: uuid.NewString(), OrganizationID: org, Active: true, Source: source, CreatedAt: now}
		if !in.SupplierID.Set || !in.Kind.Set || !in.Price.Set || in.Price.Null {
			return nil, invalid("a price needs a supplier, a kind and an amount")
		}
	} else {
		var row Item
		if err := tx.Where("organization_id = ? AND id = ?", org, id).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		it = &row
	}
	old := *it
	if in.SupplierID.Set {
		it.SupplierID = strings.TrimSpace(in.SupplierID.V)
	}
	var sp Supplier
	if err := tx.Where("organization_id = ? AND id = ?", org, it.SupplierID).Take(&sp).Error; err != nil {
		return nil, invalid("choose a supplier")
	}
	if in.Kind.Set {
		k, ok := ParseKind(in.Kind.V)
		if !ok {
			return nil, invalid("choose what the price is for (hosting plan, domain registration …)")
		}
		it.Kind = k
	}
	if in.Period.Set {
		p, ok := ParsePeriod(in.Period.V)
		if !ok {
			return nil, invalid("the billing period must be month, year, 2y, 3y or one-off")
		}
		it.Period = p
	}
	if it.Period == "" {
		it.Period = DefaultPeriod(it.Kind)
	}
	if in.TLD.Set {
		it.TLD = NormTLD(in.TLD.V)
		if in.TLD.V != "" && !in.TLD.Null && it.TLD == "" && IsDomainKind(it.Kind) {
			return nil, invalid("%q is not a domain ending like .co.ke", in.TLD.V)
		}
	}
	if in.Name.Set {
		it.Name = reSpaces.ReplaceAllString(strings.TrimSpace(in.Name.V), " ")
	}
	if IsDomainKind(it.Kind) {
		if it.TLD == "" {
			return nil, invalid("enter the domain ending (e.g. .co.ke)")
		}
		if it.Period == PeriodMonth || it.Period == PeriodOneOff {
			return nil, invalid("a domain price is per year (year, 2y or 3y)")
		}
		// A name that only says what the row is follows the kind and the TLD.
		if it.Name == "" || NormTLD(it.Name) == it.TLD || (!creating && it.Name == DomainItemName(old.Kind, old.TLD)) {
			it.Name = DomainItemName(it.Kind, it.TLD)
		}
	} else {
		it.TLD = ""
		if it.Name == "" {
			return nil, invalid("give the price a name (the plan or product)")
		}
	}
	if len(it.Name) > 200 {
		return nil, invalid("the name is too long")
	}
	it.NameKey = NameKey(it.Name)
	if in.Price.Set {
		if in.Price.Null || in.Price.V < 0 {
			return nil, invalid("the price must be zero or more")
		}
		it.Price = in.Price.V
	}
	if in.Currency.Set && !in.Currency.Null && strings.TrimSpace(in.Currency.V) != "" {
		c, ok := ParseCurrency(in.Currency.V)
		if !ok {
			return nil, invalid("choose a supported currency")
		}
		it.Currency = c
	}
	if it.Currency == "" {
		it.Currency = sp.Currency
	}
	if in.SetupFee.Set {
		if in.SetupFee.V < 0 {
			return nil, invalid("the setup fee must be zero or more")
		}
		it.SetupFee = in.SetupFee.V
		if in.SetupFee.Null {
			it.SetupFee = 0
		}
	}
	if in.RegularPrice.Set {
		switch {
		case in.RegularPrice.Null || in.RegularPrice.V == 0:
			it.RegularPrice = nil
		case in.RegularPrice.V < 0:
			return nil, invalid("the regular price must be zero or more")
		default:
			v := in.RegularPrice.V
			it.RegularPrice = &v
		}
	}
	it.Promo = it.IsPromo()
	if in.DiskGB.Set {
		switch {
		case in.DiskGB.Null:
			it.DiskGB = nil
		case in.DiskGB.V < 0 && in.DiskGB.V != Unlimited:
			return nil, invalid("the disk space must be zero or more (or unlimited)")
		default:
			v := in.DiskGB.V
			it.DiskGB = &v
		}
	}
	for _, f := range []struct {
		in    Opt[int]
		dst   **int
		label string
	}{{in.Sites, &it.Sites, "sites"}, {in.EmailAccounts, &it.EmailAccounts, "mailboxes"}} {
		if !f.in.Set {
			continue
		}
		switch {
		case f.in.Null:
			*f.dst = nil
		case f.in.V < 0 && f.in.V != Unlimited:
			return nil, invalid("the number of %s must be zero or more (or unlimited)", f.label)
		default:
			v := f.in.V
			*f.dst = &v
		}
	}
	if in.Bandwidth.Set {
		it.Bandwidth = strings.TrimSpace(in.Bandwidth.V)
		if len(it.Bandwidth) > 60 {
			it.Bandwidth = it.Bandwidth[:60]
		}
	}
	if in.SourceURL.Set {
		u, ok := CleanURL(in.SourceURL.V)
		if !ok {
			return nil, invalid("the source link must be a web address (https://…)")
		}
		it.SourceURL = u
	}
	if in.Notes.Set {
		it.Notes = strings.TrimSpace(in.Notes.V)
	}
	if in.Active.Set && !in.Active.Null {
		it.Active = in.Active.V
	}
	priceChanged := !creating && (abs(old.Price-it.Price) >= 0.00005 || !samePtr(old.RegularPrice, it.RegularPrice) || abs(old.SetupFee-it.SetupFee) >= 0.00005)
	switch {
	case in.AsOf.Set && !in.AsOf.Null && strings.TrimSpace(in.AsOf.V) != "":
		d, ok := ParseDay(in.AsOf.V)
		if !ok || d == nil {
			return nil, invalid("the \"as of\" date is not a date")
		}
		if d.After(Day(today).AddDate(0, 0, 1)) {
			return nil, invalid("the \"as of\" date is in the future")
		}
		it.AsOf = d
	case creating || priceChanged:
		// A new price is as of today unless told otherwise.
		d := Day(today)
		it.AsOf = &d
	}
	var dup int64
	tx.Model(&Item{}).Where("organization_id = ? AND supplier_id = ? AND kind = ? AND name_key = ? AND tld = ? AND period = ? AND id <> ?",
		org, it.SupplierID, it.Kind, it.NameKey, it.TLD, it.Period, it.ID).Count(&dup)
	if dup > 0 {
		return nil, invalid("%s already has this price (same kind, name, domain ending and billing period) — edit that one", sp.Name)
	}
	if priceChanged {
		ch := PriceChange{ID: uuid.NewString(), OrganizationID: org, ItemID: it.ID, Currency: old.Currency, Price: old.Price, RegularPrice: old.RegularPrice, SetupFee: old.SetupFee,
			AsOf: old.AsOf, NewPrice: it.Price, NewRegularPrice: it.RegularPrice, NewSetupFee: it.SetupFee, Source: source, ChangedBy: user, ChangedAt: now}
		if err := tx.Create(&ch).Error; err != nil {
			return nil, err
		}
		if abs(old.Price-it.Price) >= 0.00005 {
			p := old.Price
			it.PrevPrice = &p
		}
		it.PriceChangedAt = &now
		if source != "" {
			it.Source = source
		}
	}
	it.UpdatedAt = now
	if err := tx.Save(it).Error; err != nil {
		return nil, err
	}
	return it, nil
}

// DeleteItem removes a price and its history.
func (s *Service) DeleteItem(org, id string) error {
	it, err := s.item(org, id)
	if err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("organization_id = ? AND item_id = ?", org, it.ID).Delete(&PriceChange{}).Error; err != nil {
			return err
		}
		return tx.Where("organization_id = ? AND id = ?", org, it.ID).Delete(&Item{}).Error
	})
}

// History lists an item's earlier prices, newest first.
func (s *Service) History(org, id string, today time.Time) (*Item, []PriceChange, error) {
	it, err := s.item(org, id)
	if err != nil {
		return nil, nil, err
	}
	rows := []PriceChange{}
	s.DB.Where("organization_id = ? AND item_id = ?", org, id).Order("changed_at DESC").Limit(200).Find(&rows)
	return s.decorate(org, it, today), rows, nil
}

// ---------------------------------------------------------------- CSV import / export

// ImportResult is what an import did (or, for a dry run, would do).
type ImportResult struct {
	DryRun       bool     `json:"dry_run"`
	Rows         []CSVRow `json:"rows"`
	Total        int      `json:"total"`
	Created      int      `json:"created"`
	Updated      int      `json:"updated"`
	Unchanged    int      `json:"unchanged"`
	Errors       int      `json:"errors"`
	Duplicates   int      `json:"duplicates"`
	Warnings     int      `json:"warnings"`
	NewSuppliers []string `json:"new_suppliers"`
	Header       string   `json:"header"`
}

// errDryRun rolls a dry run back.
var errDryRun = errors.New("dry run")

// Import reads a price list CSV and upserts its rows by supplier + kind +
// name + TLD + period. Rows with errors are skipped and reported; a dry run
// does everything inside a transaction that is rolled back, so the preview
// is exactly what the import will do.
func (s *Service) Import(org, user string, data []byte, dryRun bool, today time.Time) (*ImportResult, error) {
	rows, err := ParseCSV(data)
	if err != nil {
		return nil, err
	}
	res := &ImportResult{DryRun: dryRun, Rows: rows, Total: len(rows), NewSuppliers: []string{}, Header: CSVHeader}
	home := s.Home(org)
	txErr := s.DB.Transaction(func(tx *gorm.DB) error {
		sups := map[string]*Supplier{}
		var existing []Supplier
		tx.Where("organization_id = ?", org).Find(&existing)
		for i := range existing {
			sups[existing[i].NameKey] = &existing[i]
		}
		now := s.now()
		for i := range res.Rows {
			r := &res.Rows[i]
			if r.Action == "error" || r.Action == "duplicate" {
				continue
			}
			sp := sups[NameKey(r.Supplier)]
			if sp == nil {
				sp = &Supplier{ID: uuid.NewString(), OrganizationID: org, Name: r.Supplier, NameKey: NameKey(r.Supplier), Type: TypeWholesale, Currency: home, Costs: []CostLine{}, CreatedAt: now, UpdatedAt: now}
				if r.SupplierType != "" {
					sp.Type = r.SupplierType
				}
				if r.SupplierCurrency != "" {
					sp.Currency = r.SupplierCurrency
				}
				if sp.Type == TypeSelfHosted {
					sp.Costs = DefaultCostLines()
				}
				if err := tx.Create(sp).Error; err != nil {
					return err
				}
				sups[sp.NameKey] = sp
				res.NewSuppliers = append(res.NewSuppliers, sp.Name)
				now = now.Add(time.Millisecond)
			} else if r.SupplierType != "" && r.SupplierType != sp.Type {
				r.Warnings = append(r.Warnings, fmt.Sprintf("%s is already a %s supplier — supplier_type %s ignored", sp.Name, strings.ReplaceAll(sp.Type, "_", "-"), r.SupplierType))
			}
			r.Currency = sp.Currency
			if r.SupplierCurrency != "" {
				r.Currency = r.SupplierCurrency
			}
			var cur Item
			found := tx.Where("organization_id = ? AND supplier_id = ? AND kind = ? AND name_key = ? AND tld = ? AND period = ?", org, sp.ID, r.Kind, NameKey(r.Name), r.TLD, r.Period).Take(&cur).Error == nil
			in := ItemInput{SupplierID: Of(sp.ID), Kind: Of(r.Kind), Name: Of(r.Name), TLD: Of(r.TLD), Period: Of(r.Period), Price: Of(r.Price), Currency: Of(r.Currency),
				SetupFee: Of(r.SetupFee), SourceURL: Of(r.SourceURL), Notes: Of(r.Notes), Active: Of(true)}
			if r.RegularPrice != nil {
				in.RegularPrice = Of(*r.RegularPrice)
			} else {
				in.RegularPrice = Null[float64]()
			}
			if r.DiskGB != nil {
				in.DiskGB = Of(*r.DiskGB)
			} else {
				in.DiskGB = Null[float64]()
			}
			if r.Sites != nil {
				in.Sites = Of(*r.Sites)
			} else {
				in.Sites = Null[int]()
			}
			if r.EmailAccounts != nil {
				in.EmailAccounts = Of(*r.EmailAccounts)
			} else {
				in.EmailAccounts = Null[int]()
			}
			asOf := Day(today)
			if r.asOf != nil {
				if r.asOf.After(asOf.AddDate(0, 0, 1)) {
					r.Action = "error"
					r.Errors = append(r.Errors, "as_of "+r.AsOf+" is in the future")
					continue
				}
				asOf = *r.asOf
			}
			in.AsOf = Of(asOf.Format("2006-01-02"))
			id := ""
			if found {
				id = cur.ID
				same := abs(cur.Price-r.Price) < 0.00005 && samePtr(cur.RegularPrice, r.RegularPrice) && abs(cur.SetupFee-r.SetupFee) < 0.00005 && cur.Currency == r.Currency &&
					samePtr(cur.DiskGB, r.DiskGB) && sameInt(cur.Sites, r.Sites) && sameInt(cur.EmailAccounts, r.EmailAccounts) && cur.SourceURL == in.SourceURL.V && cur.Notes == r.Notes &&
					cur.Active && cur.Name == r.Name && (r.asOf == nil || (cur.AsOf != nil && Day(*cur.AsOf).Equal(*r.asOf)))
				if same {
					r.Action = "unchanged"
					res.Unchanged++
					continue
				}
				if abs(cur.Price-r.Price) >= 0.00005 {
					p := cur.Price
					r.OldPrice = &p
				}
				if r.asOf == nil && abs(cur.Price-r.Price) < 0.00005 && samePtr(cur.RegularPrice, r.RegularPrice) && abs(cur.SetupFee-r.SetupFee) < 0.00005 {
					// Details changed, not the price: keep its date.
					in.AsOf = Opt[string]{}
				}
			}
			// A savepoint keeps one bad row from spoiling the rest.
			var saveErr error
			_ = tx.Transaction(func(sub *gorm.DB) error {
				_, saveErr = s.saveItem(sub, org, user, id, in, "csv", today)
				return saveErr
			})
			if saveErr != nil {
				r.Action = "error"
				r.Errors = append(r.Errors, saveErr.Error())
				continue
			}
			if found {
				r.Action = "update"
				res.Updated++
			} else {
				r.Action = "create"
				res.Created++
			}
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if txErr != nil && !errors.Is(txErr, errDryRun) {
		return nil, txErr
	}
	for _, r := range res.Rows {
		switch r.Action {
		case "error":
			res.Errors++
		case "duplicate":
			res.Duplicates++
		}
		if len(r.Warnings) > 0 {
			res.Warnings++
		}
	}
	return res, nil
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Export writes the price list in the template's format (active prices, or
// every price with all).
func (s *Service) Export(org string, f ItemFilter, today time.Time) []byte {
	items := s.Items(org, f, today)
	byID := map[string]Supplier{}
	for _, sp := range s.Suppliers(org) {
		byID[sp.ID] = sp
	}
	return ExportCSV(items, byID)
}

// ---------------------------------------------------------------- rates & data

// Rates quotes every currency against the home currency, then applies the
// what-if overrides.
func (s *Service) Rates(org string, curs []string, overrides map[string]float64) Rates {
	home := s.Home(org)
	r := Rates{Home: home, Per: map[string]float64{}, Info: map[string]RateInfo{}}
	seen := map[string]bool{home: true}
	for _, c := range curs {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		q := s.FX.Quote(org, c, home)
		ri := RateInfo{Currency: c, Rate: q.Rate, DefaultRate: q.Rate, Source: q.Source, Provider: q.Provider, AsOf: q.AsOf, Stale: q.Stale, Warning: q.Warning}
		if !q.OK() {
			ri.Rate, ri.DefaultRate, ri.Source = 0, 0, fxrates.SourceMissing
		}
		if v, ok := overrides[c]; ok && v > 0 && abs(v-ri.DefaultRate) > ri.DefaultRate*1e-9 {
			ri.Rate, ri.Overridden, ri.Source, ri.Provider, ri.Warning, ri.Stale = fxrates.SigRound(v), true, "what-if", "Your what-if rate", "", false
			t := s.now()
			ri.AsOf = &t
		}
		r.Per[c] = ri.Rate
		r.Info[c] = ri
	}
	return r
}

func (s *Service) data(org string, overrides map[string]float64, extra []string, today time.Time) Data {
	sups := s.Suppliers(org)
	items := s.Items(org, ItemFilter{Active: "1"}, today)
	curs := append([]string{}, extra...)
	for _, sp := range sups {
		curs = append(curs, sp.Currency)
		for _, l := range sp.Costs {
			curs = append(curs, l.Currency)
		}
	}
	for _, it := range items {
		curs = append(curs, it.Currency)
	}
	return Data{Home: s.Home(org), Suppliers: sups, Items: items, Rates: s.Rates(org, curs, overrides), Tiers: s.Tiers(org), Today: today}
}

// Compare runs the combo comparison.
func (s *Service) Compare(org string, in CompareInput, today time.Time) *CompareResult {
	s.EnsureSeed(org)
	tiers := s.Tiers(org)
	in = in.Normalize(tiers)
	d := s.data(org, in.Rates, nil, today)
	return Compare(in, d)
}

// RetailRequest prices one option of a comparison.
type RetailRequest struct {
	Compare CompareInput `json:"compare"`
	TLD     string       `json:"tld"`
	Key     string       `json:"key"` // option key ("" = the cheapest)
	Retail  RetailInput  `json:"retail"`
}

// Retail prices one option of a comparison for the customer.
func (s *Service) Retail(org string, req RetailRequest, today time.Time) (*RetailResult, *CompareResult, error) {
	s.EnsureSeed(org)
	home := s.Home(org)
	in := req.Compare.Normalize(s.Tiers(org))
	rin := req.Retail.Normalize(home)
	extra := []string{}
	for _, m := range rin.Markets {
		extra = append(extra, m.Currency)
	}
	d := s.data(org, in.Rates, extra, today)
	cmp := Compare(in, d)
	tld := NormTLD(req.TLD)
	if tld == "" && len(in.TLDs) > 0 {
		tld = in.TLDs[0]
	}
	o := cmp.FindOption(tld, req.Key)
	if o == nil {
		return nil, cmp, invalid("that option is no longer in the comparison — run it again")
	}
	if len(o.Parts()) == 0 {
		return nil, cmp, invalid("%s has no prices for this combo", o.SupplierName)
	}
	combo := cmp.Combo(tld)
	res := Retail(o, combo.Benchmarks, rin, d.Rates)
	res.TLD = tld
	return res, cmp, nil
}

// SelfHosted works out a self-hosted supplier's cost model.
func (s *Service) SelfHosted(org, id string, in CompareInput, today time.Time) (*SelfHostModel, error) {
	sp, err := s.supplier(org, id)
	if err != nil {
		return nil, err
	}
	if sp.Type != TypeSelfHosted {
		return nil, invalid("%s is not a self-hosted supplier", sp.Name)
	}
	in = in.Normalize(s.Tiers(org))
	d := s.data(org, in.Rates, nil, today)
	return SelfHostFull(*sp, d, in), nil
}

// ---------------------------------------------------------------- overview & context

// FeeOption is a payment fee the retail helper can start from.
type FeeOption struct {
	Currency string  `json:"currency"`
	Method   string  `json:"method"`
	Label    string  `json:"label"`
	Percent  float64 `json:"percent"`
	Fixed    float64 `json:"fixed"`
	Source   string  `json:"source"`
}

// Context is what the page needs besides the price lists.
type Context struct {
	Home             string               `json:"home"`
	Country          string               `json:"country"`
	TaxRate          float64              `json:"tax_rate"`
	TaxLabel         string               `json:"tax_label"`
	PricesIncludeTax bool                 `json:"prices_include_tax"`
	Fees             map[string]FeeOption `json:"fees"` // by currency
	SecondCurrency   string               `json:"second_currency"`
	Tiers            []Tier               `json:"tiers"`
	Kinds            []map[string]string  `json:"kinds"`
	Periods          []map[string]any     `json:"periods"`
	StaleDays        int                  `json:"stale_days"`
	CSVHeader        string               `json:"csv_header"`
	CSVDocs          []CSVColumnDoc       `json:"csv_docs"`
	Rates            []RateInfo           `json:"rates"`
	CostLineDefaults []CostLine           `json:"cost_line_defaults"`
	// KnownSites is how many client sites the organisation hosts, as far as
	// the app knows: the active sites of the Synergy analytics ("synergy"),
	// else the sites on the self-hosted server ("self_hosted"). 0 = unknown.
	KnownSites       int    `json:"known_sites"`
	KnownSitesSource string `json:"known_sites_source"`
}

// KnownSites counts the client sites the organisation hosts: the active
// sites of Expenses → Synergy (pkg/expenses, read only), else the current
// sites of a self-hosted supplier.
func (s *Service) KnownSites(org string, today time.Time) (int, string) {
	if hasTable(s.DB, "synergy_transactions") && hasTable(s.DB, "synergy_usage_snapshots") {
		in := expenses.SynInput{Today: today}
		s.DB.Where("organization_id = ?", org).Order("date, seq").Find(&in.Txns)
		s.DB.Where("organization_id = ?", org).Order("snapshot_date, domain").Find(&in.Snaps)
		if len(in.Txns) > 0 || len(in.Snaps) > 0 {
			if hasTable(s.DB, "synergy_sites") {
				s.DB.Where("organization_id = ?", org).Find(&in.Sites)
			}
			if hasTable(s.DB, "synergy_plans") {
				s.DB.Where("organization_id = ?", org).Find(&in.Plans)
			}
			if n := expenses.BuildSynergyReport(in).KPIs.ActiveSites; n > 0 {
				return n, "synergy"
			}
		}
	}
	for _, sp := range s.Suppliers(org) {
		if sp.Type == TypeSelfHosted && sp.CurrentSites > 0 {
			return sp.CurrentSites, "self_hosted"
		}
	}
	return 0, ""
}

func hasTable(db *gorm.DB, name string) bool {
	var ok bool
	if err := db.Raw("SELECT to_regclass(?) IS NOT NULL", name).Row().Scan(&ok); err != nil {
		return false
	}
	return ok
}

// FeeFor is the cheapest way the organisation takes online payments in a
// currency (its Flutterwave fee table, or the published defaults).
func (s *Service) FeeFor(org, cur, country, home string) (FeeOption, bool) {
	if !hasTable(s.DB, "pay_settings") {
		return FeeOption{}, false
	}
	st, err := payments.LoadSettings(s.DB, org)
	if err != nil || st == nil {
		return FeeOption{}, false
	}
	opts := payments.Options(st, payments.Context{Amount: 1000, Currency: cur, OrgCountry: country, OrgCurrency: home})
	for _, o := range opts {
		if !o.Known {
			continue
		}
		label := "Flutterwave · " + o.Label
		if o.Scope == payments.ScopeInternational {
			label += " (international)"
		}
		return FeeOption{Currency: cur, Method: o.Method, Label: label, Percent: o.Percent * (1 + st.FeeTaxPercent/100), Fixed: o.Fixed * (1 + st.FeeTaxPercent/100), Source: "Flutterwave fee table (Settings → Online payments)"}, true
	}
	return FeeOption{}, false
}

// ContextFor gathers the defaults of the retail helper.
func (s *Service) ContextFor(org string) *Context {
	p := orgprefs.Get(s.DB, org)
	c := &Context{Home: p.Currency, Country: p.CountryCode, Tiers: s.Tiers(org), StaleDays: StaleDays, CSVHeader: CSVHeader, CSVDocs: CSVDocs(), Fees: map[string]FeeOption{},
		Kinds: []map[string]string{}, Periods: []map[string]any{}, CostLineDefaults: DefaultCostLines()}
	for _, k := range Kinds {
		c.Kinds = append(c.Kinds, map[string]string{"key": k.Key, "label": k.Label})
	}
	for _, x := range Periods {
		c.Periods = append(c.Periods, map[string]any{"key": x.Key, "label": x.Label, "months": x.Months})
	}
	found := false
	if hasTable(s.DB, "billing_settings") {
		var row struct {
			DefaultTaxRate   float64
			TaxLabel         string
			PricesIncludeTax bool
		}
		res := s.DB.Raw(`SELECT COALESCE(default_tax_rate,0) AS default_tax_rate, COALESCE(tax_label,'') AS tax_label, COALESCE(prices_include_tax,false) AS prices_include_tax
			FROM billing_settings WHERE organization_id = ? LIMIT 1`, org).Scan(&row)
		if res.Error == nil && res.RowsAffected > 0 {
			c.TaxRate, c.TaxLabel, c.PricesIncludeTax, found = row.DefaultTaxRate, row.TaxLabel, row.PricesIncludeTax, true
		}
	}
	if !found {
		// No invoice settings saved yet: the country's standard sales tax.
		c.TaxLabel, c.TaxRate, _ = orgprefs.DefaultTax(p.CountryCode)
	}
	if c.TaxLabel == "" {
		c.TaxLabel = "Tax"
	}
	// The second market: Australian clients (or US dollars when home is AUD).
	c.SecondCurrency = "AUD"
	if c.Home == "AUD" {
		c.SecondCurrency = "USD"
	}
	curs := []string{c.Home, c.SecondCurrency}
	for _, sp := range s.Suppliers(org) {
		curs = append(curs, sp.Currency)
	}
	seen := map[string]bool{}
	for _, cur := range curs {
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if f, ok := s.FeeFor(org, cur, p.CountryCode, p.Currency); ok {
			c.Fees[cur] = f
		}
	}
	c.KnownSites, c.KnownSitesSource = s.KnownSites(org, s.now())
	r := s.Rates(org, curs, nil)
	c.Rates = []RateInfo{}
	for _, cur := range curs {
		if ri, ok := r.Info[cur]; ok {
			c.Rates = append(c.Rates, ri)
			delete(r.Info, cur)
		}
	}
	return c
}

// Overview is the page's first load.
type Overview struct {
	Home       string         `json:"home"`
	Suppliers  []SupplierView `json:"suppliers"`
	Items      []Item         `json:"items"`
	KPIs       map[string]any `json:"kpis"`
	Context    *Context       `json:"context"`
	TLDs       []string       `json:"tlds"`
	Today      string         `json:"today"`
	CanSynergy bool           `json:"synergy_available"`
}

// OverviewFor loads the suppliers, every price and the headline numbers.
func (s *Service) OverviewFor(org string, today time.Time) *Overview {
	s.EnsureSeed(org)
	items := s.Items(org, ItemFilter{}, today)
	sups := s.Suppliers(org)
	views := []SupplierView{}
	idx := map[string]int{}
	for i, sp := range sups {
		views = append(views, SupplierView{Supplier: sp})
		idx[sp.ID] = i
	}
	tldSeen := map[string]int{}
	stale, active := 0, 0
	for _, it := range items {
		v := &views[idx[it.SupplierID]]
		v.Items++
		if !it.Active {
			continue
		}
		active++
		if it.Stale {
			v.Stale++
			stale++
		}
		if it.AsOf != nil {
			d := it.AsOf.Format("2006-01-02")
			if v.LatestAt == "" || d > v.LatestAt {
				v.LatestAt = d
			}
			if v.OldestAt == "" || d < v.OldestAt {
				v.OldestAt = d
			}
		}
		if it.TLD != "" && it.SupplierType != TypeCompetitor {
			tldSeen[it.TLD]++
		}
	}
	tlds := []string{}
	for t := range tldSeen {
		tlds = append(tlds, t)
	}
	sort.Slice(tlds, func(i, j int) bool {
		if tldSeen[tlds[i]] != tldSeen[tlds[j]] {
			return tldSeen[tlds[i]] > tldSeen[tlds[j]]
		}
		return tlds[i] < tlds[j]
	})
	priced, competitors := 0, 0
	for _, v := range views {
		if v.Type == TypeCompetitor {
			competitors++
		} else if v.Items > 0 || (v.Type == TypeSelfHosted && SelfHost(v.Supplier, Rates{Home: s.Home(org)}).MonthlyTotal > 0) {
			priced++
		}
	}
	return &Overview{Home: s.Home(org), Suppliers: views, Items: items, Context: s.ContextFor(org), TLDs: tlds, Today: today.Format("2006-01-02"),
		KPIs:       map[string]any{"suppliers": len(views), "priced_suppliers": priced, "competitors": competitors, "prices": active, "stale": stale, "tlds": len(tlds)},
		CanSynergy: hasTable(s.DB, "synergy_transactions")}
}

// ---------------------------------------------------------------- catalog services

// ServiceInput is one catalog service to save from a retail price.
type ServiceInput struct {
	Name             string  `json:"name"`
	Description      string  `json:"description"`
	Category         string  `json:"category"`
	Price            float64 `json:"price"`
	Currency         string  `json:"currency"`
	PriceIncludesTax bool    `json:"price_includes_tax"`
}

// ServiceResult says what happened to one service.
type ServiceResult struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
	Action   string  `json:"action"` // created | updated
}

// DefaultServiceCategory is where saved hosting packages go in the catalog.
const DefaultServiceCategory = "Hosting & domains"

// SaveServices creates catalog services (the same table Services and
// invoicing use). A service with the same name in the same category is
// updated instead of duplicated.
func (s *Service) SaveServices(org string, list []ServiceInput) ([]ServiceResult, error) {
	if len(list) == 0 {
		return nil, invalid("nothing to save")
	}
	if len(list) > 20 {
		return nil, invalid("too many services at once")
	}
	if !hasTable(s.DB, "services") {
		return nil, errors.New("the services catalog is not available")
	}
	out := []ServiceResult{}
	now := s.now()
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		for _, in := range list {
			name := reSpaces.ReplaceAllString(strings.TrimSpace(in.Name), " ")
			if name == "" {
				return invalid("give the service a name")
			}
			if len(name) > 255 {
				return invalid("the service name is too long")
			}
			cat := strings.TrimSpace(in.Category)
			if cat == "" {
				cat = DefaultServiceCategory
			}
			if len(cat) > 100 {
				cat = cat[:100]
			}
			cur, ok := ParseCurrency(in.Currency)
			if !ok {
				return invalid("%s: choose a supported currency", name)
			}
			if in.Price < 0 || in.Price > 99999999 {
				return invalid("%s: the price must be zero or more", name)
			}
			price := RoundCur(in.Price, cur)
			var row struct{ ID string }
			tx.Raw(`SELECT id FROM services WHERE organization_id = ? AND LOWER(name) = LOWER(?) AND LOWER(category) = LOWER(?) AND deleted_at IS NULL LIMIT 1`, org, name, cat).Scan(&row)
			if row.ID != "" {
				if err := tx.Exec(`UPDATE services SET price = ?, currency = ?, price_includes_tax = ?, description = ?, is_active = true, updated_at = ? WHERE id = ? AND organization_id = ?`,
					price, cur, in.PriceIncludesTax, strings.TrimSpace(in.Description), now, row.ID, org).Error; err != nil {
					return err
				}
				out = append(out, ServiceResult{ID: row.ID, Name: name, Category: cat, Price: price, Currency: cur, Action: "updated"})
				continue
			}
			id := uuid.NewString()
			// Duration 0: a product, not a timed appointment (as for imported catalog items).
			if err := tx.Exec(`INSERT INTO services (id, organization_id, name, description, category, duration, price, currency, price_includes_tax, is_active, requires_vehicle, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, true, false, ?, ?)`, id, org, name, strings.TrimSpace(in.Description), cat, price, cur, in.PriceIncludesTax, now, now).Error; err != nil {
				return err
			}
			out = append(out, ServiceResult{ID: id, Name: name, Category: cat, Price: price, Currency: cur, Action: "created"})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- scenarios

// ScenarioInput saves a named comparison.
type ScenarioInput struct {
	Name     string         `json:"name"`
	Input    map[string]any `json:"input"`
	Snapshot map[string]any `json:"snapshot"`
}

// Scenarios lists the saved comparisons, newest first.
func (s *Service) Scenarios(org string) []Scenario {
	out := []Scenario{}
	s.DB.Where("organization_id = ?", org).Order("updated_at DESC").Limit(200).Find(&out)
	return out
}

// SaveScenario creates (id "") or replaces a saved comparison.
func (s *Service) SaveScenario(org, user, id string, in ScenarioInput) (*Scenario, error) {
	name := reSpaces.ReplaceAllString(strings.TrimSpace(in.Name), " ")
	if name == "" {
		return nil, invalid("give the comparison a name")
	}
	if len(name) > 160 {
		return nil, invalid("the name is too long")
	}
	if b, _ := json.Marshal(in); len(b) > 200000 {
		return nil, invalid("the comparison is too large to save")
	}
	now := s.now()
	sc := &Scenario{ID: uuid.NewString(), OrganizationID: org, CreatedBy: user, CreatedAt: now}
	if id != "" {
		if err := s.DB.Where("organization_id = ? AND id = ?", org, id).Take(sc).Error; err != nil {
			return nil, ErrNotFound
		}
	} else {
		// Saving under an existing name replaces it.
		var cur Scenario
		if s.DB.Where("organization_id = ? AND LOWER(name) = LOWER(?)", org, name).Take(&cur).Error == nil {
			sc = &cur
		}
	}
	sc.Name, sc.UpdatedAt = name, now
	if in.Input != nil {
		sc.Input = in.Input
	}
	if in.Snapshot != nil {
		sc.Snapshot = in.Snapshot
	}
	if sc.Input == nil {
		sc.Input = map[string]any{}
	}
	if sc.Snapshot == nil {
		sc.Snapshot = map[string]any{}
	}
	if err := s.DB.Save(sc).Error; err != nil {
		return nil, err
	}
	return sc, nil
}

// DeleteScenario removes a saved comparison.
func (s *Service) DeleteScenario(org, id string) error {
	res := s.DB.Where("organization_id = ? AND id = ?", org, id).Delete(&Scenario{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
