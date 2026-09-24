package branding

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Migrate creates the org_logos table.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&OrgLogo{}) }

// Service reads and writes organisation logos.
type Service struct{ DB *gorm.DB }

func NewService(db *gorm.DB) *Service { return &Service{DB: db} }

// Meta returns the logo's metadata (no image bytes), or nil when there is none.
func (s *Service) Meta(org string) (*OrgLogo, error) {
	if org == "" {
		return nil, nil
	}
	var l OrgLogo
	err := s.DB.Select("organization_id", "content_type", "etag", "size", "width", "height", "updated_at", "updated_by").
		Where("organization_id = ?", org).Take(&l).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// Save validates and stores (replaces) an organisation's logo.
func (s *Service) Save(org, user string, data []byte) (*OrgLogo, error) {
	chk, err := Validate(data)
	if err != nil {
		return nil, err
	}
	l := OrgLogo{
		OrganizationID: org, ContentType: chk.ContentType, Data: data, ETag: chk.ETag, Size: len(data),
		Width: chk.Width, Height: chk.Height, UpdatedAt: time.Now().UTC(), UpdatedBy: user,
	}
	err = s.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "organization_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"content_type", "data", "etag", "size", "width", "height", "updated_at", "updated_by"}),
	}).Create(&l).Error
	if err != nil {
		return nil, err
	}
	l.Data = nil
	return &l, nil
}

// Delete removes an organisation's logo.
func (s *Service) Delete(org string) error {
	return s.DB.Where("organization_id = ?", org).Delete(&OrgLogo{}).Error
}

// BaseURL is the public origin of this API server: PUBLIC_API_URL, else
// derived from the request (X-Forwarded-Proto/Host). "" when neither is known.
func BaseURL(r *http.Request) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_API_URL")), "/"); v != "" {
		return strings.TrimSuffix(v, "/api/v1")
	}
	if r == nil {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// PathFor is the versioned public path of a logo.
func PathFor(org, etag string) string {
	return "/api/v1/public/org-logo/" + url.PathEscape(org) + "?v=" + url.QueryEscape(etag)
}

// URLFor returns the absolute (when the origin is known), versioned public
// URL of the organisation's logo, or "" when it has none.
func (s *Service) URLFor(r *http.Request, org string) string {
	l, err := s.Meta(org)
	if err != nil || l == nil {
		return ""
	}
	return BaseURL(r) + PathFor(org, l.ETag)
}

// Info is the branding summary for an organisation: name, logo and the
// trading name / address from its invoice settings.
func (s *Service) Info(r *http.Request, org string) gin.H {
	out := gin.H{"organization_id": org, "name": "", "logo_url": "", "has_logo": false, "updated_at": nil, "trading_name": "", "address": ""}
	var name string
	s.DB.Raw("SELECT name FROM organizations WHERE id = ?", org).Scan(&name)
	out["name"] = name
	if s.DB.Migrator().HasTable("billing_settings") {
		var bs struct {
			BusinessName string
			Address      string
		}
		s.DB.Raw("SELECT business_name, address FROM billing_settings WHERE organization_id = ?", org).Scan(&bs)
		out["trading_name"] = bs.BusinessName
		out["address"] = bs.Address
	}
	if l, err := s.Meta(org); err == nil && l != nil {
		out["logo_url"] = BaseURL(r) + PathFor(org, l.ETag)
		out["has_logo"] = true
		out["updated_at"] = l.UpdatedAt
		out["content_type"] = l.ContentType
		out["width"] = l.Width
		out["height"] = l.Height
		out["size"] = l.Size
	}
	return out
}

// ---------- HTTP ----------

// Handler serves the logo API.
type Handler struct{ Svc *Service }

func NewHandler(db *gorm.DB) *Handler { return &Handler{Svc: NewService(db)} }

// RegisterRoutes mounts PUT/DELETE /organization/logo on the authenticated
// /api/v1 group. (GET /organization/branding is served by the core
// organisation handler, which calls Service.Info.)
func (h *Handler) RegisterRoutes(auth *gin.RouterGroup) {
	auth.PUT("/organization/logo", h.Upload)
	auth.POST("/organization/logo", h.Upload)
	auth.DELETE("/organization/logo", h.Remove)
}

// RegisterPublic mounts GET /org-logo/:orgId on the public group.
func (h *Handler) RegisterPublic(public *gin.RouterGroup) {
	public.GET("/org-logo/:orgId", h.Serve)
}

func fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

func ctxString(c *gin.Context, key string) string {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// canManage: organisation admins (and super admins acting in an org).
func canManage(c *gin.Context) bool {
	r := ctxString(c, "user_role")
	return r == "admin" || r == "super_admin"
}

func (h *Handler) caller(c *gin.Context) (string, bool) {
	org := ctxString(c, "org_id")
	if org == "" {
		fail(c, http.StatusUnauthorized, "NO_ORGANIZATION", "Organization not found in token")
		return "", false
	}
	if !canManage(c) {
		fail(c, http.StatusForbidden, "INSUFFICIENT_PERMISSIONS", "Only organisation admins can change the company logo")
		return "", false
	}
	return org, true
}

// readUpload accepts multipart/form-data (field "file" or "logo") or JSON
// {"data_url": "data:image/png;base64,..."} (also "data" / "logo").
func readUpload(c *gin.Context) ([]byte, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxLogoBytes*2)
	mt, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if strings.HasPrefix(mt, "multipart/") {
		fh, err := c.FormFile("file")
		if err != nil {
			fh, err = c.FormFile("logo")
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, invalid("Logo must be 1 MB or smaller")
			}
			return nil, invalid("Choose an image file to upload")
		}
		if fh.Size > MaxLogoBytes {
			return nil, invalid("Logo must be 1 MB or smaller")
		}
		f, err := fh.Open()
		if err != nil {
			return nil, invalid("The file could not be read")
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, MaxLogoBytes+1))
		if err != nil {
			return nil, invalid("The file could not be read")
		}
		return b, nil
	}
	var in struct {
		DataURL string `json:"data_url"`
		Data    string `json:"data"`
		Logo    string `json:"logo"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, invalid("Logo must be 1 MB or smaller")
		}
		return nil, invalid("Send the logo as a file upload or a data URI")
	}
	s := in.DataURL
	if s == "" {
		s = in.Data
	}
	if s == "" {
		s = in.Logo
	}
	if strings.TrimSpace(s) == "" {
		return nil, invalid("Choose an image file to upload")
	}
	return DecodeDataURI(s)
}

// Upload handles PUT /organization/logo.
func (h *Handler) Upload(c *gin.Context) {
	org, ok := h.caller(c)
	if !ok {
		return
	}
	data, err := readUpload(c)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "1 MB") {
			status = http.StatusRequestEntityTooLarge
		}
		fail(c, status, "INVALID_LOGO", err.Error())
		return
	}
	if _, err := h.Svc.Save(org, ctxString(c, "user_id"), data); err != nil {
		if IsValidation(err) {
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "1 MB") {
				status = http.StatusRequestEntityTooLarge
			}
			fail(c, status, "INVALID_LOGO", err.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", "Could not save the logo")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.Svc.Info(c.Request, org), "message": "Logo updated"})
}

// Remove handles DELETE /organization/logo.
func (h *Handler) Remove(c *gin.Context) {
	org, ok := h.caller(c)
	if !ok {
		return
	}
	if err := h.Svc.Delete(org); err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", "Could not remove the logo")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.Svc.Info(c.Request, org), "message": "Logo removed"})
}

// etagMatches implements If-None-Match (a list, weak or strong, or *).
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		if p == "*" {
			return true
		}
		p = strings.TrimPrefix(p, "W/")
		if strings.Trim(p, `"`) == etag {
			return true
		}
	}
	return false
}

