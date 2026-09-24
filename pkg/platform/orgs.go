package platform

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/plans"
)

// OrgRow is one organisation in the list.
type OrgRow struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Email           string          `json:"email"`
	Phone           string          `json:"phone"`
	Website         string          `json:"website"`
	ABN             *string         `json:"abn"`
	BusinessType    string          `json:"business_type"`
	Description     string          `json:"description"`
	AddressStreet   string          `json:"address_street"`
	AddressSuburb   string          `json:"address_suburb"`
	AddressState    string          `json:"address_state"`
	AddressPostcode string          `json:"address_postcode"`
	AddressCountry  string          `json:"address_country"`
	EnabledModules  json.RawMessage `json:"enabled_modules" gorm:"-"`
	RawModules      *string         `json:"-" gorm:"column:enabled_modules"`
	Status          string          `json:"status"`
	StatusReason    string          `json:"status_reason"`
	Users           int64           `json:"users"`
	ActiveUsers     int64           `json:"active_users"`
	Invoices        int64           `json:"invoices"`
	LastActive      *time.Time      `json:"last_active"`
	PlanName        *string         `json:"plan_name"`
	AdminEmail      *string         `json:"admin_email"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

func (h *Handler) orgSelect() string {
	inv := "0"
	if hasTable(h.DB, "billing_invoices") {
		inv = "(SELECT COUNT(*) FROM billing_invoices i WHERE i.organization_id = o.id AND i.deleted_at IS NULL AND i.doc_type = 'invoice')"
	}
	plan, planStatus := "NULL", "NULL"
	if hasTable(h.DB, "plan_subscriptions") {
		plan = "(SELECT ps.tier FROM plan_subscriptions ps WHERE ps.organization_id = o.id LIMIT 1)"
		planStatus = "(SELECT ps.status FROM plan_subscriptions ps WHERE ps.organization_id = o.id LIMIT 1)"
	}
	return `o.id, o.name, o.email, o.phone, o.website, o.abn, o.business_type, o.description,
		o.address_street, o.address_suburb, o.address_state, o.address_postcode, o.address_country,
		o.enabled_modules AS raw_modules, o.created_at, o.updated_at,
		COALESCE(s.status, 'active') AS status, COALESCE(s.reason, '') AS status_reason,
		(SELECT COUNT(*) FROM users u WHERE u.organization_id = o.id AND u.deleted_at IS NULL) AS users,
		(SELECT COUNT(*) FROM users u WHERE u.organization_id = o.id AND u.deleted_at IS NULL AND u.is_active) AS active_users,
		(SELECT MAX(u.last_login_at) FROM users u WHERE u.organization_id = o.id AND u.deleted_at IS NULL) AS last_active,
		(SELECT u.email FROM users u WHERE u.organization_id = o.id AND u.deleted_at IS NULL AND u.role = 'admin' ORDER BY u.created_at LIMIT 1) AS admin_email,
		` + inv + ` AS invoices, ` + plan + ` AS plan_name, ` + planStatus + ` AS plan_status`
}

func fixModules(rows []OrgRow) {
	for i := range rows {
		rows[i].EnabledModules = json.RawMessage("[]")
		if rows[i].RawModules != nil {
			s := strings.TrimSpace(*rows[i].RawModules)
			if strings.HasPrefix(s, "[") && json.Valid([]byte(s)) {
				rows[i].EnabledModules = json.RawMessage(s)
			}
		}
	}
}

func (h *Handler) orgQuery() *gorm.DB {
	return h.DB.Table("organizations o").
		Joins("LEFT JOIN platform_org_status s ON s.organization_id = o.id").
		Where("o.deleted_at IS NULL")
}

// ListOrganizations lists organisations with usage columns.
func (h *Handler) ListOrganizations(c *gin.Context) {
	page, limit, offset := pageParams(c, 25, 200)
	q := h.orgQuery()
	if s := strings.TrimSpace(c.Query("search")); s != "" {
		p := like(s)
		q = q.Where("LOWER(o.name) LIKE ? OR LOWER(o.email) LIKE ? OR LOWER(COALESCE(o.abn,'')) LIKE ? OR o.id = ?", p, p, p, strings.TrimSpace(s))
	}
	switch c.Query("status") {
	case "suspended":
		q = q.Where("s.status = ?", "suspended")
	case "active":
		q = q.Where("COALESCE(s.status,'active') = ?", "active")
	}
	if bt := c.Query("business_type"); bt != "" {
		q = q.Where("o.business_type = ?", bt)
	}
	var total int64
	if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load organisations")
		return
	}
	order := "o.created_at DESC"
	switch c.Query("sort") {
	case "name":
		order = "LOWER(o.name) ASC"
	case "oldest":
		order = "o.created_at ASC"
	case "users":
		order = "users DESC, o.created_at DESC"
	case "last_active":
		order = "last_active DESC NULLS LAST"
	}
	rows := []OrgRow{}
	if err := q.Select(h.orgSelect()).Order(order).Limit(limit).Offset(offset).Scan(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load organisations")
		return
	}
	fixModules(rows)

	var types []string
	h.DB.Table("organizations").Where("deleted_at IS NULL").Distinct("business_type").Order("business_type").Pluck("business_type", &types)
	ok(c, http.StatusOK, gin.H{"organizations": rows, "total": total, "page": page, "limit": limit, "business_types": types})
}

func (h *Handler) loadOrg(id string) (*OrgRow, error) {
	rows := []OrgRow{}
	if err := h.orgQuery().Select(h.orgSelect()).Where("o.id = ?", id).Limit(1).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	fixModules(rows)
	return &rows[0], nil
}

// usageTables lists per-organisation record counts shown in the org drawer.
var usageTables = []struct{ Key, Label, Table, Where string }{
	{"customers", "Customers", "customers", ""},
	{"invoices", "Invoices", "billing_invoices", "doc_type = 'invoice'"},
	{"quotes", "Quotes", "billing_invoices", "doc_type = 'quote'"},
	{"bookings", "Bookings", "bookings", ""},
	{"products", "Products", "products", ""},
	{"services", "Services", "services", ""},
	{"events", "Events", "events", ""},
	{"pos_transactions", "POS sales", "pos_transactions", ""},
	{"suppliers", "Suppliers", "suppliers", ""},
	{"bills", "Bills", "bills", ""},
}

// GetOrganization returns one organisation with users and usage.
func (h *Handler) GetOrganization(c *gin.Context) {
	org, err := h.loadOrg(c.Param("id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Organisation not found")
		return
	} else if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load organisation")
		return
	}
	users := []userRow{}
	h.userQuery().Where("u.organization_id = ?", org.ID).Select(userSelect).Order("u.created_at ASC").Limit(200).Scan(&users)

	usage := []gin.H{}
	for _, t := range usageTables {
		if !hasTable(h.DB, t.Table) {
			continue
		}
		q := h.DB.Table(t.Table).Where("organization_id = ?", org.ID)
		if h.DB.Migrator().HasColumn(t.Table, "deleted_at") {
			q = q.Where("deleted_at IS NULL")
		}
		if t.Where != "" {
			q = q.Where(t.Where)
		}
		var n int64
		if q.Count(&n).Error == nil {
			usage = append(usage, gin.H{"key": t.Key, "label": t.Label, "count": n})
		}
	}

	revenue := []currencyTotal{}
	if hasTable(h.DB, "billing_invoices") {
		h.DB.Table("billing_invoices").Select("COALESCE(NULLIF(currency,''),'AUD') AS currency, COUNT(*) AS count, COALESCE(SUM(total),0) AS invoiced, COALESCE(SUM(amount_paid),0) AS collected").
			Where("organization_id = ? AND deleted_at IS NULL AND doc_type = 'invoice' AND status NOT IN ('draft','void')", org.ID).
			Group("1").Scan(&revenue)
	}
	ok(c, http.StatusOK, gin.H{"organization": org, "users": users, "usage": usage, "revenue": revenue})
}

type orgInput struct {
	Name         string `json:"name"`
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	Website      string `json:"website"`
	ABN          string `json:"abn"`
	BusinessType string `json:"business_type"`
	Description  string `json:"description"`
	Address      struct {
		Street   string `json:"street"`
		Suburb   string `json:"suburb"`
		State    string `json:"state"`
		Postcode string `json:"postcode"`
		Country  string `json:"country"`
	} `json:"address"`
}

var abnRe = regexp.MustCompile(`^[0-9]{11}$`)

func (in *orgInput) normalise() string {
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.TrimSpace(strings.ToLower(in.Email))
	in.Phone = strings.TrimSpace(in.Phone)
	in.Website = strings.TrimSpace(in.Website)
	in.ABN = strings.ReplaceAll(strings.TrimSpace(in.ABN), " ", "")
	in.BusinessType = strings.TrimSpace(strings.ToLower(in.BusinessType))
	if in.BusinessType == "" {
		in.BusinessType = "general"
	}
	if in.Name == "" {
		return "Organisation name is required"
	}
	if len(in.Name) > 255 {
		return "Organisation name is too long"
	}
	if in.Email != "" {
		if _, err := mail.ParseAddress(in.Email); err != nil {
			return "Organisation email is not a valid email address"
		}
	}
	if in.ABN != "" && !abnRe.MatchString(in.ABN) {
		return "ABN must be 11 digits"
	}
	if len(in.Phone) > 20 {
		return "Phone number is too long"
	}
	if len(in.Address.Postcode) > 10 {
		return "Postcode is too long"
	}
	return ""
}

func (h *Handler) abnTaken(abn, exceptID string) bool {
	if abn == "" {
		return false
	}
	var n int64
	q := h.DB.Unscoped().Table("organizations").Where("abn = ?", abn)
	if exceptID != "" {
		q = q.Where("id <> ?", exceptID)
	}
	q.Count(&n)
	return n > 0
}

type adminInput struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	Password  string `json:"password"`
}

// CreateOrganization creates an organisation and its first admin user.
func (h *Handler) CreateOrganization(c *gin.Context) {
	var req struct {
		orgInput
		Admin adminInput `json:"admin"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	if msg := req.orgInput.normalise(); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	a := &req.Admin
	a.FirstName, a.LastName = strings.TrimSpace(a.FirstName), strings.TrimSpace(a.LastName)
	a.Email = strings.ToLower(strings.TrimSpace(a.Email))
	if a.FirstName == "" || a.LastName == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The admin user's first and last name are required")
		return
	}
	if _, err := mail.ParseAddress(a.Email); err != nil || a.Email == "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The admin user's email is not valid")
		return
	}
	generated := false
	if a.Password == "" {
		a.Password = TempPassword()
		generated = true
	} else if msg := passwordProblem(a.Password); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	if h.emailTaken(a.Email, "") {
		fail(c, http.StatusConflict, "EMAIL_TAKEN", "A user with this email already exists")
		return
	}
	if h.abnTaken(req.ABN, "") {
		fail(c, http.StatusConflict, "ABN_TAKEN", "Another organisation already uses this ABN")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(a.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(c, http.StatusInternalServerError, "HASH_ERROR", "Failed to secure the password")
		return
	}

	org := models.Organization{
		Name: req.Name, Email: req.Email, Phone: req.Phone, Website: req.Website,
		BusinessType: req.BusinessType, Description: req.Description, ABN: req.ABN,
		Address: models.Address{Street: req.Address.Street, Suburb: req.Address.Suburb, State: req.Address.State,
			Postcode: req.Address.Postcode, Country: req.Address.Country},
		EnabledModules: []string{},
		ModulesConfig:  map[string]bool{},
	}
	if org.Address.Country == "" {
		org.Address.Country = "Australia"
	}
	user := models.User{
		Email: a.Email, PasswordHash: string(hash), FirstName: a.FirstName, LastName: a.LastName,
		Phone: strings.TrimSpace(a.Phone), Role: "admin", IsActive: true,
	}
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		omit := []string{"Users", "Customers", "Vehicles", "Services", "Bookings"}
		if org.ABN == "" {
			omit = append(omit, "ABN") // stored as NULL so the unique index allows many blanks
		}
		if err := tx.Omit(omit...).Create(&org).Error; err != nil {
			return err
		}
		user.OrganizationID = org.ID
		if err := tx.Omit("Organization").Create(&user).Error; err != nil {
			return err
		}
		if err := tx.Create(&OrgStatus{OrganizationID: org.ID, Status: "active", UpdatedBy: actorID(c), UpdatedAt: time.Now()}).Error; err != nil {
			return err
		}
		return models.SetupOrganizationDefaults(tx, org.ID)
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to create the organisation: "+err.Error())
		return
	}
	// Start the organisation on its default plan straight away (same rules the
	// entitlement middleware applies on first use).
	if _, err := plans.NewService(h.DB, nil).Subscription(org.ID); err != nil {
		fmt.Printf("platform: default plan for %s: %v\n", org.ID, err)
	}
	Audit(h.DB, c, org.ID, "organization.create", "organization", org.ID, nil, gin.H{"name": org.Name, "admin_email": user.Email})
	row, _ := h.loadOrg(org.ID)
	resp := gin.H{"organization": row, "admin": gin.H{"id": user.ID, "email": user.Email}}
	if generated {
		resp["temporary_password"] = a.Password
	}
	ok(c, http.StatusCreated, resp)
}

