package plans

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Handler exposes the plans API.
type Handler struct {
	Svc *Service
}

// RegisterPublic mounts /public/plans.
func (h *Handler) RegisterPublic(public *gin.RouterGroup) {
	g := public.Group("/plans")
	g.GET("/catalog", h.PublicCatalog)
	g.POST("/quote", h.QuoteHandler)
}

// RegisterAuth mounts the authenticated and super-admin endpoints.
func (h *Handler) RegisterAuth(auth *gin.RouterGroup) {
	g := auth.Group("/plans")
	g.GET("/me", h.Me)
	g.PUT("/me", h.UpdateMe)
	g.GET("/catalog", h.PublicCatalog)
	g.POST("/quote", h.QuoteHandler)

	sa := auth.Group("/super-admin", requireSuperAdmin)
	sa.GET("/plans/catalog", h.AdminCatalog)
	sa.PUT("/plans/catalog", h.AdminUpdateCatalog)
	sa.GET("/plans/organizations", h.AdminOrganizations)
	sa.GET("/organizations/:id/plan", h.AdminGetOrgPlan)
	sa.PUT("/organizations/:id/plan", h.AdminSetOrgPlan)
}

// ---------------------------------------------------------------- helpers

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func failErr(c *gin.Context, err error) {
	var qe *QuoteError
	if errors.As(err, &qe) {
		fail(c, http.StatusUnprocessableEntity, qe.Code, qe.Msg)
		return
	}
	fail(c, http.StatusInternalServerError, "PLANS_ERROR", "Plans are temporarily unavailable. Please try again.")
}

func ctxString(c *gin.Context, key string) string {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func role(c *gin.Context) string { return ctxString(c, "user_role") }

func requireSuperAdmin(c *gin.Context) {
	if role(c) != "super_admin" {
		fail(c, http.StatusForbidden, "SUPER_ADMIN_REQUIRED", "Super administrator access required")
		c.Abort()
		return
	}
	c.Next()
}

func canManage(r string) bool {
	switch r {
	case "super_admin", "admin", "owner":
		return true
	}
	return false
}

func pickCurrency(q string) string {
	q = strings.ToUpper(strings.TrimSpace(q))
	if currency.IsSupported(q) {
		return q
	}
	return "USD"
}

// ---------------------------------------------------------------- catalogue

type catalogModule struct {
	Module
	Price float64 `json:"price"`
}

type catalogTier struct {
	Tier
	BasePrice float64 `json:"base_price"`
	FromPrice float64 `json:"from_price"` // cheapest possible monthly price
}

type currencyInfo struct {
	Code     string  `json:"code"`
	Rate     float64 `json:"rate"`
	Source   string  `json:"source"`
	Decimals int     `json:"decimals"`
}

func (h *Handler) catalogIn(cur string) (gin.H, error) {
	cat, err := h.Svc.Catalog()
	if err != nil {
		return nil, err
	}
	rate, src, ok := h.Svc.FX.Rate(cur)
	if !ok {
		cur, rate, src = "USD", 1, "fixed"
	}
	dec := currency.Decimals(cur)
	conv := func(v float64) float64 { return NiceRound(v*rate, dec) }
	mods := []catalogModule{}
	cheapest := math.MaxFloat64
	for _, m := range cat.Modules {
		if !m.Active {
			continue
		}
		mods = append(mods, catalogModule{Module: m, Price: conv(m.MonthlyPriceUSD)})
		if !m.Core && m.MonthlyPriceUSD < cheapest {
			cheapest = m.MonthlyPriceUSD
		}
	}
	if cheapest == math.MaxFloat64 {
		cheapest = 0
	}
	tiers := []catalogTier{}
	for _, t := range cat.Tiers {
		from := t.BasePriceUSD
		if !t.IncludesAllModules && !t.IncludesIndustryBundle {
			from += cheapest
		}
		tiers = append(tiers, catalogTier{Tier: t, BasePrice: conv(t.BasePriceUSD), FromPrice: conv(from)})
	}
	inds := []Industry{}
	for _, i := range cat.Industries {
		if i.Active {
			inds = append(inds, i)
		}
	}
	curs := []currencyInfo{}
	for _, code := range currency.Supported {
		r, s, ok := h.Svc.FX.Rate(code)
		if !ok {
			continue
		}
		curs = append(curs, currencyInfo{Code: code, Rate: r, Source: s, Decimals: currency.Decimals(code)})
	}
	return gin.H{
		"currency": cur, "rate": rate, "rate_source": src, "decimals": dec, "base_currency": "USD",
		"modules": mods, "industries": inds, "tiers": tiers, "currencies": curs,
	}, nil
}

// PublicCatalog returns modules, industries and tiers priced in ?currency=.
func (h *Handler) PublicCatalog(c *gin.Context) {
	data, err := h.catalogIn(pickCurrency(c.Query("currency")))
	if err != nil {
		failErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

type quoteRequest struct {
	Tier     string   `json:"tier"`
	Industry string   `json:"industry"`
	Modules  []string `json:"modules"`
	Cycle    string   `json:"cycle"`
	Currency string   `json:"currency"`
}

// QuoteHandler prices a selection and, for comparison, the same selection on
// every tier.
func (h *Handler) QuoteHandler(c *gin.Context) {
	var req quoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Invalid quote request")
		return
	}
	cat, err := h.Svc.Catalog()
	if err != nil {
		failErr(c, err)
		return
	}
	sel := Selection{Tier: req.Tier, Industry: req.Industry, Modules: req.Modules, Cycle: req.Cycle, Currency: req.Currency}
	q, err := Price(cat, sel, h.Svc.FX)
	if err != nil {
		failErr(c, err)
		return
	}
	alts := []gin.H{}
	for _, t := range cat.Tiers {
		s := sel
		s.Tier = t.Key
		if t.MaxModules > 0 && len(Normalize(s).Modules) > t.MaxModules {
			// Suggest the cheapest valid selection: the first N chosen modules.
			s.Modules = Normalize(s).Modules[:t.MaxModules]
		}
		aq, err := Price(cat, s, h.Svc.FX)
		if err != nil {
			alts = append(alts, gin.H{"tier": t.Key, "error": err.Error()})
			continue
		}
		alts = append(alts, gin.H{"tier": t.Key, "quote": aq})
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"quote": q, "alternatives": alts}})
}

