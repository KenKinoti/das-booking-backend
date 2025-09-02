package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupTestHandler() (*Handler, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	
	// Setup test database
	db, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	models.MigrateDB(db)
	
	// Setup test configuration
	cfg := &config.Config{
		JWTSecret:          "test-secret-key",
		JWTExpiry:          24 * time.Hour,
		RefreshTokenExpiry: 7 * 24 * time.Hour,
	}
	
	// Create handler
	handler := NewHandler(db, cfg)
	
	// Setup router
	router := gin.New()
	handler.SetupRoutes(router)
	
	// Create test organization
	org := models.Organization{
		ID:   "test-org",
		Name: "Test Org",
	}
	db.Create(&org)
	
	// Create test user
	user := models.User{
		ID:             "test-user",
		Email:          "test@example.com",
		PasswordHash:   "$2a$10$92IXUNpkjO0rOQ5byMi.Ye4oKoEa3Ro9llC/.og/at2.uheWG/igi", // "password"
		FirstName:      "Test",
		LastName:       "User",
		Role:           "admin",
		OrganizationID: org.ID,
		IsActive:       true,
	}
	db.Create(&user)
	
	return handler, router
}

func TestLogin(t *testing.T) {
	_, router := setupTestHandler()

	t.Run("Valid login", func(t *testing.T) {
		loginData := map[string]string{
			"email":    "test@example.com",
			"password": "password",
		}
		
		body, _ := json.Marshal(loginData)
		req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		
		assert.Equal(t, http.StatusOK, w.Code)
		
		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))
		assert.Contains(t, response, "data")
	})

	t.Run("Invalid credentials", func(t *testing.T) {
		loginData := map[string]string{
			"email":    "test@example.com",
			"password": "wrongpassword",
		}
		
		body, _ := json.Marshal(loginData)
		req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		
		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.False(t, response["success"].(bool))
	})

	t.Run("Invalid JSON", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewBuffer([]byte("invalid json")))
		req.Header.Set("Content-Type", "application/json")
		
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("Missing fields", func(t *testing.T) {
		loginData := map[string]string{
			"email": "test@example.com",
			// missing password
		}
		
		body, _ := json.Marshal(loginData)
		req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}