var extFor = map[string]string{TypePNG: "png", TypeJPEG: "jpg", TypeWebP: "webp", TypeSVG: "svg"}

// Serve handles GET /public/org-logo/:orgId — the logo image itself.
func (h *Handler) Serve(c *gin.Context) {
	org := c.Param("orgId")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cross-Origin-Resource-Policy", "cross-origin")
	// An SVG opened directly must never run script or load anything.
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
	if org == "" || len(org) > 255 {
		fail(c, http.StatusNotFound, "NOT_FOUND", "No logo")
		return
	}
	meta, err := h.Svc.Meta(org)
	if err != nil {
		fail(c, http.StatusInternalServerError, "SERVER_ERROR", "Could not load the logo")
		return
	}
	if meta == nil {
		c.Header("Cache-Control", "no-cache")
		fail(c, http.StatusNotFound, "NOT_FOUND", "This organisation has no logo")
		return
	}
	c.Header("ETag", `"`+meta.ETag+`"`)
	c.Header("Cache-Control", "public, max-age=300")
	c.Header("Last-Modified", meta.UpdatedAt.UTC().Format(http.TimeFormat))
	if inm := c.GetHeader("If-None-Match"); inm != "" && etagMatches(inm, meta.ETag) {
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return
	}
	var l OrgLogo
	if err := h.Svc.DB.Where("organization_id = ?", org).Take(&l).Error; err != nil {
		fail(c, http.StatusNotFound, "NOT_FOUND", "This organisation has no logo")
		return
	}
	c.Header("Content-Disposition", `inline; filename="logo.`+extFor[l.ContentType]+`"`)
	c.Data(http.StatusOK, l.ContentType, l.Data)
}

// ---------- backfill ----------

// Backfill moves each organisation's old invoicing logo (a data URI in
// billing_settings.logo_url) into org_logos when the organisation has no
// company logo yet, then clears the old field. It is idempotent: moved logos
// are cleared, and organisations that already have a logo are skipped.
// Invalid old logos are left in place (and logged).
func Backfill(db *gorm.DB) (moved int, err error) {
	if !db.Migrator().HasTable("billing_settings") || !db.Migrator().HasTable(&OrgLogo{}) {
		return 0, nil
	}
	var rows []struct {
		OrganizationID string
		LogoURL        string
	}
	if err := db.Raw(`SELECT b.organization_id, b.logo_url FROM billing_settings b
		WHERE b.logo_url LIKE 'data:%' AND NOT EXISTS (SELECT 1 FROM org_logos l WHERE l.organization_id = b.organization_id)`).
		Scan(&rows).Error; err != nil {
		return 0, err
	}
	svc := NewService(db)
	for _, r := range rows {
		data, derr := DecodeDataURI(r.LogoURL)
		if derr == nil {
			_, derr = svc.Save(r.OrganizationID, "backfill", data)
		}
		if derr != nil {
			if IsValidation(derr) {
				log.Printf("branding: org %s invoicing logo not migrated: %v", r.OrganizationID, derr)
				continue
			}
			return moved, derr
		}
		if err := db.Exec("UPDATE billing_settings SET logo_url = '' WHERE organization_id = ?", r.OrganizationID).Error; err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}