// UpdateOrganization edits an organisation's details.
func (h *Handler) UpdateOrganization(c *gin.Context) {
	id := c.Param("id")
	before, err := h.loadOrg(id)
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Organisation not found")
		return
	}
	var in orgInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	if msg := in.normalise(); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	if h.abnTaken(in.ABN, id) {
		fail(c, http.StatusConflict, "ABN_TAKEN", "Another organisation already uses this ABN")
		return
	}
	var abn interface{}
	if in.ABN != "" {
		abn = in.ABN
	}
	upd := map[string]interface{}{
		"name": in.Name, "email": in.Email, "phone": in.Phone, "website": in.Website, "abn": abn,
		"business_type": in.BusinessType, "description": in.Description,
		"address_street": in.Address.Street, "address_suburb": in.Address.Suburb, "address_state": in.Address.State,
		"address_postcode": in.Address.Postcode, "address_country": in.Address.Country, "updated_at": time.Now(),
	}
	if err := h.DB.Table("organizations").Where("id = ?", id).Updates(upd).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to update the organisation")
		return
	}
	after, _ := h.loadOrg(id)
	Audit(h.DB, c, id, "organization.update", "organization", id,
		gin.H{"name": before.Name, "email": before.Email, "business_type": before.BusinessType},
		gin.H{"name": in.Name, "email": in.Email, "business_type": in.BusinessType})
	ok(c, http.StatusOK, gin.H{"organization": after})
}

