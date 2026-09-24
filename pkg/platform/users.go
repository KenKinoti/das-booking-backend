package platform

import (
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Roles a platform admin can assign.
var Roles = []string{"super_admin", "admin", "manager", "staff", "care_worker", "support_coordinator"}

func validRole(r string) bool {
	for _, x := range Roles {
		if x == r {
			return true
		}
	}
	return false
}

type userRow struct {
	ID               string     `json:"id"`
	Email            string     `json:"email"`
	FirstName        string     `json:"first_name"`
	LastName         string     `json:"last_name"`
	Phone            string     `json:"phone"`
	Role             string     `json:"role"`
	IsActive         bool       `json:"is_active"`
	OrganizationID   string     `json:"organization_id"`
	OrganizationName string     `json:"organization_name"`
	OrgStatus        string     `json:"organization_status"`
	LastLoginAt      *time.Time `json:"last_login_at"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

const userSelect = `u.id, u.email, u.first_name, u.last_name, u.phone, u.role, u.is_active, u.organization_id,
	COALESCE(o.name, '') AS organization_name, COALESCE(s.status, 'active') AS org_status,
	u.last_login_at, u.created_at, u.updated_at`

func (h *Handler) userQuery() *gorm.DB {
	return h.DB.Table("users u").
		Joins("LEFT JOIN organizations o ON o.id = u.organization_id").
		Joins("LEFT JOIN platform_org_status s ON s.organization_id = u.organization_id").
		Where("u.deleted_at IS NULL")
}

func (h *Handler) loadUser(id string) (*userRow, error) {
	rows := []userRow{}
	if err := h.userQuery().Select(userSelect).Where("u.id = ?", id).Limit(1).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &rows[0], nil
}

func (h *Handler) emailTaken(email, exceptID string) bool {
	var n int64
	q := h.DB.Unscoped().Table("users").Where("LOWER(email) = ?", strings.ToLower(email))
	if exceptID != "" {
		q = q.Where("id <> ?", exceptID)
	}
	q.Count(&n)
	return n > 0
}

func (h *Handler) orgExists(id string) bool {
	var n int64
	h.DB.Table("organizations").Where("id = ? AND deleted_at IS NULL", id).Count(&n)
	return n > 0
}

// ListUsers lists users across every organisation.
func (h *Handler) ListUsers(c *gin.Context) {
	page, limit, offset := pageParams(c, 25, 200)
	q := h.userQuery().Where("o.deleted_at IS NULL")
	if s := strings.TrimSpace(c.Query("search")); s != "" {
		p := like(s)
		q = q.Where("LOWER(u.email) LIKE ? OR LOWER(u.first_name || ' ' || u.last_name) LIKE ? OR LOWER(COALESCE(o.name,'')) LIKE ?", p, p, p)
	}
	if org := c.Query("organization_id"); org != "" {
		q = q.Where("u.organization_id = ?", org)
	}
	if role := c.Query("role"); role != "" {
		q = q.Where("u.role = ?", role)
	}
	switch c.Query("status") {
	case "active":
		q = q.Where("u.is_active")
	case "inactive":
		q = q.Where("NOT u.is_active")
	case "never":
		q = q.Where("u.last_login_at IS NULL")
	case "recent":
		q = q.Where("u.last_login_at >= ?", time.Now().AddDate(0, 0, -30))
	}
	var total int64
	if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load users")
		return
	}
	order := "u.created_at DESC"
	switch c.Query("sort") {
	case "name":
		order = "LOWER(u.first_name || ' ' || u.last_name) ASC"
	case "last_login":
		order = "u.last_login_at DESC NULLS LAST"
	case "organization":
		order = "LOWER(o.name) ASC, LOWER(u.first_name) ASC"
	}
	rows := []userRow{}
	if err := q.Select(userSelect).Order(order).Limit(limit).Offset(offset).Scan(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to load users")
		return
	}

	// Summary counts for the KPI row (unfiltered).
	var all, active, recent, never int64
	base := func() *gorm.DB { return h.userQuery().Where("o.deleted_at IS NULL") }
	base().Count(&all)
	base().Where("u.is_active").Count(&active)
	base().Where("u.last_login_at >= ?", time.Now().AddDate(0, 0, -30)).Count(&recent)
	base().Where("u.last_login_at IS NULL").Count(&never)

	ok(c, http.StatusOK, gin.H{
		"users": rows, "total": total, "page": page, "limit": limit, "roles": Roles,
		"summary": gin.H{"total": all, "active": active, "active_30d": recent, "never_signed_in": never},
	})
}

type userInput struct {
	FirstName      string `json:"first_name"`
	LastName       string `json:"last_name"`
	Email          string `json:"email"`
	Phone          string `json:"phone"`
	Role           string `json:"role"`
	OrganizationID string `json:"organization_id"`
	IsActive       *bool  `json:"is_active"`
	Password       string `json:"password"`
}

func (in *userInput) normalise() string {
	in.FirstName, in.LastName = strings.TrimSpace(in.FirstName), strings.TrimSpace(in.LastName)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Phone = strings.TrimSpace(in.Phone)
	in.Role = strings.TrimSpace(in.Role)
	if in.FirstName == "" || in.LastName == "" {
		return "First and last name are required"
	}
	if len(in.FirstName) > 100 || len(in.LastName) > 100 {
		return "Name is too long"
	}
	if _, err := mail.ParseAddress(in.Email); err != nil || in.Email == "" {
		return "Enter a valid email address"
	}
	if !validRole(in.Role) {
		return "Choose a valid role"
	}
	if in.OrganizationID == "" {
		return "Choose an organisation"
	}
	if len(in.Phone) > 20 {
		return "Phone number is too long"
	}
	return ""
}

// CreateUser creates a user in any organisation.
func (h *Handler) CreateUser(c *gin.Context) {
	var in userInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	if msg := in.normalise(); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	if !h.orgExists(in.OrganizationID) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "That organisation does not exist")
		return
	}
	if h.emailTaken(in.Email, "") {
		fail(c, http.StatusConflict, "EMAIL_TAKEN", "A user with this email already exists")
		return
	}
	generated := false
	if in.Password == "" {
		in.Password = TempPassword()
		generated = true
	} else if msg := passwordProblem(in.Password); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(c, http.StatusInternalServerError, "HASH_ERROR", "Failed to secure the password")
		return
	}
	active := true
	if in.IsActive != nil {
		active = *in.IsActive
	}
	u := models.User{Email: in.Email, PasswordHash: string(hash), FirstName: in.FirstName, LastName: in.LastName,
		Phone: in.Phone, Role: in.Role, OrganizationID: in.OrganizationID, IsActive: active}
	if err := h.DB.Omit("Organization").Create(&u).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to create the user")
		return
	}
	if !active {
		// gorm skips zero-value bools on create when a default exists.
		h.DB.Table("users").Where("id = ?", u.ID).Update("is_active", false)
	}
	Audit(h.DB, c, in.OrganizationID, "user.create", "user", u.ID, nil, gin.H{"email": u.Email, "role": u.Role})
	row, _ := h.loadUser(u.ID)
	resp := gin.H{"user": row}
	if generated {
		resp["temporary_password"] = in.Password
	}
	ok(c, http.StatusCreated, resp)
}

// UpdateUser edits a user's profile, role, organisation or active flag.
func (h *Handler) UpdateUser(c *gin.Context) {
	id := c.Param("id")
	before, err := h.loadUser(id)
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "User not found")
		return
	}
	var in userInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	if msg := in.normalise(); msg != "" {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
		return
	}
	if !h.orgExists(in.OrganizationID) {
		fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "That organisation does not exist")
		return
	}
	if h.emailTaken(in.Email, id) {
		fail(c, http.StatusConflict, "EMAIL_TAKEN", "Another user already has this email")
		return
	}
	active := before.IsActive
	if in.IsActive != nil {
		active = *in.IsActive
	}
	if id == actorID(c) {
		if !active {
			fail(c, http.StatusBadRequest, "SELF", "You cannot deactivate your own account")
			return
		}
		if in.Role != "super_admin" {
			fail(c, http.StatusBadRequest, "SELF", "You cannot remove your own super admin role")
			return
		}
	}
	if before.Role == "super_admin" && (in.Role != "super_admin" || !active) {
		var others int64
		h.DB.Table("users").Where("role = 'super_admin' AND is_active AND deleted_at IS NULL AND id <> ?", id).Count(&others)
		if others == 0 {
			fail(c, http.StatusBadRequest, "LAST_SUPER_ADMIN", "At least one active super admin must remain")
			return
		}
	}
	upd := map[string]interface{}{
		"first_name": in.FirstName, "last_name": in.LastName, "email": in.Email, "phone": in.Phone,
		"role": in.Role, "organization_id": in.OrganizationID, "is_active": active, "updated_at": time.Now(),
	}
	if err := h.DB.Table("users").Where("id = ?", id).Updates(upd).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to update the user")
		return
	}
	if before.IsActive && !active && hasTable(h.DB, "refresh_tokens") {
		h.DB.Table("refresh_tokens").Where("user_id = ?", id).Update("is_revoked", true)
	}
	action := "user.update"
	if before.IsActive && !active {
		action = "user.deactivate"
	} else if !before.IsActive && active {
		action = "user.activate"
	}
	Audit(h.DB, c, in.OrganizationID, action, "user", id,
		gin.H{"email": before.Email, "role": before.Role, "organization_id": before.OrganizationID, "is_active": before.IsActive},
		gin.H{"email": in.Email, "role": in.Role, "organization_id": in.OrganizationID, "is_active": active})
	row, _ := h.loadUser(id)
	ok(c, http.StatusOK, gin.H{"user": row})
}

// ResetPassword sets a new temporary password and returns it once.
func (h *Handler) ResetPassword(c *gin.Context) {
	id := c.Param("id")
	u, err := h.loadUser(id)
	if err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "User not found")
		return
	}
	pw := TempPassword()
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		fail(c, http.StatusInternalServerError, "HASH_ERROR", "Failed to secure the password")
		return
	}
	if err := h.DB.Table("users").Where("id = ?", id).Updates(map[string]interface{}{"password_hash": string(hash), "updated_at": time.Now()}).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to reset the password")
		return
	}
	if hasTable(h.DB, "refresh_tokens") {
		h.DB.Table("refresh_tokens").Where("user_id = ?", id).Update("is_revoked", true)
	}
	Audit(h.DB, c, u.OrganizationID, "user.reset_password", "user", id, nil, gin.H{"email": u.Email})
	ok(c, http.StatusOK, gin.H{"temporary_password": pw, "email": u.Email})
}
