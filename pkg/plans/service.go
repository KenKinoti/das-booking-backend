package plans

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Service owns the catalogue, subscriptions and entitlement cache.
type Service struct {
	DB  *gorm.DB
	FX  *FX
	Now func() time.Time
	// EntitlementTTL is how long an organisation's entitlements are cached.
	EntitlementTTL time.Duration

	mu      sync.Mutex
	catalog *Catalog
	catAt   time.Time
	ents    map[string]entEntry
}

type entEntry struct {
	e  Access
	at time.Time
}

// Access is what the middleware needs to know about an organisation.
type Access struct {
	Modules  map[string]bool
	MaxUsers int
	Status   string
}

// Has reports whether the organisation may use module k.
func (a Access) Has(k string) bool { return k == "" || k == CoreModule || a.Modules[k] }

// NewService builds a service.
func NewService(db *gorm.DB, fx *FX) *Service {
	if fx == nil {
		fx = NewFX()
	}
	return &Service{DB: db, FX: fx, Now: time.Now, EntitlementTTL: 30 * time.Second, ents: map[string]entEntry{}}
}

// Migrate creates the tables and seeds defaults once.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(&Module{}, &Industry{}, &Tier{}, &FXRate{}, &Subscription{}, &Meta{}); err != nil {
		return err
	}
	return Seed(db)
}

// Seed inserts default rows that do not exist yet (never overwrites edits).
func Seed(db *gorm.DB) error {
	ins := db.Session(&gorm.Session{}).Clauses(clause.OnConflict{DoNothing: true}).Session(&gorm.Session{})
	for _, m := range DefaultModules() {
		m := m
		if err := ins.Create(&m).Error; err != nil {
			return err
		}
	}
	for _, i := range DefaultIndustries() {
		i := i
		if err := ins.Create(&i).Error; err != nil {
			return err
		}
	}
	for _, t := range DefaultTiers() {
		t := t
		if err := ins.Create(&t).Error; err != nil {
			return err
		}
	}
	for _, r := range DefaultFXRates() {
		r := r
		if err := ins.Create(&r).Error; err != nil {
			return err
		}
	}
	meta := Meta{Key: "installed_at", Value: time.Now().UTC().Format(time.RFC3339)}
	return ins.Create(&meta).Error
}