// SuspendOrganization blocks sign-in for every user of the organisation.
func (h *Handler) SuspendOrganization(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&req)
	org, err := h.loadOrg(id)
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Organisation not found")
		return
	}
	if id == actorOrg(c) {
		fail(c, http.StatusBadRequest, "OWN_ORGANIZATION", "You cannot suspend the organisation you are signed in to")
		return
	}
	if org.Status == "suspended" {
		fail(c, http.StatusConflict, "ALREADY_SUSPENDED", "This organisation is already suspended")
		return
	}
	var ids []string
	h.DB.Table("users").Where("organization_id = ? AND deleted_at IS NULL AND is_active", id).Pluck("id", &ids)
	idsJSON, _ := json.Marshal(ids)
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		st := OrgStatus{OrganizationID: id, Status: "suspended", Reason: strings.TrimSpace(req.Reason),
			SuspendedUsers: string(idsJSON), UpdatedBy: actorID(c), UpdatedAt: time.Now()}
		if err := tx.Save(&st).Error; err != nil {
			return err
		}
		if len(ids) > 0 {
			if err := tx.Table("users").Where("id IN ?", ids).Updates(map[string]interface{}{"is_active": false, "updated_at": time.Now()}).Error; err != nil {
				return err
			}
			// Revoke outstanding refresh tokens so sessions cannot be renewed.
			if hasTable(tx, "refresh_tokens") {
				tx.Table("refresh_tokens").Where("user_id IN ?", ids).Update("is_revoked", true)
			}
		}
		if hasTable(tx, "organization_subscriptions") {
			tx.Table("organization_subscriptions").Where("organization_id = ?", id).Update("status", "suspended")
		}
		return nil
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to suspend the organisation")
		return
	}
	Audit(h.DB, c, id, "organization.suspend", "organization", id, gin.H{"status": "active"}, gin.H{"status": "suspended", "reason": req.Reason, "users_deactivated": len(ids)})
	row, _ := h.loadOrg(id)
	ok(c, http.StatusOK, gin.H{"organization": row, "users_deactivated": len(ids)})
}

