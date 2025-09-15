package crm

import (
	"net/http"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// Lead handlers
func (h *Handler) GetLeads(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	leads, err := h.service.GetLeads(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": leads})
}

func (h *Handler) CreateLead(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var lead Lead
	if err := c.ShouldBindJSON(&lead); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	lead.OrganizationID = orgID
	if err := h.service.CreateLead(&lead); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": lead})
}

func (h *Handler) GetLead(c *gin.Context) {
	id := c.Param("id")
	lead, err := h.service.GetLead(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Lead not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": lead})
}

func (h *Handler) UpdateLead(c *gin.Context) {
	id := c.Param("id")
	lead, err := h.service.GetLead(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Lead not found"})
		return
	}

	if err := c.ShouldBindJSON(lead); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.service.UpdateLead(lead); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": lead})
}

func (h *Handler) DeleteLead(c *gin.Context) {
	id := c.Param("id")
	if err := h.service.DeleteLead(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Lead deleted"})
}

// Customer handlers
func (h *Handler) GetCustomers(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	customers, err := h.service.GetCustomers(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": customers})
}

func (h *Handler) CreateCustomer(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var customer CRMCustomer
	if err := c.ShouldBindJSON(&customer); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	customer.OrganizationID = orgID
	if err := h.service.CreateCustomer(&customer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": customer})
}

// Opportunity handlers
func (h *Handler) GetOpportunities(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	opportunities, err := h.service.GetOpportunities(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": opportunities})
}

func (h *Handler) CreateOpportunity(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var opportunity Opportunity
	if err := c.ShouldBindJSON(&opportunity); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	opportunity.OrganizationID = orgID
	if err := h.service.CreateOpportunity(&opportunity); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": opportunity})
}

func (h *Handler) GetDashboard(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	dashboard, err := h.service.GetDashboard(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": dashboard})
}

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	// Lead routes
	r.GET("/leads", h.GetLeads)
	r.POST("/leads", h.CreateLead)
	r.GET("/leads/:id", h.GetLead)
	r.PUT("/leads/:id", h.UpdateLead)
	r.DELETE("/leads/:id", h.DeleteLead)

	// Customer routes
	r.GET("/customers", h.GetCustomers)
	r.POST("/customers", h.CreateCustomer)

	// Opportunity routes
	r.GET("/opportunities", h.GetOpportunities)
	r.POST("/opportunities", h.CreateOpportunity)

	// Dashboard
	r.GET("/dashboard", h.GetDashboard)
}