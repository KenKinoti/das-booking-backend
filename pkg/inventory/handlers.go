package inventory

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

func (h *Handler) GetProducts(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	products, err := h.service.GetProducts(orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": products})
}

func (h *Handler) CreateProduct(c *gin.Context) {
	orgID := c.GetString("organization_id")
	if orgID == "" {
		orgID = "default-org"
	}

	var product Product
	if err := c.ShouldBindJSON(&product); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	product.OrganizationID = orgID
	if err := h.service.CreateProduct(&product); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"success": true, "data": product})
}

func (h *Handler) GetProduct(c *gin.Context) {
	id := c.Param("id")
	product, err := h.service.GetProduct(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": product})
}

func (h *Handler) UpdateProduct(c *gin.Context) {
	id := c.Param("id")
	product, err := h.service.GetProduct(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}

	if err := c.ShouldBindJSON(product); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.service.UpdateProduct(product); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": product})
}

func (h *Handler) DeleteProduct(c *gin.Context) {
	id := c.Param("id")
	if err := h.service.DeleteProduct(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Product deleted"})
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
	r.GET("/products", h.GetProducts)
	r.POST("/products", h.CreateProduct)
	r.GET("/products/:id", h.GetProduct)
	r.PUT("/products/:id", h.UpdateProduct)
	r.DELETE("/products/:id", h.DeleteProduct)
	r.GET("/dashboard", h.GetDashboard)
}