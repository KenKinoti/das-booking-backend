// Package account lets the signed-in user manage their own profile and password.
package account

import (
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
	_ "time/tzdata" // time zones even on hosts without zoneinfo
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/orgprefs"
)

// Handler serves /api/v1/account.
type Handler struct{ DB *gorm.DB }

func NewHandler(db *gorm.DB) *Handler { return &Handler{DB: db} }

// RegisterRoutes mounts the routes on g.
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("/profile", h.getProfile)
	g.PUT("/profile", h.updateProfile)
	g.POST("/change-password", h.changePassword)
	g.GET("/organization", h.getOrganization)
	g.PUT("/organization", h.updateOrganization)
}

func fail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"success": false, "error": msg, "message": msg})
}

func (h *Handler) me(c *gin.Context) (*models.User, bool) {
	id := c.GetString("user_id")
	if id == "" {
		fail(c, http.StatusUnauthorized, "Not signed in")
		return nil, false
	}
	var u models.User
	if err := h.DB.Omit("Organization").Where("id = ?", id).First(&u).Error; err != nil {
		fail(c, http.StatusNotFound, "Account not found")
		return nil, false
	}
	return &u, true
}

func view(u *models.User, orgName string) gin.H {
	return gin.H{
		"id": u.ID, "email": u.Email, "first_name": u.FirstName, "last_name": u.LastName, "phone": u.Phone,
		"role": u.Role, "organization_id": u.OrganizationID, "organization_name": orgName,
		"is_active": u.IsActive, "last_login_at": u.LastLoginAt, "created_at": u.CreatedAt, "updated_at": u.UpdatedAt,
	}
}

func (h *Handler) orgName(id string) string {
	var name string
	h.DB.Table("organizations").Select("name").Where("id = ?", id).Limit(1).Scan(&name)
	return name
}

