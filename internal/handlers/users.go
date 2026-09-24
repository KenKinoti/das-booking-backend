package handlers

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func (h *Handler) GetCurrentUser(c *gin.Context) {
	userID, exists := c.Get("user_id")
	fmt.Printf("GetCurrentUser - userID: %v, exists: %v\n", userID, exists)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "User not authenticated",
			},
		})
		return
	}

	var user models.User
	if err := h.DB.Where("id = ?", userID).First(&user).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "USER_NOT_FOUND",
				"message": "User not found",
			},
		})
		return
	}

	// Convert to response format (without password hash)
	userResponse := UserResponse{
		ID:             user.ID,
		Email:          user.Email,
		FirstName:      user.FirstName,
		LastName:       user.LastName,
		Phone:          user.Phone,
		Role:           user.Role,
		OrganizationID: user.OrganizationID,
		IsActive:       user.IsActive,
		LastLoginAt:    user.LastLoginAt,
		CreatedAt:      user.CreatedAt,
		UpdatedAt:      user.UpdatedAt,
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    userResponse,
	})
}

// assignableRoles are the roles an organisation admin can give to people.
var assignableRoles = map[string]bool{
	"admin":               true,
	"manager":             true,
	"staff":               true,
	"care_worker":         true,
	"support_coordinator": true,
}