// ActivateOrganization lifts a suspension and restores the users it disabled.
func (h *Handler) ActivateOrganization(c *gin.Context) {
	id := c.Param("id")
	if _, err := h.loadOrg(id); err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Organisation not found")
		return
	}
	var st OrgStatus
	if err := h.DB.First(&st, "organization_id = ?", id).Error; err != nil || st.Status != "suspended" {
		fail(c, http.StatusConflict, "NOT_SUSPENDED", "This organisation is not suspended")
		return
	}
	var ids []string
	_ = json.Unmarshal([]byte(st.SuspendedUsers), &ids)
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if len(ids) > 0 {
			if err := tx.Table("users").Where("id IN ? AND organization_id = ?", ids, id).Updates(map[string]interface{}{"is_active": true, "updated_at": time.Now()}).Error; err != nil {
				return err
			}
		}
		if hasTable(tx, "organization_subscriptions") {
			tx.Table("organization_subscriptions").Where("organization_id = ?", id).Update("status", "active")
		}
		return tx.Model(&OrgStatus{}).Where("organization_id = ?", id).Updates(map[string]interface{}{
			"status": "active", "reason": "", "suspended_users": "", "updated_by": actorID(c), "updated_at": time.Now(),
		}).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to reactivate the organisation")
		return
	}
	Audit(h.DB, c, id, "organization.activate", "organization", id, gin.H{"status": "suspended"}, gin.H{"status": "active", "users_restored": len(ids)})
	row, _ := h.loadOrg(id)
	ok(c, http.StatusOK, gin.H{"organization": row, "users_restored": len(ids)})
}

