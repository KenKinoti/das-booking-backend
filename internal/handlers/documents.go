package handlers

import (
	"fmt"
	"io"
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

func (h *Handler) GetDocuments(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Parse query parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	participantID := c.Query("participant_id")
	category := c.Query("category")
	fileType := c.Query("file_type")
	isActive := c.Query("is_active")
	search := c.Query("search")

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 10
	}

	offset := (page - 1) * limit

	// Build query with organization access control
	query := h.DB.Joins("LEFT JOIN participants ON documents.participant_id = participants.id").
		Where("(documents.participant_id IS NULL AND documents.uploaded_by IN (SELECT id FROM users WHERE organization_id = ?)) OR participants.organization_id = ?", orgID, orgID)

	if participantID != "" {
		query = query.Where("documents.participant_id = ?", participantID)
	}

	if category != "" {
		query = query.Where("documents.category = ?", category)
	}

	if fileType != "" {
		query = query.Where("documents.file_type = ?", fileType)
	}

	if isActive != "" {
		activeFilter, _ := strconv.ParseBool(isActive)
		query = query.Where("documents.is_active = ?", activeFilter)
	}

	if search != "" {
		searchTerm := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(documents.title) LIKE ? OR LOWER(documents.description) LIKE ? OR LOWER(documents.original_filename) LIKE ?", searchTerm, searchTerm, searchTerm)
	}

	// Get total count
	var total int64
	query.Model(&models.Document{}).Count(&total)

	// Get documents with related data
	var documents []models.Document
	if err := query.Preload("Participant").Preload("UploadedByUser").
		Limit(limit).Offset(offset).Order("documents.created_at DESC").
		Find(&documents).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch documents",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"documents": documents,
			"pagination": gin.H{
				"page":        page,
				"limit":       limit,
				"total":       total,
				"total_pages": (total + int64(limit) - 1) / int64(limit),
			},
		},
	})
}

func (h *Handler) GetDocument(c *gin.Context) {
	documentID := c.Param("id")
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Find document with access control
	var document models.Document
	query := h.DB.Joins("LEFT JOIN participants ON documents.participant_id = participants.id").
		Joins("JOIN users ON documents.uploaded_by = users.id").
		Where("documents.id = ? AND ((documents.participant_id IS NULL AND users.organization_id = ?) OR participants.organization_id = ?)", documentID, orgID, orgID)

	if err := query.Preload("Participant").Preload("UploadedByUser").First(&document).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "DOCUMENT_NOT_FOUND",
					"message": "Document not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch document",
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    document,
	})
}

func (h *Handler) UploadDocument(c *gin.Context) {
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	userID, _ := c.Get("user_id")

	// Parse multipart form
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil { // 32 MB limit
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "FORM_PARSE_ERROR",
				"message": "Failed to parse multipart form",
			},
		})
		return
	}

	// Get form fields
	title := c.PostForm("title")
	description := c.PostForm("description")
	category := c.PostForm("category")
	participantID := c.PostForm("participant_id")
	expiryDateStr := c.PostForm("expiry_date")

	if title == "" || category == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "VALIDATION_ERROR",
				"message": "Title and category are required",
			},
		})
		return
	}

	// Validate participant if provided
	if participantID != "" {
		var participant models.Participant
		if err := h.DB.Where("id = ? AND organization_id = ?", participantID, orgID).First(&participant).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "INVALID_PARTICIPANT",
					"message": "Participant not found",
				},
			})
			return
		}
	}

	// Parse expiry date if provided
	var expiryDate *time.Time
	if expiryDateStr != "" {
		if parsedDate, err := time.Parse("2006-01-02", expiryDateStr); err == nil {
			expiryDate = &parsedDate
		} else {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "INVALID_DATE",
					"message": "Invalid expiry date format (YYYY-MM-DD expected)",
				},
			})
			return
		}
	}

	// Get uploaded file
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "FILE_REQUIRED",
				"message": "File upload is required",
			},
		})
		return
	}
	defer file.Close()

	// Validate file size (10MB limit)
	if header.Size > 10<<20 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "FILE_TOO_LARGE",
				"message": "File size exceeds 10MB limit",
			},
		})
		return
	}

	// Create uploads directory if it doesn't exist
	uploadsDir := "uploads/documents"
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DIRECTORY_ERROR",
				"message": "Failed to create uploads directory",
			},
		})
		return
	}

	// Generate unique filename
	fileExt := filepath.Ext(header.Filename)
	filename := fmt.Sprintf("%s_%s%s", uuid.New().String(), time.Now().Format("20060102_150405"), fileExt)
	filePath := filepath.Join(uploadsDir, filename)

	// Save file
	dst, err := os.Create(filePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "FILE_SAVE_ERROR",
				"message": "Failed to save file",
			},
		})
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, file); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "FILE_SAVE_ERROR",
				"message": "Failed to save file",
			},
		})
		return
	}

	// Create document record
	document := models.Document{
		Title:            title,
		Description:      description,
		Category:         category,
		Filename:         filename,
		OriginalFilename: header.Filename,
		FileType:         header.Header.Get("Content-Type"),
		FileSize:         header.Size,
		FilePath:         filePath,
		URL:              fmt.Sprintf("/api/v1/documents/%s/download", ""), // Will be updated after ID is generated
		UploadedBy:       userID.(string),
		IsActive:         true,
		ExpiryDate:       expiryDate,
	}

	if participantID != "" {
		document.ParticipantID = &participantID
	}

	if err := h.DB.Create(&document).Error; err != nil {
		// Remove uploaded file if database insertion fails
		os.Remove(filePath)
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to save document record",
			},
		})
		return
	}

	// Update URL with document ID
	document.URL = fmt.Sprintf("/api/v1/documents/%s/download", document.ID)
	h.DB.Save(&document)

	// Fetch document with related data
	h.DB.Preload("Participant").Preload("UploadedByUser").First(&document, "id = ?", document.ID)

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    document,
		"message": "Document uploaded successfully",
	})
}