// Catalog returns the (briefly cached) catalogue.
func (s *Service) Catalog() (*Catalog, error) {
	s.mu.Lock()
	if s.catalog != nil && s.Now().Sub(s.catAt) < time.Minute {
		c := s.catalog
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()
	cat := &Catalog{}
	if err := s.DB.Order("sort_order, key").Find(&cat.Modules).Error; err != nil {
		return nil, err
	}
	if err := s.DB.Order("sort_order, key").Find(&cat.Industries).Error; err != nil {
		return nil, err
	}
	if err := s.DB.Order("sort_order, key").Find(&cat.Tiers).Error; err != nil {
		return nil, err
	}
	if len(cat.Tiers) == 0 || len(cat.Modules) == 0 {
		return nil, errors.New("plan catalogue is empty")
	}
	var rates []FXRate
	if err := s.DB.Find(&rates).Error; err == nil {
		s.FX.SetFallback(rates)
	}
	s.mu.Lock()
	s.catalog, s.catAt = cat, s.Now()
	s.mu.Unlock()
	return cat, nil
}

// Invalidate drops cached catalogue and entitlements (after edits).
func (s *Service) Invalidate(org string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if org == "" {
		s.catalog = nil
		s.ents = map[string]entEntry{}
		return
	}
	delete(s.ents, org)
}

// Subscription returns the organisation's subscription, provisioning one if
// needed: organisations that existed before plans were installed keep full
// access (Large, all modules, grandfathered); newer ones start a trial.
func (s *Service) Subscription(org string) (*Subscription, error) {
	if org == "" {
		return nil, errors.New("organisation required")
	}
	var sub Subscription
	err := s.DB.Where("organization_id = ?", org).First(&sub).Error
	if err == nil {
		return &sub, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	sub = s.defaultSubscription(org)
	if err := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&sub).Error; err != nil {
		return nil, err
	}
	// Re-read in case another request won the race.
	if err := s.DB.Where("organization_id = ?", org).First(&sub).Error; err != nil {
		return nil, err
	}
	return &sub, nil
}

func (s *Service) defaultSubscription(org string) Subscription {
	now := s.Now().UTC()
	var row struct {
		BusinessType string
		CreatedAt    time.Time
	}
	s.DB.Raw("SELECT business_type, created_at FROM organizations WHERE id = ?", org).Scan(&row)
	industry := industryAliases[strings.ToLower(strings.TrimSpace(row.BusinessType))]
	if industry == "" {
		industry = "general"
	}
	cur := s.orgCurrency(org)
	sub := Subscription{
		ID: uuid.NewString(), OrganizationID: org, Industry: industry, Cycle: CycleMonthly,
		Currency: cur, DefaultCurrency: cur, Modules: []string{},
	}
	var installed Meta
	s.DB.Where("key = ?", "installed_at").First(&installed)
	installedAt, _ := time.Parse(time.RFC3339, installed.Value)
	legacy := row.CreatedAt.IsZero() || installedAt.IsZero() || row.CreatedAt.Before(installedAt)
	if !legacy {
		// The platform operator's own organisation always keeps everything.
		var admins int64
		s.DB.Table("users").Where("organization_id = ? AND role = ? AND deleted_at IS NULL", org, "super_admin").Count(&admins)
		legacy = admins > 0
	}
	if legacy {
		sub.Tier, sub.Status, sub.Grandfathered = TierLarge, StatusActive, true
		sub.Notes = "Existing organisation — full access kept when plans were introduced."
	} else {
		sub.Tier, sub.Status = TierSME, StatusTrial
		days := 14
		if cat, err := s.Catalog(); err == nil {
			if ind, ok := cat.Industry(industry); ok && ind.RecommendedTier != "" {
				if _, ok := cat.Tier(ind.RecommendedTier); ok {
					sub.Tier = ind.RecommendedTier
				}
			}
			if t, ok := cat.Tier(sub.Tier); ok && t.TrialDays > 0 {
				days = t.TrialDays
			}
			if sub.Tier == TierSmall {
				if ind, ok := cat.Industry(industry); ok {
					max := 2
					if t, ok := cat.Tier(TierSmall); ok && t.MaxModules > 0 {
						max = t.MaxModules
					}
					for _, m := range ind.Modules {
						if len(sub.Modules) < max && cat.sellable(m) {
							sub.Modules = append(sub.Modules, m)
						}
					}
				}
			}
		}
		end := now.AddDate(0, 0, days)
		sub.TrialEndsAt = &end
	}
	if q, err := s.quoteFor(&sub); err == nil {
		sub.PriceUSD, sub.Price = q.MonthlyUSD, q.Total
	}
	return sub
}

// orgCurrency guesses the organisation's currency from existing settings.
func (s *Service) orgCurrency(org string) string {
	for _, q := range []string{
		"SELECT currency FROM organization_settings WHERE organization_id = ? LIMIT 1",
		"SELECT currency FROM billing_settings WHERE organization_id = ? LIMIT 1",
	} {
		var cur string
		if err := s.DB.Raw(q, org).Row().Scan(&cur); err == nil {
			cur = strings.ToUpper(strings.TrimSpace(cur))
			if currency.IsSupported(cur) {
				return cur
			}
		}
	}
	return "AUD"
}

func (s *Service) quoteFor(sub *Subscription) (*Quote, error) {
	cat, err := s.Catalog()
	if err != nil {
		return nil, err
	}
	return Price(cat, Selection{Tier: sub.Tier, Industry: sub.Industry, Modules: sub.Modules, Cycle: sub.Cycle, Currency: sub.Currency, CustomPriceUSD: sub.CustomPriceUSD}, s.FX)
}

// Quote prices a subscription as stored.
func (s *Service) Quote(sub *Subscription) (*Quote, error) { return s.quoteFor(sub) }

// AccessFor returns (cached) entitlements for an organisation.
func (s *Service) AccessFor(org string) (Access, error) {
	s.mu.Lock()
	if e, ok := s.ents[org]; ok && s.Now().Sub(e.at) < s.EntitlementTTL {
		s.mu.Unlock()
		return e.e, nil
	}
	s.mu.Unlock()
	sub, err := s.Subscription(org)
	if err != nil {
		return Access{}, err
	}
	cat, err := s.Catalog()
	if err != nil {
		return Access{}, err
	}
	a := AccessOf(cat, sub)
	s.mu.Lock()
	s.ents[org] = entEntry{e: a, at: s.Now()}
	s.mu.Unlock()
	return a, nil
}

// AccessOf computes entitlements for a subscription. Invalid selections
// degrade gracefully (only valid modules count) rather than locking out.
func AccessOf(cat *Catalog, sub *Subscription) Access {
	a := Access{Modules: map[string]bool{CoreModule: true}, Status: sub.Status}
	tier, ok := cat.Tier(sub.Tier)
	if !ok {
		return a
	}
	a.MaxUsers = tier.MaxUsers
	if sub.MaxUsers != nil {
		a.MaxUsers = *sub.MaxUsers
	}
	if sub.Status == StatusCancelled {
		return a
	}
	_, included, addons, err := Resolve(cat, Selection{Tier: sub.Tier, Industry: sub.Industry, Modules: sub.Modules})
	if err != nil {
		// e.g. a Small plan whose modules exceed a lowered limit: keep the first N valid ones.
		included, addons = []string{}, []string{}
		if tier.IncludesAllModules {
			included = cat.AllSellable()
		} else {
			if tier.IncludesIndustryBundle {
				if ind, ok := cat.Industry(sub.Industry); ok {
					included = ind.Modules
				}
			}
			for _, m := range Normalize(Selection{Modules: sub.Modules}).Modules {
				if cat.sellable(m) && (tier.MaxModules <= 0 || len(included)+len(addons) < tier.MaxModules) {
					addons = append(addons, m)
				}
			}
		}
	}
	for _, m := range Entitlements(included, addons) {
		a.Modules[m] = true
	}
	return a
}

// ModuleList returns the sorted entitlement keys of an Access.
func (a Access) ModuleList() []string {
	out := []string{}
	for k, v := range a.Modules {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// UserCount counts active users in an organisation.
func (s *Service) UserCount(org string) int64 {
	var n int64
	s.DB.Table("users").Where("organization_id = ? AND deleted_at IS NULL", org).Count(&n)
	return n
}

// Save validates and stores a subscription change.
func (s *Service) Save(sub *Subscription, by string) (*Quote, error) {
	q, err := s.quoteFor(sub)
	if err != nil {
		return nil, err
	}
	sub.PriceUSD = q.MonthlyUSD
	sub.Price = q.Total
	sub.UpdatedBy = by
	if sub.ID == "" {
		sub.ID = uuid.NewString()
	}
	if err := s.DB.Save(sub).Error; err != nil {
		return nil, fmt.Errorf("save subscription: %w", err)
	}
	s.Invalidate(sub.OrganizationID)
	return q, nil
}

// SyncDefaultCurrency mirrors the default currency into organisation settings
// (best effort) so modules that read settings pick it up too.
func (s *Service) SyncDefaultCurrency(org, cur string) {
	if cur == "" {
		return
	}
	if err := s.DB.Exec("UPDATE organization_settings SET currency = ? WHERE organization_id = ?", cur, org).Error; err != nil {
		log.Printf("plans: sync default currency: %v", err)
	}
}
