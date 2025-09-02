package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

// BillingRecord represents a billing/invoice record
type BillingRecord struct {
	ID            string    `json:"id"`
	ParticipantID string    `json:"participant_id"`
	ShiftIDs      []string  `json:"shift_ids"`
	InvoiceNumber string    `json:"invoice_number"`
	Amount        float64   `json:"amount"`
	Status        string    `json:"status"` // draft, sent, paid, overdue
	IssueDate     time.Time `json:"issue_date"`
	DueDate       time.Time `json:"due_date"`
	PaidDate      *time.Time `json:"paid_date,omitempty"`
	Description   string    `json:"description"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (h *Handler) GetBilling(c *gin.Context) {
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

	// For MVP, generate mock billing data based on completed shifts
	var shifts []models.Shift
	query := h.DB.Joins("JOIN participants ON shifts.participant_id = participants.id").
		Where("participants.organization_id = ? AND shifts.status = ?", orgID, "completed")

	if err := query.Preload("Participant").Find(&shifts).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "DATABASE_ERROR",
				"message": "Failed to fetch billing data",
			},
		})
		return
	}

	// Generate mock billing records
	billingRecords := []BillingRecord{}
	participantBilling := make(map[string]*BillingRecord)

	for _, shift := range shifts {
		if _, exists := participantBilling[shift.ParticipantID]; !exists {
			billingRecord := &BillingRecord{
				ID:            "bill_" + shift.ParticipantID,
				ParticipantID: shift.ParticipantID,
				ShiftIDs:      []string{},
				InvoiceNumber: "INV-2025-" + shift.ParticipantID[0:8],
				Amount:        0,
				Status:        "draft",
				IssueDate:     time.Now(),
				DueDate:       time.Now().Add(30 * 24 * time.Hour),
				Description:   "Care services for " + shift.Participant.FirstName + " " + shift.Participant.LastName,
				CreatedAt:     time.Now(),
				UpdatedAt:     time.Now(),
			}
			participantBilling[shift.ParticipantID] = billingRecord
		}

		// Calculate shift cost
		hours := shift.EndTime.Sub(shift.StartTime).Hours()
		cost := hours * shift.HourlyRate
		
		participantBilling[shift.ParticipantID].Amount += cost
		participantBilling[shift.ParticipantID].ShiftIDs = append(
			participantBilling[shift.ParticipantID].ShiftIDs, 
			shift.ID,
		)
	}

	// Convert map to slice
	for _, billing := range participantBilling {
		billingRecords = append(billingRecords, *billing)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"billing": billingRecords,
			"pagination": gin.H{
				"page":        1,
				"limit":       len(billingRecords),
				"total":       len(billingRecords),
				"total_pages": 1,
			},
		},
	})
}

func (h *Handler) GetBillingRecord(c *gin.Context) {
	billingID := c.Param("id")
	
	// For MVP, return mock data
	billingRecord := BillingRecord{
		ID:            billingID,
		ParticipantID: "participant_1",
		InvoiceNumber: "INV-2025-001",
		Amount:        450.00,
		Status:        "sent",
		IssueDate:     time.Now().Add(-7 * 24 * time.Hour),
		DueDate:       time.Now().Add(23 * 24 * time.Hour),
		Description:   "Care services",
		CreatedAt:     time.Now().Add(-7 * 24 * time.Hour),
		UpdatedAt:     time.Now(),
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    billingRecord,
	})
}

type GenerateInvoiceRequest struct {
	ParticipantID string   `json:"participant_id" binding:"required"`
	ShiftIDs      []string `json:"shift_ids" binding:"required"`
	DueDate       string   `json:"due_date"`
	Description   string   `json:"description"`
}

func (h *Handler) GenerateInvoice(c *gin.Context) {
	var req GenerateInvoiceRequest
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

	// For MVP, return mock generated invoice
	invoiceRecord := BillingRecord{
		ID:            "bill_" + req.ParticipantID + "_" + time.Now().Format("20060102"),
		ParticipantID: req.ParticipantID,
		ShiftIDs:      req.ShiftIDs,
		InvoiceNumber: "INV-2025-" + time.Now().Format("001"),
		Amount:        375.50, // Mock calculation
		Status:        "draft",
		IssueDate:     time.Now(),
		DueDate:       time.Now().Add(30 * 24 * time.Hour),
		Description:   req.Description,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	c.JSON(http.StatusCreated, gin.H{
		"success": true,
		"data":    invoiceRecord,
		"message": "Invoice generated successfully",
	})
}

type PaymentRequest struct {
	Amount      float64   `json:"amount" binding:"required,gt=0"`
	PaymentDate time.Time `json:"payment_date"`
	Method      string    `json:"method"`
	Reference   string    `json:"reference"`
}

func (h *Handler) MarkAsPaid(c *gin.Context) {
	billingID := c.Param("id")
	
	var req PaymentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": gin.H{
				"code":    "VALIDATION_ERROR",
				"message": "Invalid payment data",
				"details": err.Error(),
			},
		})
		return
	}

	// For MVP, return updated record
	now := time.Now()
	billingRecord := BillingRecord{
		ID:            billingID,
		ParticipantID: "participant_1",
		InvoiceNumber: "INV-2025-001",
		Amount:        req.Amount,
		Status:        "paid",
		IssueDate:     now.Add(-7 * 24 * time.Hour),
		DueDate:       now.Add(23 * 24 * time.Hour),
		PaidDate:      &now,
		Description:   "Care services",
		CreatedAt:     now.Add(-7 * 24 * time.Hour),
		UpdatedAt:     now,
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    billingRecord,
		"message": "Payment recorded successfully",
	})
}

func (h *Handler) DownloadInvoice(c *gin.Context) {
	billingID := c.Param("id")
	
	// For MVP, return mock PDF response
	c.Header("Content-Type", "application/pdf")
	c.Header("Content-Disposition", "attachment; filename=invoice_"+billingID+".pdf")
	
	// Return mock PDF content (in real implementation, generate actual PDF)
	mockPDFContent := "%PDF-1.4\n1 0 obj\n<<\n/Type /Catalog\n/Pages 2 0 R\n>>\nendobj\n\n2 0 obj\n<<\n/Type /Pages\n/Kids [3 0 R]\n/Count 1\n>>\nendobj\n\n3 0 obj\n<<\n/Type /Page\n/Parent 2 0 R\n/MediaBox [0 0 612 792]\n>>\nendobj\n\nxref\n0 4\n0000000000 65535 f \n0000000010 00000 n \n0000000053 00000 n \n0000000104 00000 n \ntrailer\n<<\n/Size 4\n/Root 1 0 R\n>>\nstartxref\n164\n%%EOF"
	
	c.String(http.StatusOK, mockPDFContent)
}