type UpdateDocumentRequest struct {
	Title       *string    `json:"title,omitempty"`
	Description *string    `json:"description,omitempty"`
	Category    *string    `json:"category,omitempty"`
	IsActive    *bool      `json:"is_active,omitempty"`
	ExpiryDate  *time.Time `json:"expiry_date,omitempty"`
}

func (h *Handler) UpdateDocument(c *gin.Context) {
	documentID := c.Param("id")
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	var req UpdateDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "VALIDATION_ERROR",
				"message": "Invalid request parameters",
				"details": err.Error(),
			},
		})
		return
	}

	// Find document with access control
	var document models.Document
	query := h.DB.Joins("LEFT JOIN participants ON documents.participant_id = participants.id").
		Joins("JOIN users ON documents.uploaded_by = users.id").
		Where("documents.id = ? AND ((documents.participant_id IS NULL AND users.organization_id = ?) OR participants.organization_id = ?)", documentID, orgID, orgID)

	if err := query.First(&document).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "DOCUMENT_NOT_FOUND",
					"message": "Document not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch document",
			},
		})
		return
	}

	// Update fields
	updates := make(map[string]interface{})
	if req.Title != nil {
		updates["title"] = *req.Title
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Category != nil {
		updates["category"] = *req.Category
	}
	if req.IsActive != nil {
		updates["is_active"] = *req.IsActive
	}
	if req.ExpiryDate != nil {
		updates["expiry_date"] = *req.ExpiryDate
	}

	if err := h.DB.Model(&document).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to update document",
			},
		})
		return
	}

	// Fetch updated document
	h.DB.Preload("Participant").Preload("UploadedByUser").First(&document, "id = ?", documentID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    document,
		"message": "Document updated successfully",
	})
}

func (h *Handler) DeleteDocument(c *gin.Context) {
	documentID := c.Param("id")
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Find document with access control
	var document models.Document
	query := h.DB.Joins("LEFT JOIN participants ON documents.participant_id = participants.id").
		Joins("JOIN users ON documents.uploaded_by = users.id").
		Where("documents.id = ? AND ((documents.participant_id IS NULL AND users.organization_id = ?) OR participants.organization_id = ?)", documentID, orgID, orgID)

	if err := query.First(&document).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "DOCUMENT_NOT_FOUND",
					"message": "Document not found",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch document",
			},
		})
		return
	}

	// Soft delete document
	if err := h.DB.Delete(&document).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete document",
			},
		})
		return
	}

	// Note: We keep the physical file for data integrity
	// In a production system, you might want to move files to a "trash" folder
	// or implement a cleanup job that removes files after a certain period

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Document deleted successfully",
	})
}

func (h *Handler) DownloadDocument(c *gin.Context) {
	documentID := c.Param("id")
	orgID, exists := c.Get("org_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "UNAUTHORIZED",
				"message": "Organization not found in context",
			},
		})
		return
	}

	// Find document with access control
	var document models.Document
	query := h.DB.Joins("LEFT JOIN participants ON documents.participant_id = participants.id").
		Joins("JOIN users ON documents.uploaded_by = users.id").
		Where("documents.id = ? AND documents.is_active = ? AND ((documents.participant_id IS NULL AND users.organization_id = ?) OR participants.organization_id = ?)", documentID, true, orgID, orgID)

	if err := query.First(&document).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"error": gin.H{
					"code":    "DOCUMENT_NOT_FOUND",
					"message": "Document not found or inactive",
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch document",
			},
		})
		return
	}

	// Check if file exists
	if _, err := os.Stat(document.FilePath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "FILE_NOT_FOUND",
				"message": "Physical file not found",
			},
		})
		return
	}

	// Set appropriate headers
	c.Header("Content-Type", document.FileType)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", document.OriginalFilename))
	c.Header("Content-Length", strconv.FormatInt(document.FileSize, 10))

	// Serve file
	c.File(document.FilePath)
}