// ---------------------------------------------------------------- my plan

func (h *Handler) planView(org string) (gin.H, error) {
	sub, err := h.Svc.Subscription(org)
	if err != nil {
		return nil, err
	}
	cat, err := h.Svc.Catalog()
	if err != nil {
		return nil, err
	}
	access := AccessOf(cat, sub)
	q, qerr := h.Svc.Quote(sub)
	tier, _ := cat.Tier(sub.Tier)
	ind, _ := cat.Industry(sub.Industry)
	users := h.Svc.UserCount(org)
	trial := gin.H{"active": false}
	if sub.Status == StatusTrial && sub.TrialEndsAt != nil {
		left := int(math.Ceil(time.Until(*sub.TrialEndsAt).Hours() / 24))
		if left < 0 {
			left = 0
		}
		trial = gin.H{"active": left > 0, "ends_at": sub.TrialEndsAt, "days_left": left, "expired": left == 0}
	}
	// The organisation's home currency (single source: pkg/orgprefs).
	def := orgprefs.Currency(h.Svc.DB, org)
	var orgName string
	h.Svc.DB.Raw("SELECT name FROM organizations WHERE id = ?", org).Row().Scan(&orgName)
	out := gin.H{
		"organization_id": org, "organization_name": orgName,
		"subscription": sub, "tier": tier, "industry": ind,
		"entitlements": access.ModuleList(), "default_currency": def,
		"limits": gin.H{"max_users": access.MaxUsers, "users": users, "max_modules": tier.MaxModules},
		"trial":  trial, "billing_note": "Billing is handled by DASYIN. Plan changes take effect immediately.",
	}
	if qerr == nil {
		out["quote"] = q
	} else {
		out["quote_error"] = qerr.Error()
	}
	return out, nil
}

// Me returns the caller's subscription, entitlements, limits and price.
func (h *Handler) Me(c *gin.Context) {
	org := ctxString(c, "org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organisation not found in token")
		return
	}
	v, err := h.planView(org)
	if err != nil {
		failErr(c, err)
		return
	}
	v["can_manage"] = canManage(role(c))
	c.JSON(http.StatusOK, gin.H{"success": true, "data": v})
}

type updateRequest struct {
	Tier            *string  `json:"tier"`
	Industry        *string  `json:"industry"`
	Modules         []string `json:"modules"`
	Cycle           *string  `json:"cycle"`
	Currency        *string  `json:"currency"`
	DefaultCurrency *string  `json:"default_currency"`
	// Super admin only
	Status         *string    `json:"status"`
	CustomPriceUSD *float64   `json:"custom_price_usd"`
	ClearCustom    bool       `json:"clear_custom_price"`
	MaxUsers       *int       `json:"max_users_override"`
	ClearMaxUsers  bool       `json:"clear_max_users"`
	TrialEndsAt    *time.Time `json:"trial_ends_at"`
	Notes          *string    `json:"notes"`
}

