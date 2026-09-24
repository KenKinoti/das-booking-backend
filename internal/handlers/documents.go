package handlers

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"gorm.io/gorm"
)

// Document storage
//
// Cloud Run containers have an ephemeral, per-instance filesystem, so files
// written to local disk disappear on redeploys and are invisible to other
// instances. Uploaded document bytes are therefore stored in Postgres in the
// document_blobs table (one row per document, max 10 MB per file). Documents
// uploaded by older versions still have a FilePath on disk; downloads fall
// back to that path when no blob row exists.

// MaxDocumentSize is the largest file accepted by UploadDocument.
const MaxDocumentSize = 10 << 20

// DocumentBlob holds the bytes of an uploaded document.
type DocumentBlob struct {
	DocumentID     string    `gorm:"type:varchar(255);primaryKey"`
	OrganizationID string    `gorm:"type:varchar(255);index;not null"`
	Data           []byte    `gorm:"type:bytea;not null"`
	CreatedAt      time.Time `json:"created_at"`
}

func (DocumentBlob) TableName() string { return "document_blobs" }

func docErr(c *gin.Context, status int, code, msg string) {
	c.JSON(status, gin.H{"success": false, "error": gin.H{"code": code, "message": msg}})
}

// orgDocuments scopes documents to the caller's organisation (through the
// uploader, or through the participant for participant documents).
func (h *Handler) orgDocuments(orgID string) *gorm.DB {
	return h.DB.Model(&models.Document{}).
		Where("(documents.uploaded_by IN (SELECT id FROM users WHERE organization_id = ?) OR documents.participant_id IN (SELECT id FROM participants WHERE organization_id = ?))", orgID, orgID)
}

func (h *Handler) loadDocument(c *gin.Context) (*models.Document, bool) {
	var doc models.Document
	if err := h.orgDocuments(c.GetString("org_id")).Where("documents.id = ?", c.Param("id")).First(&doc).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			docErr(c, http.StatusNotFound, "DOCUMENT_NOT_FOUND", "Document not found")
		} else {
			docErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to fetch document")
		}
		return nil, false
	}
	return &doc, true
}

func (h *Handler) reloadDocument(id string) models.Document {
	var doc models.Document
	h.DB.Preload("Participant").Preload("UploadedByUser").First(&doc, "id = ?", id)
	return doc
}

// GetDocuments lists documents. Query: page, limit, category, file_type
// (pdf|image|word|sheet|other), is_active, search, expiring (days), sort.
func (h *Handler) GetDocuments(c *gin.Context) {
	orgID := c.GetString("org_id")
	if orgID == "" {
		docErr(c, http.StatusUnauthorized, "UNAUTHORIZED", "Organization not found in context")
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "25"))
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 200 {
		limit = 25
	}

	query := h.orgDocuments(orgID)
	if v := c.Query("participant_id"); v != "" {
		query = query.Where("documents.participant_id = ?", v)
	}
	if v := c.Query("category"); v != "" {
		query = query.Where("documents.category = ?", v)
	}
	switch c.Query("file_type") {
	case "pdf":
		query = query.Where("documents.file_type ILIKE ?", "%pdf%")
	case "image":
		query = query.Where("documents.file_type ILIKE ?", "image/%")
	case "word":
		query = query.Where("(documents.file_type ILIKE ? OR documents.file_type ILIKE ? OR documents.file_type ILIKE ?)", "%word%", "%opendocument.text%", "text/%")
	case "sheet":
		query = query.Where("(documents.file_type ILIKE ? OR documents.file_type ILIKE ? OR documents.file_type ILIKE ?)", "%sheet%", "%excel%", "%csv%")
	}
	if v := c.Query("is_active"); v != "" {
		b, _ := strconv.ParseBool(v)
		query = query.Where("documents.is_active = ?", b)
	}
	if v := c.Query("expiring"); v != "" {
		days, _ := strconv.Atoi(v)
		if days <= 0 {
			days = 30
		}
		query = query.Where("documents.expiry_date IS NOT NULL AND documents.expiry_date <= ?", time.Now().AddDate(0, 0, days))
	}
	if search := strings.TrimSpace(c.Query("search")); search != "" {
		term := "%" + strings.ToLower(search) + "%"
		query = query.Where("(LOWER(documents.title) LIKE ? OR LOWER(documents.description) LIKE ? OR LOWER(documents.original_filename) LIKE ?)", term, term, term)
	}

	var total int64
	query.Session(&gorm.Session{}).Count(&total)

	order := "documents.created_at DESC"
	switch c.Query("sort") {
	case "name":
		order = "LOWER(documents.title) ASC"
	case "size":
		order = "documents.file_size DESC"
	case "oldest":
		order = "documents.created_at ASC"
	case "expiry":
		order = "documents.expiry_date ASC NULLS LAST"
	}

	var documents []models.Document
	if err := query.Preload("Participant").Preload("UploadedByUser").
		Limit(limit).Offset((page - 1) * limit).Order(order).
		Find(&documents).Error; err != nil {
		docErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to fetch documents")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"documents": documents,
			"pagination": gin.H{
				"page": page, "limit": limit, "total": total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

// GetDocumentStats returns counts and storage used for the organisation.
func (h *Handler) GetDocumentStats(c *gin.Context) {
	orgID := c.GetString("org_id")
	type catRow struct {
		Category string `json:"category"`
		Count    int64  `json:"count"`
		Size     int64  `json:"size"`
	}
	var cats []catRow
	h.orgDocuments(orgID).Select("documents.category AS category, COUNT(*) AS count, COALESCE(SUM(documents.file_size), 0) AS size").
		Group("documents.category").Order("count DESC").Scan(&cats)
	var total, size int64
	for _, r := range cats {
		total += r.Count
		size += r.Size
	}
	var expiring, expired, recent int64
	now := time.Now()
	h.orgDocuments(orgID).Where("documents.expiry_date IS NOT NULL AND documents.expiry_date > ? AND documents.expiry_date <= ?", now, now.AddDate(0, 0, 30)).Count(&expiring)
	h.orgDocuments(orgID).Where("documents.expiry_date IS NOT NULL AND documents.expiry_date <= ?", now).Count(&expired)
	h.orgDocuments(orgID).Where("documents.created_at >= ?", now.AddDate(0, 0, -7)).Count(&recent)
	if cats == nil {
		cats = []catRow{}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"total": total, "total_size": size, "expiring_soon": expiring, "expired": expired,
		"uploaded_this_week": recent, "categories": cats, "max_file_size": MaxDocumentSize,
	}})
}