func userErr(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func toUserResponse(u models.User) UserResponse {
	return UserResponse{
		ID: u.ID, Email: u.Email, FirstName: u.FirstName, LastName: u.LastName, Phone: u.Phone,
		Role: u.Role, OrganizationID: u.OrganizationID, IsActive: u.IsActive, LastLoginAt: u.LastLoginAt,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

// temporaryPassword returns a random password that satisfies the usual
// complexity rules (upper, lower, digit, symbol).
func temporaryPassword() string {
	const (
		upper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		lower  = "abcdefghijkmnpqrstuvwxyz"
		digits = "23456789"
		syms   = "!@#$%*?"
	)
	pick := func(set string) byte {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		return set[n.Int64()]
	}
	all := upper + lower + digits
	b := []byte{pick(upper), pick(lower), pick(digits), pick(syms)}
	for len(b) < 14 {
		b = append(b, pick(all))
	}
	for i := len(b) - 1; i > 0; i-- {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		j := n.Int64()
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

func validPassword(p string) bool {
	return len(p) >= 8 && len(p) <= 72
}

// GetUsers lists users of the caller's organisation. Super admins may pass
// all_orgs=true to list users of every organisation.
// Query: page, limit, role, is_active, search, created_after, all_orgs.
func (h *Handler) GetUsers(c *gin.Context) {
	orgID := c.GetString("org_id")
	if orgID == "" {
		userErr(c, http.StatusUnauthorized, "UNAUTHORIZED", "Organization not found in context")
		return
	}
	userRole := c.GetString("user_role")

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	if page < 1 {
		page = 1
	}
	maxLimit := 100
	if userRole == "super_admin" || userRole == "admin" || userRole == "manager" {
		maxLimit = 1000
	}
	if limit < 1 {
		limit = 10
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	offset := (page - 1) * limit

	query := h.DB.Model(&models.User{})
	if !(userRole == "super_admin" && c.Query("all_orgs") == "true") {
		query = query.Where("organization_id = ?", orgID)
	}
	if role := c.Query("role"); role != "" {
		query = query.Where("role = ?", role)
	}
	if isActive := c.Query("is_active"); isActive != "" {
		activeFilter, _ := strconv.ParseBool(isActive)
		query = query.Where("is_active = ?", activeFilter)
	}
	if search := strings.TrimSpace(c.Query("search")); search != "" {
		term := "%" + strings.ToLower(search) + "%"
		query = query.Where("(LOWER(first_name) LIKE ? OR LOWER(last_name) LIKE ? OR LOWER(first_name || ' ' || last_name) LIKE ? OR LOWER(email) LIKE ? OR phone LIKE ?)", term, term, term, term, term)
	}
	if createdAfter := c.Query("created_after"); createdAfter != "" {
		if t, err := time.Parse(time.RFC3339, createdAfter); err == nil {
			query = query.Where("created_at >= ?", t)
		}
	}

	var total int64
	query.Session(&gorm.Session{}).Count(&total)

	order := "created_at DESC"
	if c.Query("sort") == "name" {
		order = "first_name ASC, last_name ASC"
	}
	var users []models.User
	if err := query.Limit(limit).Offset(offset).Order(order).Find(&users).Error; err != nil {
		userErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to fetch users")
		return
	}

	out := make([]UserResponse, len(users))
	for i, u := range users {
		out[i] = toUserResponse(u)
	}

	// Role/status counts for the whole (unfiltered) organisation.
	type roleCount struct {
		Role     string `json:"role"`
		IsActive bool   `json:"is_active"`
		N        int64  `json:"count"`
	}
	var counts []roleCount
	h.DB.Model(&models.User{}).Select("role, is_active, COUNT(*) AS n").Where("organization_id = ?", orgID).Group("role, is_active").Scan(&counts)
	summary := gin.H{"total": int64(0), "active": int64(0), "inactive": int64(0), "admins": int64(0)}
	for _, rc := range counts {
		summary["total"] = summary["total"].(int64) + rc.N
		if rc.IsActive {
			summary["active"] = summary["active"].(int64) + rc.N
		} else {
			summary["inactive"] = summary["inactive"].(int64) + rc.N
		}
		if rc.Role == "admin" || rc.Role == "super_admin" {
			summary["admins"] = summary["admins"].(int64) + rc.N
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"users":   out,
			"summary": summary,
			"pagination": gin.H{
				"page":        page,
				"limit":       limit,
				"total":       total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

type CreateUserRequest struct {
	Email     string `json:"email" binding:"required,email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name" binding:"required"`
	LastName  string `json:"last_name"`
	Phone     string `json:"phone"`
	Role      string `json:"role" binding:"required"`
}

// CreateUser adds a user to the caller's organisation. When no password is
// given a temporary one is generated and returned once in the response.
func (h *Handler) CreateUser(c *gin.Context) {
	orgID := c.GetString("org_id")
	if orgID == "" {
		userErr(c, http.StatusUnauthorized, "UNAUTHORIZED", "Organization not found in context")
		return
	}

	var req CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		userErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Enter a first name, a valid email address and a role")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.FirstName = strings.TrimSpace(req.FirstName)
	req.LastName = strings.TrimSpace(req.LastName)
	if !assignableRoles[req.Role] {
		userErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose a valid role")
		return
	}
	if req.Role == "admin" && c.GetString("user_role") != "admin" && c.GetString("user_role") != "super_admin" {
		userErr(c, http.StatusForbidden, "FORBIDDEN", "Only admins can create other admins")
		return
	}
	generated := ""
	if req.Password == "" {
		generated = temporaryPassword()
		req.Password = generated
	} else if !validPassword(req.Password) {
		userErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Password must be 8 to 72 characters")
		return
	}

	var existing models.User
	if err := h.DB.Unscoped().Where("LOWER(email) = ?", req.Email).First(&existing).Error; err == nil {
		if existing.DeletedAt.Valid && existing.OrganizationID == orgID {
			// Re-creating someone who was removed earlier: restore the account.
		} else {
			userErr(c, http.StatusConflict, "USER_EXISTS", "A user with this email already exists")
			return
		}
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		userErr(c, http.StatusInternalServerError, "PASSWORD_HASH_ERROR", "Failed to hash password")
		return
	}

	var user models.User
	if existing.ID != "" {
		if err := h.DB.Unscoped().Model(&models.User{}).Where("id = ?", existing.ID).Updates(map[string]interface{}{
			"deleted_at": nil, "password_hash": string(hashed), "first_name": req.FirstName, "last_name": req.LastName,
			"phone": req.Phone, "role": req.Role, "is_active": true,
		}).Error; err != nil {
			userErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to create user")
			return
		}
		h.DB.First(&user, "id = ?", existing.ID)
	} else {
		user = models.User{
			ID: uuid.NewString(), Email: req.Email, PasswordHash: string(hashed), FirstName: req.FirstName,
			LastName: req.LastName, Phone: req.Phone, Role: req.Role, OrganizationID: orgID, IsActive: true,
		}
		if err := h.DB.Omit("Organization", "Shifts", "UploadedDocs", "UserPermissions", "RefreshTokens").Create(&user).Error; err != nil {
			userErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to create user")
			return
		}
	}

	resp := gin.H{"success": true, "data": toUserResponse(user), "message": "User created"}
	if generated != "" {
		resp["temporary_password"] = generated
	}
	c.JSON(http.StatusCreated, resp)
}

type UpdateUserRequest struct {
	FirstName *string `json:"first_name,omitempty"`
	LastName  *string `json:"last_name,omitempty"`
	Email     *string `json:"email,omitempty"`
	Phone     *string `json:"phone,omitempty"`
	Role      *string `json:"role,omitempty"`
	IsActive  *bool   `json:"is_active,omitempty"`
}

// findOrgUser loads a user of the caller's organisation (super admins: any).
func (h *Handler) findOrgUser(c *gin.Context, id string) (*models.User, bool) {
	var user models.User
	q := h.DB.Where("id = ?", id)
	if c.GetString("user_role") != "super_admin" {
		q = q.Where("organization_id = ?", c.GetString("org_id"))
	}
	if err := q.First(&user).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			userErr(c, http.StatusNotFound, "USER_NOT_FOUND", "User not found")
		} else {
			userErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to fetch user")
		}
		return nil, false
	}
	return &user, true
}

func isUserAdmin(role string) bool {
	return role == "admin" || role == "super_admin"
}

// UpdateUser edits a user. Admins can edit anyone in their organisation;
// everyone else can only edit their own name and phone.
func (h *Handler) UpdateUser(c *gin.Context) {
	userID := c.Param("id")
	if c.GetString("org_id") == "" {
		userErr(c, http.StatusUnauthorized, "UNAUTHORIZED", "Organization not found in context")
		return
	}
	currentUserID := c.GetString("user_id")
	currentRole := c.GetString("user_role")
	self := currentUserID == userID

	var req UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		userErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}

	user, ok := h.findOrgUser(c, userID)
	if !ok {
		return
	}

	if !isUserAdmin(currentRole) {
		if !self {
			userErr(c, http.StatusForbidden, "FORBIDDEN", "Only admins can edit other users")
			return
		}
		if req.Role != nil || req.IsActive != nil || req.Email != nil {
			userErr(c, http.StatusForbidden, "FORBIDDEN", "Only admins can change roles, status or email")
			return
		}
	}
	if user.Role == "super_admin" && currentRole != "super_admin" {
		userErr(c, http.StatusForbidden, "FORBIDDEN", "Super admin accounts can only be changed by a super admin")
		return
	}
	if self && req.IsActive != nil && !*req.IsActive {
		userErr(c, http.StatusBadRequest, "INVALID_OPERATION", "You cannot deactivate your own account")
		return
	}
	if req.Role != nil && *req.Role != user.Role {
		if self {
			userErr(c, http.StatusBadRequest, "INVALID_OPERATION", "You cannot change your own role")
			return
		}
		if !assignableRoles[*req.Role] {
			userErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Choose a valid role")
			return
		}
	}

	updates := map[string]interface{}{}
	if req.FirstName != nil {
		if strings.TrimSpace(*req.FirstName) == "" {
			userErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "First name is required")
			return
		}
		updates["first_name"] = strings.TrimSpace(*req.FirstName)
	}
	if req.LastName != nil {
		updates["last_name"] = strings.TrimSpace(*req.LastName)
	}
	if req.Phone != nil {
		updates["phone"] = strings.TrimSpace(*req.Phone)
	}
	if req.Email != nil {
		email := strings.ToLower(strings.TrimSpace(*req.Email))
		if !strings.Contains(email, "@") {
			userErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Enter a valid email address")
			return
		}
		if email != strings.ToLower(user.Email) {
			var n int64
			h.DB.Unscoped().Model(&models.User{}).Where("LOWER(email) = ? AND id <> ?", email, user.ID).Count(&n)
			if n > 0 {
				userErr(c, http.StatusConflict, "USER_EXISTS", "A user with this email already exists")
				return
			}
			updates["email"] = email
		}
	}
	if req.Role != nil {
		updates["role"] = *req.Role
	}
	if req.IsActive != nil {
		updates["is_active"] = *req.IsActive
	}

	if len(updates) > 0 {
		if err := h.DB.Model(&models.User{}).Where("id = ?", user.ID).Updates(updates).Error; err != nil {
			userErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to update user")
			return
		}
		if req.IsActive != nil && !*req.IsActive {
			h.DB.Model(&models.RefreshToken{}).Where("user_id = ?", user.ID).Update("is_revoked", true)
		}
	}
	h.DB.First(user, "id = ?", user.ID)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": toUserResponse(*user), "message": "User updated"})
}

// ResetUserPassword sets a new password for a user (admin only). When no
// password is sent a temporary one is generated and returned once.
func (h *Handler) ResetUserPassword(c *gin.Context) {
	if !isUserAdmin(c.GetString("user_role")) {
		userErr(c, http.StatusForbidden, "FORBIDDEN", "Only admins can reset passwords")
		return
	}
	user, ok := h.findOrgUser(c, c.Param("id"))
	if !ok {
		return
	}
	if user.Role == "super_admin" && c.GetString("user_role") != "super_admin" {
		userErr(c, http.StatusForbidden, "FORBIDDEN", "Super admin accounts can only be changed by a super admin")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	_ = c.ShouldBindJSON(&req)
	generated := ""
	if req.Password == "" {
		generated = temporaryPassword()
		req.Password = generated
	} else if !validPassword(req.Password) {
		userErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Password must be 8 to 72 characters")
		return
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		userErr(c, http.StatusInternalServerError, "PASSWORD_HASH_ERROR", "Failed to hash password")
		return
	}
	if err := h.DB.Model(&models.User{}).Where("id = ?", user.ID).Update("password_hash", string(hashed)).Error; err != nil {
		userErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to reset password")
		return
	}
	h.DB.Model(&models.RefreshToken{}).Where("user_id = ?", user.ID).Update("is_revoked", true)
	resp := gin.H{"success": true, "message": "Password reset"}
	if generated != "" {
		resp["temporary_password"] = generated
	}
	c.JSON(http.StatusOK, resp)
}

// DeleteUser removes a user from the organisation (soft delete).
func (h *Handler) DeleteUser(c *gin.Context) {
	userID := c.Param("id")
	if c.GetString("user_id") == userID {
		userErr(c, http.StatusBadRequest, "INVALID_OPERATION", "You cannot delete your own account")
		return
	}
	user, ok := h.findOrgUser(c, userID)
	if !ok {
		return
	}
	if user.Email == "kennedy@dasyin.com.au" || (user.Role == "super_admin" && c.GetString("user_role") != "super_admin") {
		userErr(c, http.StatusForbidden, "PROTECTED_USER", "This administrator account cannot be deleted")
		return
	}
	if err := h.DB.Delete(user).Error; err != nil {
		userErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to delete user")
		return
	}
	h.DB.Model(&models.RefreshToken{}).Where("user_id = ?", userID).Update("is_revoked", true)
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "User deleted"})
}