func (h *Handler) apply(c *gin.Context, org string, admin bool) {
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Invalid plan change")
		return
	}
	sub, err := h.Svc.Subscription(org)
	if err != nil {
		failErr(c, err)
		return
	}
	cat, err := h.Svc.Catalog()
	if err != nil {
		failErr(c, err)
		return
	}
	next := *sub
	planChanged := false
	if req.Tier != nil {
		next.Tier = strings.ToLower(strings.TrimSpace(*req.Tier))
		planChanged = planChanged || next.Tier != sub.Tier
	}
	if req.Industry != nil {
		next.Industry = strings.ToLower(strings.TrimSpace(*req.Industry))
	}
	if req.Modules != nil {
		next.Modules = Normalize(Selection{Modules: req.Modules}).Modules
	}
	if req.Cycle != nil {
		next.Cycle = Normalize(Selection{Cycle: *req.Cycle}).Cycle
	}
	if req.Currency != nil {
		next.Currency = strings.ToUpper(strings.TrimSpace(*req.Currency))
	}
	if req.DefaultCurrency != nil {
		d := strings.ToUpper(strings.TrimSpace(*req.DefaultCurrency))
		if !currency.IsSupported(d) {
			fail(c, http.StatusUnprocessableEntity, "UNSUPPORTED_CURRENCY", "Currency "+d+" is not supported")
			return
		}
		next.DefaultCurrency = d
	}
	// Large includes everything; don't keep a stale selection around.
	if t, ok := cat.Tier(next.Tier); ok && t.IncludesAllModules {
		next.Modules = []string{}
	}
	if admin {
		if req.Status != nil {
			if !ValidStatus(*req.Status) {
				fail(c, http.StatusUnprocessableEntity, "INVALID_STATUS", "Status must be trial, active, past_due or cancelled")
				return
			}
			next.Status = *req.Status
		}
		if req.ClearCustom {
			next.CustomPriceUSD = nil
		} else if req.CustomPriceUSD != nil {
			if *req.CustomPriceUSD < 0 {
				fail(c, http.StatusUnprocessableEntity, "INVALID_PRICE", "Custom price cannot be negative")
				return
			}
			v := *req.CustomPriceUSD
			next.CustomPriceUSD = &v
		}
		if req.ClearMaxUsers {
			next.MaxUsers = nil
		} else if req.MaxUsers != nil {
			if *req.MaxUsers < 0 {
				fail(c, http.StatusUnprocessableEntity, "INVALID_LIMIT", "Max users cannot be negative")
				return
			}
			v := *req.MaxUsers
			next.MaxUsers = &v
		}
		if req.TrialEndsAt != nil {
			t := req.TrialEndsAt.UTC()
			next.TrialEndsAt = &t
		}
		if req.Notes != nil {
			next.Notes = *req.Notes
		}
		if next.Status == StatusTrial && next.TrialEndsAt == nil {
			days := 14
			if t, ok := cat.Tier(next.Tier); ok && t.TrialDays > 0 {
				days = t.TrialDays
			}
			end := time.Now().UTC().AddDate(0, 0, days)
			next.TrialEndsAt = &end
		}
	} else {
		if sub.Status == StatusCancelled {
			next.Status = StatusActive
		}
		// A customer-driven change ends any custom enterprise agreement on a different tier.
		if planChanged {
			next.CustomPriceUSD = nil
		}
	}
	if planChanged || req.Modules != nil || req.Industry != nil {
		next.Grandfathered = false
	}
	// User limit: can't move to a plan smaller than the current team.
	if t, ok := cat.Tier(next.Tier); ok {
		limit := t.MaxUsers
		if next.MaxUsers != nil {
			limit = *next.MaxUsers
		}
		if n := h.Svc.UserCount(org); limit > 0 && n > int64(limit) && !admin {
			fail(c, http.StatusUnprocessableEntity, "USER_LIMIT", fmt.Sprintf("The %s plan allows up to %d users and your team has %d. Remove users or choose a bigger plan.", t.Name, limit, n))
			return
		}
	}
	if _, err := h.Svc.Save(&next, ctxString(c, "user_id")); err != nil {
		failErr(c, err)
		return
	}
	if t, ok := cat.Tier(next.Tier); ok {
		h.notifyPlan(c, org, t.Name, planChanged, admin)
	}
	if req.DefaultCurrency != nil {
		h.Svc.SyncDefaultCurrency(org, next.DefaultCurrency)
	}
	v, err := h.planView(org)
	if err != nil {
		failErr(c, err)
		return
	}
	v["can_manage"] = canManage(role(c))
	c.JSON(http.StatusOK, gin.H{"success": true, "data": v, "message": "Plan updated"})
}

