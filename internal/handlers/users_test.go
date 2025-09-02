package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/stretchr/testify/assert"
)

func TestCreateUser(t *testing.T) {
	handler, router := setupTestHandler()

	t.Run("Valid user creation by admin", func(t *testing.T) {
		userData := map[string]interface{}{
			"email":      "newuser@example.com",
			"password":   "password123",
			"first_name": "New",
			"last_name":  "User",
			"role":       "care_worker",
			"phone":      "+61412345678",
		}

		body, _ := json.Marshal(userData)
		req := httptest.NewRequest("POST", "/api/v1/users", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusCreated, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))
		assert.Contains(t, response, "data")
	})

	t.Run("Invalid email format", func(t *testing.T) {
		userData := map[string]interface{}{
			"email":      "invalid-email",
			"password":   "password123",
			"first_name": "New",
			"last_name":  "User",
			"role":       "care_worker",
		}

		body, _ := json.Marshal(userData)
		req := httptest.NewRequest("POST", "/api/v1/users", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("Duplicate email", func(t *testing.T) {
		userData := map[string]interface{}{
			"email":      "test@example.com", // Same as test user
			"password":   "password123",
			"first_name": "Duplicate",
			"last_name":  "User",
			"role":       "care_worker",
		}

		body, _ := json.Marshal(userData)
		req := httptest.NewRequest("POST", "/api/v1/users", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("Unauthorized access", func(t *testing.T) {
		userData := map[string]interface{}{
			"email":    "newuser@example.com",
			"password": "password123",
			"role":     "care_worker",
		}

		body, _ := json.Marshal(userData)
		req := httptest.NewRequest("POST", "/api/v1/users", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestGetUsers(t *testing.T) {
	_, router := setupTestHandler()

	t.Run("Get users with pagination", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/users?page=1&limit=10", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(nil))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))
		assert.Contains(t, response, "data")
	})

	t.Run("Filter by role", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/users?role=admin", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(nil))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))
	})
}

func TestGetCurrentUser(t *testing.T) {
	_, router := setupTestHandler()

	t.Run("Get current user info", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/users/me", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(nil))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))

		data := response["data"].(map[string]interface{})
		assert.Equal(t, "test@example.com", data["email"])
		assert.Equal(t, "Test", data["first_name"])
	})

	t.Run("Unauthorized access", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/users/me", nil)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestUpdateUser(t *testing.T) {
	handler, router := setupTestHandler()

	// Create a test user to update
	testUser := models.User{
		ID:             "update-test-user",
		Email:          "updateme@example.com",
		PasswordHash:   "$2a$10$92IXUNpkjO0rOQ5byMi.Ye4oKoEa3Ro9llC/.og/at2.uheWG/igi",
		FirstName:      "Update",
		LastName:       "Me",
		Role:           "care_worker",
		OrganizationID: "test-org",
		IsActive:       true,
	}
	handler.DB.Create(&testUser)

	t.Run("Valid user update", func(t *testing.T) {
		updateData := map[string]interface{}{
			"first_name": "Updated",
			"last_name":  "Name",
			"phone":      "+61987654321",
		}

		body, _ := json.Marshal(updateData)
		req := httptest.NewRequest("PUT", "/api/v1/users/update-test-user", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))
	})

	t.Run("User not found", func(t *testing.T) {
		updateData := map[string]interface{}{
			"first_name": "Updated",
		}

		body, _ := json.Marshal(updateData)
		req := httptest.NewRequest("PUT", "/api/v1/users/nonexistent", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestDeleteUser(t *testing.T) {
	handler, router := setupTestHandler()

	// Create a test user to delete
	testUser := models.User{
		ID:             "delete-test-user",
		Email:          "deleteme@example.com",
		PasswordHash:   "$2a$10$92IXUNpkjO0rOQ5byMi.Ye4oKoEa3Ro9llC/.og/at2.uheWG/igi",
		FirstName:      "Delete",
		LastName:       "Me",
		Role:           "care_worker",
		OrganizationID: "test-org",
		IsActive:       true,
	}
	handler.DB.Create(&testUser)

	t.Run("Valid user deletion", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "/api/v1/users/delete-test-user", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))
	})

	t.Run("User not found", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "/api/v1/users/nonexistent", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

// Helper function to get a test JWT token
func getTestToken(handler *Handler) string {
	if handler == nil {
		// Return a mock token for basic auth tests
		cfg := &config.Config{
			JWTSecret: "test-secret-key",
			JWTExpiry: 24 * time.Hour,
		}
		handler = &Handler{Config: cfg}
	}
	
	user := models.User{
		ID:    "test-user",
		Email: "test@example.com",
		Role:  "admin",
		OrganizationID: "test-org",
	}
	
	// Generate test JWT token
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": user.ID,
		"email":   user.Email,
		"role":    user.Role,
		"org_id":  user.OrganizationID,
		"exp":     time.Now().Add(time.Hour).Unix(),
		"iat":     time.Now().Unix(),
	})

	tokenString, _ := token.SignedString([]byte("test-secret-key"))
	return tokenString
}