// DeleteOrganization soft-deletes an organisation after the caller types its
// exact name. Its users are deactivated and their sessions revoked.
func (h *Handler) DeleteOrganization(c *gin.Context) {
	id := c.Param("id")
	org, err := h.loadOrg(id)
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Organisation not found")
		return
	}
	var req struct {
		ConfirmName string `json:"confirm_name"`
	}
	_ = c.ShouldBindJSON(&req)
	if strings.TrimSpace(req.ConfirmName) != org.Name {
		fail(c, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "Type the organisation's exact name to confirm deletion")
		return
	}
	if id == actorOrg(c) {
		fail(c, http.StatusBadRequest, "OWN_ORGANIZATION", "You cannot delete the organisation you are signed in to")
		return
	}
	var superAdmins int64
	h.DB.Table("users").Where("organization_id = ? AND role = 'super_admin' AND deleted_at IS NULL", id).Count(&superAdmins)
	if superAdmins > 0 {
		fail(c, http.StatusBadRequest, "HAS_SUPER_ADMINS", "Move or demote this organisation's super admins before deleting it")
		return
	}
	var ids []string
	h.DB.Table("users").Where("organization_id = ? AND deleted_at IS NULL", id).Pluck("id", &ids)
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		if err := tx.Table("organizations").Where("id = ?", id).Update("deleted_at", now).Error; err != nil {
			return err
		}
		if len(ids) > 0 {
			if err := tx.Table("users").Where("id IN ?", ids).Updates(map[string]interface{}{"is_active": false, "updated_at": now}).Error; err != nil {
				return err
			}
			if hasTable(tx, "refresh_tokens") {
				tx.Table("refresh_tokens").Where("user_id IN ?", ids).Update("is_revoked", true)
			}
		}
		return nil
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to delete the organisation")
		return
	}
	Audit(h.DB, c, id, "organization.delete", "organization", id, gin.H{"name": org.Name, "users": len(ids)}, nil)
	ok(c, http.StatusOK, gin.H{"deleted": true, "users_deactivated": len(ids)})
}

// Impersonate issues a short-lived token that keeps the caller's identity and
// super-admin role but scopes every request to the target organisation.
func (h *Handler) Impersonate(c *gin.Context) {
	id := c.Param("id")
	org, err := h.loadOrg(id)
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "Organisation not found")
		return
	}
	expiry := time.Hour
	claims := jwt.MapClaims{
		"user_id":       actorID(c),
		"email":         c.GetString("user_email"),
		"role":          "super_admin",
		"org_id":        org.ID,
		"impersonating": true,
		"home_org_id":   actorOrg(c),
		"exp":           time.Now().Add(expiry).Unix(),
		"iat":           time.Now().Unix(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(h.Cfg.JWTSecret))
	if err != nil {
		fail(c, http.StatusInternalServerError, "TOKEN_ERROR", "Failed to create the session")
		return
	}
	Audit(h.DB, c, org.ID, "organization.impersonate", "organization", org.ID, nil, gin.H{"organization": org.Name})
	ok(c, http.StatusOK, gin.H{
		"token":        tok,
		"expires_in":   int(expiry.Seconds()),
		"organization": gin.H{"id": org.ID, "name": org.Name, "status": org.Status, "enabled_modules": org.EnabledModules},
	})
}

// ---------------------------------------------------------------- passwords

const pwAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

// TempPassword returns a random 14 character password that satisfies the
// platform's rules (upper, lower, digit and symbol).
func TempPassword() string {
	pick := func(set string) byte {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		return set[n.Int64()]
	}
	b := make([]byte, 0, 14)
	b = append(b, pick("ABCDEFGHJKLMNPQRSTUVWXYZ"), pick("abcdefghijkmnpqrstuvwxyz"), pick("23456789"), pick("!@#$%*?"))
	for len(b) < 14 {
		b = append(b, pick(pwAlphabet))
	}
	for i := len(b) - 1; i > 0; i-- {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		b[i], b[j.Int64()] = b[j.Int64()], b[i]
	}
	return string(b)
}

func passwordProblem(p string) string {
	if len(p) < 8 {
		return "Password must be at least 8 characters"
	}
	if len(p) > 72 {
		return "Password must be at most 72 characters"
	}
	var upper, lower, digit bool
	for _, r := range p {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	if !upper || !lower || !digit {
		return "Password needs an upper-case letter, a lower-case letter and a number"
	}
	return ""
}