// UpdateMe lets an organisation admin change tier, industry, modules, cycle,
// currency and default currency.
func (h *Handler) UpdateMe(c *gin.Context) {
	if !canManage(role(c)) {
		fail(c, http.StatusForbidden, "INSUFFICIENT_PERMISSIONS", "Only organisation admins can change the plan")
		return
	}
	org := ctxString(c, "org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organisation not found in token")
		return
	}
	h.apply(c, org, false)
}

// ---------------------------------------------------------------- super admin

// AdminCatalog returns the raw catalogue (USD) and FX table.
func (h *Handler) AdminCatalog(c *gin.Context) {
	h.Svc.Invalidate("")
	cat, err := h.Svc.Catalog()
	if err != nil {
		failErr(c, err)
		return
	}
	fx, asOf := h.Svc.FX.Table()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"modules": cat.Modules, "industries": cat.Industries, "tiers": cat.Tiers,
		"fx_rates": fx, "live_fx_as_of": asOf, "base_currency": "USD",
	}})
}

type catalogUpdate struct {
	Modules    []Module   `json:"modules"`
	Industries []Industry `json:"industries"`
	Tiers      []Tier     `json:"tiers"`
	FXRates    []FXRate   `json:"fx_rates"`
}

var slugRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,39}$`)

// AdminUpdateCatalog upserts any of modules, industries, tiers and fallback FX rates.
func (h *Handler) AdminUpdateCatalog(c *gin.Context) {
	var req catalogUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "INVALID_REQUEST", "Invalid catalogue update")
		return
	}
	cat, err := h.Svc.Catalog()
	if err != nil {
		failErr(c, err)
		return
	}
	known := map[string]bool{}
	for _, m := range cat.Modules {
		known[m.Key] = true
	}
	bad := func(msg string) { fail(c, http.StatusUnprocessableEntity, "INVALID_CATALOG", msg) }
	for _, m := range req.Modules {
		if !known[m.Key] {
			bad("Unknown module " + m.Key + " (modules map to app features and cannot be added here)")
			return
		}
		if strings.TrimSpace(m.Name) == "" || m.MonthlyPriceUSD < 0 {
			bad("Module " + m.Key + " needs a name and a price of 0 or more")
			return
		}
	}
	for _, t := range req.Tiers {
		if _, ok := cat.Tier(t.Key); !ok {
			bad("Unknown tier " + t.Key)
			return
		}
		if strings.TrimSpace(t.Name) == "" || t.BasePriceUSD < 0 || t.MaxModules < 0 || t.MaxUsers < 0 || t.TrialDays < 0 {
			bad("Tier " + t.Key + " has invalid values (prices and limits must be 0 or more)")
			return
		}
		if t.AddonDiscountPct < 0 || t.AddonDiscountPct > 100 || t.AnnualDiscountPct < 0 || t.AnnualDiscountPct > 100 {
			bad("Discounts for " + t.Key + " must be between 0 and 100%")
			return
		}
	}
	for i := range req.Industries {
		ind := &req.Industries[i]
		ind.Key = strings.ToLower(strings.TrimSpace(ind.Key))
		if !slugRe.MatchString(ind.Key) || strings.TrimSpace(ind.Name) == "" {
			bad("Industries need a key (lowercase letters, digits, - or _) and a name")
			return
		}
		mods := []string{}
		for _, m := range Normalize(Selection{Modules: ind.Modules}).Modules {
			if !known[m] {
				bad("Industry " + ind.Key + " references unknown module " + m)
				return
			}
			mods = append(mods, m)
		}
		ind.Modules = mods
		if ind.RecommendedTier == "" {
			ind.RecommendedTier = TierSME
		}
		if _, ok := cat.Tier(ind.RecommendedTier); !ok {
			bad("Industry " + ind.Key + " recommends unknown tier " + ind.RecommendedTier)
			return
		}
	}
	for i := range req.FXRates {
		r := &req.FXRates[i]
		r.Currency = strings.ToUpper(strings.TrimSpace(r.Currency))
		if !currency.IsSupported(r.Currency) || r.RatePerUSD <= 0 {
			bad("FX rate for " + r.Currency + " must be a supported currency with a rate above 0")
			return
		}
		if r.Currency == "USD" {
			r.RatePerUSD = 1
		}
	}
	err = h.Svc.DB.Transaction(func(tx *gorm.DB) error {
		for _, m := range req.Modules {
			if err := tx.Model(&Module{}).Where("key = ?", m.Key).Updates(map[string]interface{}{
				"name": m.Name, "description": m.Description, "monthly_price_usd": m.MonthlyPriceUSD,
				"active": m.Active || m.Core, "sort_order": m.SortOrder, "updated_at": time.Now(),
			}).Error; err != nil {
				return err
			}
		}
		for _, t := range req.Tiers {
			features, _ := json.Marshal(nonNil(t.Features))
			if err := tx.Model(&Tier{}).Where("key = ?", t.Key).Updates(map[string]interface{}{
				"name": t.Name, "tagline": t.Tagline, "base_price_usd": t.BasePriceUSD, "max_modules": t.MaxModules,
				"max_users": t.MaxUsers, "addon_discount_pct": t.AddonDiscountPct, "annual_discount_pct": t.AnnualDiscountPct,
				"includes_industry_bundle": t.IncludesIndustryBundle, "includes_all_modules": t.IncludesAllModules,
				"enterprise": t.Enterprise, "trial_days": t.TrialDays, "features": string(features), "updated_at": time.Now(),
			}).Error; err != nil {
				return err
			}
		}
		for _, ind := range req.Industries {
			ind := ind
			ind.UpdatedAt = time.Now()
			if err := tx.Save(&ind).Error; err != nil {
				return err
			}
		}
		for _, r := range req.FXRates {
			r := r
			r.UpdatedAt = time.Now()
			if err := tx.Save(&r).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "SAVE_FAILED", "Could not save the catalogue: "+err.Error())
		return
	}
	h.Svc.Invalidate("")
	h.AdminCatalog(c)
}

// AdminOrganizations lists organisations with their plan summary.
func (h *Handler) AdminOrganizations(c *gin.Context) {
	type orgRow struct {
		ID           string    `json:"id"`
		Name         string    `json:"name"`
		BusinessType string    `json:"business_type"`
		CreatedAt    time.Time `json:"created_at"`
	}
	var orgs []orgRow
	if err := h.Svc.DB.Raw("SELECT id, name, business_type, created_at FROM organizations WHERE deleted_at IS NULL ORDER BY name").Scan(&orgs).Error; err != nil {
		failErr(c, err)
		return
	}
	var subs []Subscription
	h.Svc.DB.Find(&subs)
	byOrg := map[string]Subscription{}
	for _, s := range subs {
		byOrg[s.OrganizationID] = s
	}
	out := []gin.H{}
	for _, o := range orgs {
		row := gin.H{"id": o.ID, "name": o.Name, "business_type": o.BusinessType, "created_at": o.CreatedAt}
		if s, ok := byOrg[o.ID]; ok {
			row["plan"] = gin.H{"tier": s.Tier, "industry": s.Industry, "status": s.Status, "currency": s.Currency,
				"cycle": s.Cycle, "price": s.Price, "price_usd": s.PriceUSD, "modules": s.Modules, "grandfathered": s.Grandfathered}
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
}

func (h *Handler) orgExists(id string) bool {
	var n int64
	h.Svc.DB.Table("organizations").Where("id = ? AND deleted_at IS NULL", id).Count(&n)
	return n > 0
}

// AdminGetOrgPlan returns any organisation's plan.
func (h *Handler) AdminGetOrgPlan(c *gin.Context) {
	id := c.Param("id")
	if !h.orgExists(id) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Organisation not found")
		return
	}
	v, err := h.planView(id)
	if err != nil {
		failErr(c, err)
		return
	}
	v["can_manage"] = true
	c.JSON(http.StatusOK, gin.H{"success": true, "data": v})
}

// AdminSetOrgPlan overrides any organisation's plan (incl. status, custom
// price, user limit and trial end).
func (h *Handler) AdminSetOrgPlan(c *gin.Context) {
	id := c.Param("id")
	if !h.orgExists(id) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Organisation not found")
		return
	}
	h.apply(c, id, true)
}

func nonNil(s []string) []string {
	out := []string{}
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