func (h *Handler) GetDocument(c *gin.Context) {
	doc, ok := h.loadDocument(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.reloadDocument(doc.ID)})
}

func (h *Handler) UploadDocument(c *gin.Context) {
	orgID := c.GetString("org_id")
	userID := c.GetString("user_id")
	if orgID == "" || userID == "" {
		docErr(c, http.StatusUnauthorized, "UNAUTHORIZED", "Organization not found in context")
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxDocumentSize+(1<<20))
	if err := c.Request.ParseMultipartForm(MaxDocumentSize + (1 << 20)); err != nil {
		docErr(c, http.StatusBadRequest, "FILE_TOO_LARGE", "The file is too large (maximum 10 MB)")
		return
	}

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		docErr(c, http.StatusBadRequest, "FILE_REQUIRED", "Choose a file to upload")
		return
	}
	defer file.Close()
	if header.Size > MaxDocumentSize {
		docErr(c, http.StatusBadRequest, "FILE_TOO_LARGE", "The file is too large (maximum 10 MB)")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxDocumentSize+1))
	if err != nil {
		docErr(c, http.StatusBadRequest, "UPLOAD_FAILED", "Could not read the uploaded file")
		return
	}
	if len(data) > MaxDocumentSize {
		docErr(c, http.StatusBadRequest, "FILE_TOO_LARGE", "The file is too large (maximum 10 MB)")
		return
	}
	if len(data) == 0 {
		docErr(c, http.StatusBadRequest, "EMPTY_FILE", "The file is empty")
		return
	}

	title := strings.TrimSpace(c.PostForm("title"))
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(header.Filename), filepath.Ext(header.Filename))
	}
	category := strings.TrimSpace(c.PostForm("category"))
	if category == "" {
		category = "general"
	}
	participantID := c.PostForm("participant_id")
	if participantID != "" {
		var n int64
		h.DB.Model(&models.Participant{}).Where("id = ? AND organization_id = ?", participantID, orgID).Count(&n)
		if n == 0 {
			docErr(c, http.StatusBadRequest, "INVALID_PARTICIPANT", "Participant not found")
			return
		}
	}
	var expiry *time.Time
	if v := strings.TrimSpace(c.PostForm("expiry_date")); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			docErr(c, http.StatusBadRequest, "INVALID_DATE", "Expiry date must be YYYY-MM-DD")
			return
		}
		expiry = &t
	}

	contentType := header.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(header.Filename))); t != "" {
			contentType = t
		} else {
			contentType = http.DetectContentType(data)
		}
	}

	id := uuid.NewString()
	doc := models.Document{
		ID:               id,
		Title:            title,
		Description:      strings.TrimSpace(c.PostForm("description")),
		Category:         category,
		Filename:         id + strings.ToLower(filepath.Ext(header.Filename)),
		OriginalFilename: filepath.Base(header.Filename),
		FileType:         contentType,
		FileSize:         int64(len(data)),
		FilePath:         "db:" + id,
		URL:              fmt.Sprintf("/api/v1/documents/%s/download", id),
		UploadedBy:       userID,
		IsActive:         true,
		ExpiryDate:       expiry,
	}
	if participantID != "" {
		doc.ParticipantID = &participantID
	}

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Participant", "UploadedByUser").Create(&doc).Error; err != nil {
			return err
		}
		return tx.Create(&DocumentBlob{DocumentID: id, OrganizationID: orgID, Data: data}).Error
	})
	if err != nil {
		docErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to save the document")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": h.reloadDocument(id)})
}

