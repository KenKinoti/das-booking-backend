package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"message": "AGO CRM API is running",
		"version": "1.0.0",
	})
}