func (h *Handler) getProfile(c *gin.Context) {
	u, found := h.me(c)
	if !found {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": view(u, h.orgName(u.OrganizationID))})
}

func (h *Handler) updateProfile(c *gin.Context) {
	u, found := h.me(c)
	if !found {
		return
	}
	var in struct {
		FirstName *string `json:"first_name"`
		LastName  *string `json:"last_name"`
		Phone     *string `json:"phone"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	upd := map[string]interface{}{}
	if in.FirstName != nil {
		v := strings.TrimSpace(*in.FirstName)
		if v == "" || len([]rune(v)) > 100 {
			fail(c, http.StatusBadRequest, "First name is required (up to 100 characters)")
			return
		}
		upd["first_name"] = v
	}
	if in.LastName != nil {
		v := strings.TrimSpace(*in.LastName)
		if v == "" || len([]rune(v)) > 100 {
			fail(c, http.StatusBadRequest, "Last name is required (up to 100 characters)")
			return
		}
		upd["last_name"] = v
	}
	if in.Phone != nil {
		v := strings.TrimSpace(*in.Phone)
		if len(v) > 20 {
			fail(c, http.StatusBadRequest, "Phone number is too long (20 characters max)")
			return
		}
		for _, r := range v {
			if !strings.ContainsRune("0123456789+-() .", r) {
				fail(c, http.StatusBadRequest, "Phone number can only contain digits, spaces and + - ( )")
				return
			}
		}
		upd["phone"] = v
	}
	if len(upd) > 0 {
		if err := h.DB.Model(&models.User{}).Where("id = ?", u.ID).Updates(upd).Error; err != nil {
			fail(c, http.StatusInternalServerError, "Could not save your profile")
			return
		}
	}
	u, _ = h.me(c)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": view(u, h.orgName(u.OrganizationID)), "message": "Profile updated"})
}

// ValidateNewPassword applies the password policy.
func ValidateNewPassword(current, next string) string {
	switch {
	case len(next) < 8:
		return "New password must be at least 8 characters"
	case len(next) > 72:
		return "New password must be 72 characters or fewer"
	case next == current:
		return "New password must be different from the current one"
	case strings.TrimSpace(next) == "":
		return "New password can't be blank"
	}
	return ""
}

func (h *Handler) changePassword(c *gin.Context) {
	u, found := h.me(c)
	if !found {
		return
	}
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	if in.CurrentPassword == "" {
		fail(c, http.StatusBadRequest, "Enter your current password")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.CurrentPassword)) != nil {
		// Slow down guessing a little.
		time.Sleep(400 * time.Millisecond)
		fail(c, http.StatusBadRequest, "Your current password is incorrect")
		return
	}
	if msg := ValidateNewPassword(in.CurrentPassword, in.NewPassword); msg != "" {
		fail(c, http.StatusBadRequest, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not update your password")
		return
	}
	if err := h.DB.Model(&models.User{}).Where("id = ?", u.ID).Update("password_hash", string(hash)).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not update your password")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Password changed"})
}

// ---------------------------------------------------------------------------
// Organisation profile and regional settings

func canEditOrg(c *gin.Context) bool {
	switch c.GetString("user_role") {
	case "super_admin", "admin", "owner", "manager":
		return true
	}
	return false
}

func (h *Handler) orgPayload(c *gin.Context, orgID string) (gin.H, bool) {
	var org models.Organization
	if err := h.DB.Omit("Users", "Customers", "Vehicles", "Services", "Bookings").Where("id = ?", orgID).First(&org).Error; err != nil {
		fail(c, http.StatusNotFound, "Organisation not found")
		return nil, false
	}
	var st models.OrganizationSettings
	if err := h.DB.Omit("Organization").Where("organization_id = ?", orgID).First(&st).Error; err != nil {
		st = models.OrganizationSettings{DateFormat: "DD/MM/YYYY", TimeFormat: "24h", Language: "en-AU", EnableEmailNotifications: true}
	}
	// Country, currency and time zone come from the single source.
	p := orgprefs.Get(h.DB, orgID)
	st.Currency, st.Timezone = p.Currency, p.Timezone
	return gin.H{
		"organization": gin.H{
			"id": org.ID, "name": org.Name, "abn": org.ABN, "phone": org.Phone, "email": org.Email,
			"website": org.Website, "description": org.Description, "address": org.Address, "business_type": org.BusinessType,
			"country_code": p.CountryCode, "country_name": p.CountryName, "region_source": p.Source, "needs_setup": p.NeedsSetup,
		},
		"settings": gin.H{
			"timezone": st.Timezone, "date_format": st.DateFormat, "time_format": st.TimeFormat, "currency": st.Currency,
			"language": st.Language, "enable_email_notifications": st.EnableEmailNotifications, "enable_sms_notifications": st.EnableSMSNotifications,
		},
		"can_edit": canEditOrg(c),
	}, true
}

func (h *Handler) getOrganization(c *gin.Context) {
	orgID := c.GetString("org_id")
	if orgID == "" {
		fail(c, http.StatusUnauthorized, "Organisation not found in token")
		return
	}
	if p, found := h.orgPayload(c, orgID); found {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": p})
	}
}

type orgInput struct {
	CountryCode string         `json:"country_code"`
	Name        string         `json:"name"`
	ABN         string         `json:"abn"`
	Phone       string         `json:"phone"`
	Email       string         `json:"email"`
	Website     string         `json:"website"`
	Description string         `json:"description"`
	Address     models.Address `json:"address"`
	Settings    struct {
		Timezone                 string `json:"timezone"`
		DateFormat               string `json:"date_format"`
		TimeFormat               string `json:"time_format"`
		Currency                 string `json:"currency"`
		Language                 string `json:"language"`
		EnableEmailNotifications bool   `json:"enable_email_notifications"`
		EnableSMSNotifications   bool   `json:"enable_sms_notifications"`
	} `json:"settings"`
}

// ValidateOrg checks the organisation form and returns a user-facing message.
func ValidateOrg(in *orgInput) string {
	in.Name = strings.TrimSpace(in.Name)
	in.ABN = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, in.ABN)
	in.Email = strings.TrimSpace(in.Email)
	in.Website = strings.TrimSpace(in.Website)
	in.Phone = strings.TrimSpace(in.Phone)
	switch {
	case in.Name == "" || len([]rune(in.Name)) > 255:
		return "Business name is required"
	case len(in.ABN) > 11:
		return "Business number can be at most 11 characters"
	case len(in.Phone) > 20:
		return "Phone number is too long (20 characters max)"
	}
	if in.Email != "" {
		if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email {
			return "Enter a valid email address"
		}
	}
	if in.Website != "" {
		if !strings.Contains(in.Website, "://") {
			in.Website = "https://" + in.Website
		}
		u, err := url.Parse(in.Website)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.Contains(u.Host, ".") {
			return "Enter a valid website, e.g. https://example.com"
		}
	}
	st := &in.Settings
	if st.Timezone == "" {
		st.Timezone = "Australia/Sydney"
	}
	if _, err := time.LoadLocation(st.Timezone); err != nil {
		return "Unknown time zone " + st.Timezone
	}
	st.Currency = strings.ToUpper(strings.TrimSpace(st.Currency))
	if st.Currency != "" && !currency.IsSupported(st.Currency) {
		return "Unsupported currency " + st.Currency
	}
	in.CountryCode = strings.ToUpper(strings.TrimSpace(in.CountryCode))
	if in.CountryCode != "" && !currency.IsCountry(in.CountryCode) {
		return "Choose a country from the list"
	}
	switch st.DateFormat {
	case "DD/MM/YYYY", "MM/DD/YYYY", "YYYY-MM-DD", "DD-MM-YYYY":
	default:
		return "Choose a date format"
	}
	if st.TimeFormat != "12h" && st.TimeFormat != "24h" {
		return "Choose a time format"
	}
	if st.Language == "" {
		st.Language = "en-AU"
	}
	if len(st.Language) > 10 {
		return "Invalid language"
	}
	return ""
}

func (h *Handler) updateOrganization(c *gin.Context) {
	orgID := c.GetString("org_id")
	if orgID == "" {
		fail(c, http.StatusUnauthorized, "Organisation not found in token")
		return
	}
	if !canEditOrg(c) {
		fail(c, http.StatusForbidden, "Only admins and managers can change organisation settings")
		return
	}
	var in orgInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	if msg := ValidateOrg(&in); msg != "" {
		fail(c, http.StatusBadRequest, msg)
		return
	}
	if in.ABN != "" {
		var n int64
		h.DB.Model(&models.Organization{}).Where("abn = ? AND id <> ?", in.ABN, orgID).Count(&n)
		if n > 0 {
			fail(c, http.StatusConflict, "Another organisation already uses that business number")
			return
		}
	}
	a := in.Address
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		upd := map[string]interface{}{
			"name": in.Name, "phone": in.Phone, "email": in.Email, "website": in.Website, "description": strings.TrimSpace(in.Description),
			"address_street": strings.TrimSpace(a.Street), "address_suburb": strings.TrimSpace(a.Suburb), "address_state": strings.TrimSpace(a.State),
			"address_postcode": strings.TrimSpace(a.Postcode), "address_country": strings.TrimSpace(a.Country),
		}
		// ABN is unique; store NULL-like empty only when it was empty before to avoid clashes.
		if in.ABN != "" {
			upd["abn"] = in.ABN
		}
		if err := tx.Model(&models.Organization{}).Where("id = ?", orgID).Updates(upd).Error; err != nil {
			return err
		}
		st := in.Settings
		var existing models.OrganizationSettings
		if tx.Omit("Organization").Where("organization_id = ?", orgID).First(&existing).Error != nil {
			existing = models.OrganizationSettings{ID: uuidString(), OrganizationID: orgID}
			if err := tx.Omit("Organization").Create(&existing).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.OrganizationSettings{}).Where("organization_id = ?", orgID).Updates(map[string]interface{}{
			"timezone": st.Timezone, "date_format": st.DateFormat, "time_format": st.TimeFormat, "currency": st.Currency,
			"language": st.Language, "enable_email_notifications": st.EnableEmailNotifications, "enable_sms_notifications": st.EnableSMSNotifications,
		}).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not save organisation settings")
		return
	}
	// Country / currency / time zone: the single source (and its mirrors).
	country := in.CountryCode
	if country == "" {
		country = strings.TrimSpace(a.Country)
	}
	if _, err := orgprefs.Set(h.DB, orgID, orgprefs.Input{CountryCode: country, Currency: in.Settings.Currency, Timezone: in.Settings.Timezone}, orgprefs.SourceUser); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	if p, found := h.orgPayload(c, orgID); found {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": p, "message": "Settings saved"})
	}
}

func uuidString() string { return uuid.New().String() }