type UpdateDocumentRequest struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Category    *string `json:"category,omitempty"`
	IsActive    *bool   `json:"is_active,omitempty"`
	// ExpiryDate accepts "YYYY-MM-DD", an RFC3339 time, or "" to clear.
	ExpiryDate *string `json:"expiry_date,omitempty"`
}

func (h *Handler) UpdateDocument(c *gin.Context) {
	var req UpdateDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		docErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request: "+err.Error())
		return
	}
	doc, ok := h.loadDocument(c)
	if !ok {
		return
	}
	updates := map[string]interface{}{}
	if req.Title != nil {
		if strings.TrimSpace(*req.Title) == "" {
			docErr(c, http.StatusBadRequest, "VALIDATION_ERROR", "Title is required")
			return
		}
		updates["title"] = strings.TrimSpace(*req.Title)
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Category != nil && strings.TrimSpace(*req.Category) != "" {
		updates["category"] = strings.TrimSpace(*req.Category)
	}
	if req.IsActive != nil {
		updates["is_active"] = *req.IsActive
	}
	if req.ExpiryDate != nil {
		v := strings.TrimSpace(*req.ExpiryDate)
		if v == "" {
			updates["expiry_date"] = nil
		} else {
			if len(v) > 10 {
				v = v[:10]
			}
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				docErr(c, http.StatusBadRequest, "INVALID_DATE", "Expiry date must be YYYY-MM-DD")
				return
			}
			updates["expiry_date"] = t
		}
	}
	if len(updates) > 0 {
		if err := h.DB.Model(&models.Document{}).Where("id = ?", doc.ID).Updates(updates).Error; err != nil {
			docErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to update document")
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": h.reloadDocument(doc.ID)})
}

// DeleteDocument deletes the record and its stored bytes.
func (h *Handler) DeleteDocument(c *gin.Context) {
	doc, ok := h.loadDocument(c)
	if !ok {
		return
	}
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("document_id = ?", doc.ID).Delete(&DocumentBlob{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.Document{}, "id = ?", doc.ID).Error
	})
	if err != nil {
		docErr(c, http.StatusInternalServerError, "DATABASE_ERROR", "Failed to delete document")
		return
	}
	if doc.FilePath != "" && !strings.HasPrefix(doc.FilePath, "db:") {
		os.Remove(doc.FilePath)
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Document deleted"})
}

// DownloadDocument streams the file. ?inline=1 lets browsers preview it.
func (h *Handler) DownloadDocument(c *gin.Context) {
	doc, ok := h.loadDocument(c)
	if !ok {
		return
	}
	disposition := "attachment"
	if c.Query("inline") == "1" {
		disposition = "inline"
	}
	name := strings.NewReplacer(`"`, "", "\n", "", "\r", "").Replace(doc.OriginalFilename)
	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, name))
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")

	var blob DocumentBlob
	if err := h.DB.Where("document_id = ?", doc.ID).First(&blob).Error; err == nil {
		c.Data(http.StatusOK, doc.FileType, blob.Data)
		return
	}
	if doc.FilePath != "" && !strings.HasPrefix(doc.FilePath, "db:") {
		if _, err := os.Stat(doc.FilePath); err == nil {
			c.Header("Content-Type", doc.FileType)
			c.File(doc.FilePath)
			return
		}
	}
	docErr(c, http.StatusNotFound, "FILE_NOT_FOUND", "The file for this document is no longer available")
}
