// Package platform implements the super-admin ("platform admin") API:
// cross-organisation statistics, organisation and user management, the audit
// feed, database health and the platform's own integration settings.
//
// Every route registered here sits behind RequireSuperAdmin.
package platform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// Handler serves the platform admin API.
type Handler struct {
	DB  *gorm.DB
	Cfg *config.Config
}

// NewHandler builds a handler.
func NewHandler(db *gorm.DB, cfg *config.Config) *Handler { return &Handler{DB: db, Cfg: cfg} }

// OrgStatus tracks suspension of an organisation. Users that were active when
// the organisation was suspended are remembered so reactivation restores
// exactly those accounts.
type OrgStatus struct {
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	Status         string    `json:"status" gorm:"type:varchar(20);not null;default:'active'"`
	Reason         string    `json:"reason" gorm:"type:text"`
	SuspendedUsers string    `json:"-" gorm:"type:text"`
	UpdatedBy      string    `json:"updated_by" gorm:"type:varchar(255)"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName keeps the table name explicit.
func (OrgStatus) TableName() string { return "platform_org_status" }

// Migrate creates the package's tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&OrgStatus{})
}

// RequireSuperAdmin rejects anyone who is not a super admin.
func RequireSuperAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString("user_role") != "super_admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"success": false,
				"error":   gin.H{"code": "SUPER_ADMIN_REQUIRED", "message": "Super administrator access required"},
			})
			return
		}
		c.Next()
	}
}

// RegisterRoutes mounts everything under <group>/super-admin/platform.
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	g := auth.Group("/super-admin/platform", RequireSuperAdmin())

	g.GET("/overview", h.Overview)

	g.GET("/organizations", h.ListOrganizations)
	g.GET("/organizations/duplicates", h.Duplicates)
	g.POST("/organizations/merge", h.Merge)
	g.POST("/organizations", h.CreateOrganization)
	g.GET("/organizations/:id", h.GetOrganization)
	g.PUT("/organizations/:id", h.UpdateOrganization)
	g.POST("/organizations/:id/suspend", h.SuspendOrganization)
	g.POST("/organizations/:id/activate", h.ActivateOrganization)
	g.POST("/organizations/:id/impersonate", h.Impersonate)
	g.POST("/organizations/:id/reset", h.ResetOrganization)  // reset for handover (orgreset.go)
	g.GET("/organizations/:id/export", h.ExportOrganization) // backup ZIP of what a reset removes
	g.DELETE("/organizations/:id", h.DeleteOrganization)

	g.GET("/modules", h.ModuleMatrix)

	g.GET("/users", h.ListUsers)
	g.POST("/users", h.CreateUser)
	g.PUT("/users/:id", h.UpdateUser)
	g.POST("/users/:id/reset-password", h.ResetPassword)

	g.GET("/audit-logs", h.AuditFeed)

	g.GET("/database", h.DatabaseHealth)
	g.POST("/database/analyze", h.AnalyzeDatabase)

	g.GET("/integrations", h.GetIntegrations)
	g.PUT("/integrations", h.UpdateIntegrations)
	g.POST("/integrations/test/:kind", h.TestIntegration)
	g.GET("/integrations/logs", h.IntegrationLogs)
	g.POST("/integrations/send-test-email", h.SendTestEmail)
}

// ---------------------------------------------------------------- helpers

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func fail(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func actorID(c *gin.Context) string  { return c.GetString("user_id") }
func actorOrg(c *gin.Context) string { return c.GetString("org_id") }

func pageParams(c *gin.Context, def, max int) (page, limit, offset int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ = strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(def)))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > max {
		limit = def
	}
	return page, limit, (page - 1) * limit
}

func like(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
	return "%" + s + "%"
}

func hasTable(db *gorm.DB, name string) bool { return db.Migrator().HasTable(name) }

// Audit writes one row to audit_logs. Failures are logged, never fatal.
func Audit(db *gorm.DB, c *gin.Context, orgID, action, entityType, entityID string, oldV, newV interface{}) {
	enc := func(v interface{}) string {
		if v == nil {
			return "{}"
		}
		b, err := json.Marshal(v)
		if err != nil || string(b) == "null" {
			return "{}"
		}
		return string(b)
	}
	if orgID == "" {
		orgID = actorOrg(c)
	}
	var uid *string
	if u := actorID(c); u != "" {
		uid = &u
	}
	ua := c.Request.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}
	row := models.AuditLog{
		ID:             uuid.NewString(),
		OrganizationID: orgID,
		UserID:         uid,
		Action:         action,
		EntityType:     entityType,
		EntityID:       entityID,
		OldValues:      enc(oldV),
		NewValues:      enc(newV),
		IPAddress:      c.ClientIP(),
		UserAgent:      ua,
		CreatedAt:      time.Now(),
	}
	if err := db.Omit("Organization", "User").Create(&row).Error; err != nil {
		fmt.Printf("platform: audit write failed: %v\n", err)
	}
}
