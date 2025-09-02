package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// IntegrationTestSuite defines the integration test suite
type IntegrationTestSuite struct {
	suite.Suite
	router      *gin.Engine
	db          *gorm.DB
	handler     *handlers.Handler
	accessToken string
	userID      string
	orgID       string
}

// SetupSuite runs once before all tests
func (suite *IntegrationTestSuite) SetupSuite() {
	// Set Gin to test mode
	gin.SetMode(gin.TestMode)

	// Setup test database
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	suite.Require().NoError(err)

	// Auto migrate
	err = models.MigrateDB(db)
	suite.Require().NoError(err)

	suite.db = db

	// Setup test configuration
	cfg := &config.Config{
		JWTSecret:          "test-secret-key-for-testing-only",
		JWTExpiry:          24 * time.Hour,
		RefreshTokenExpiry: 7 * 24 * time.Hour,
	}

	// Create handler
	suite.handler = handlers.NewHandler(db, cfg)

	// Setup router
	suite.router = gin.New()
	suite.handler.SetupRoutes(suite.router)

	// Seed test data
	suite.seedTestData()

	// Login to get access token
	suite.loginTestUser()
}

// TearDownSuite runs once after all tests
func (suite *IntegrationTestSuite) TearDownSuite() {
	// Close database connection
	sqlDB, _ := suite.db.DB()
	sqlDB.Close()
}

// seedTestData creates test data for integration tests
func (suite *IntegrationTestSuite) seedTestData() {
	// Create test organization
	org := models.Organization{
		ID:    "test-org-id",
		Name:  "Test Organization",
		Email: "test@example.com",
	}
	suite.db.Create(&org)
	suite.orgID = org.ID

	// Create test admin user
	user := models.User{
		ID:             "test-user-id",
		Email:          "admin@test.com",
		PasswordHash:   "$2a$10$92IXUNpkjO0rOQ5byMi.Ye4oKoEa3Ro9llC/.og/at2.uheWG/igi", // "password"
		FirstName:      "Test",
		LastName:       "Admin",
		Role:           "admin",
		OrganizationID: org.ID,
		IsActive:       true,
	}
	suite.db.Create(&user)
	suite.userID = user.ID
}

// loginTestUser performs login and stores access token
func (suite *IntegrationTestSuite) loginTestUser() {
	loginData := map[string]interface{}{
		"email":    "admin@test.com",
		"password": "password",
	}

	body, _ := json.Marshal(loginData)
	req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)

	suite.Equal(http.StatusOK, w.Code)

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	suite.NoError(err)

	data := response["data"].(map[string]interface{})
	suite.accessToken = data["token"].(string)
}

// makeAuthenticatedRequest helper method to make authenticated requests
func (suite *IntegrationTestSuite) makeAuthenticatedRequest(method, path string, body interface{}) *httptest.ResponseRecorder {
	var reqBody io.Reader
	if body != nil {
		jsonBody, _ := json.Marshal(body)
		reqBody = bytes.NewBuffer(jsonBody)
	}

	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+suite.accessToken)

	w := httptest.NewRecorder()
	suite.router.ServeHTTP(w, req)

	return w
}

// Test Authentication
func (suite *IntegrationTestSuite) TestAuthentication() {
	suite.Run("Login with valid credentials", func() {
		loginData := map[string]interface{}{
			"email":    "admin@test.com",
			"password": "password",
		}

		body, _ := json.Marshal(loginData)
		req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		suite.router.ServeHTTP(w, req)

		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))
		suite.Contains(response, "data")
	})

	suite.Run("Login with invalid credentials", func() {
		loginData := map[string]interface{}{
			"email":    "admin@test.com",
			"password": "wrongpassword",
		}

		body, _ := json.Marshal(loginData)
		req := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		suite.router.ServeHTTP(w, req)

		suite.Equal(http.StatusUnauthorized, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.False(response["success"].(bool))
	})
}

// Test Users Management
func (suite *IntegrationTestSuite) TestUsersManagement() {
	suite.Run("Get current user", func() {
		w := suite.makeAuthenticatedRequest("GET", "/api/v1/users/me", nil)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		suite.Equal("admin@test.com", data["email"])
	})

	suite.Run("Get all users", func() {
		w := suite.makeAuthenticatedRequest("GET", "/api/v1/users?page=1&limit=10", nil)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))
	})

	suite.Run("Create user", func() {
		userData := map[string]interface{}{
			"email":      "newuser@test.com",
			"password":   "testpassword123",
			"first_name": "New",
			"last_name":  "User",
			"role":       "care_worker",
		}

		w := suite.makeAuthenticatedRequest("POST", "/api/v1/users", userData)
		suite.Equal(http.StatusCreated, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		suite.Equal("newuser@test.com", data["email"])
		suite.Equal("care_worker", data["role"])
	})

	suite.Run("Create user with duplicate email", func() {
		userData := map[string]interface{}{
			"email":      "admin@test.com", // This email already exists
			"password":   "testpassword123",
			"first_name": "Duplicate",
			"last_name":  "User",
			"role":       "care_worker",
		}

		w := suite.makeAuthenticatedRequest("POST", "/api/v1/users", userData)
		suite.Equal(http.StatusConflict, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.False(response["success"].(bool))
	})
}

// Test Participants Management
func (suite *IntegrationTestSuite) TestParticipantsManagement() {
	var participantID string

	suite.Run("Create participant", func() {
		participantData := map[string]interface{}{
			"first_name":    "Jane",
			"last_name":     "Doe",
			"date_of_birth": "1990-05-15",
			"ndis_number":   "1234567890",
			"email":         "jane.doe@test.com",
			"phone":         "+61456789123",
			"address": map[string]interface{}{
				"street":   "123 Test Street",
				"suburb":   "Adelaide",
				"state":    "SA",
				"postcode": "5000",
				"country":  "Australia",
			},
		}

		w := suite.makeAuthenticatedRequest("POST", "/api/v1/participants", participantData)
		suite.Equal(http.StatusCreated, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		participantID = data["id"].(string)
		suite.Equal("Jane", data["first_name"])
		suite.Equal("1234567890", data["ndis_number"])
	})

	suite.Run("Get participant by ID", func() {
		w := suite.makeAuthenticatedRequest("GET", fmt.Sprintf("/api/v1/participants/%s", participantID), nil)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		suite.Equal(participantID, data["id"])
	})

	suite.Run("Update participant", func() {
		updateData := map[string]interface{}{
			"phone": "+61987654321",
		}

		w := suite.makeAuthenticatedRequest("PUT", fmt.Sprintf("/api/v1/participants/%s", participantID), updateData)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		suite.Equal("+61987654321", data["phone"])
	})

	suite.Run("Create participant with duplicate NDIS number", func() {
		duplicateData := map[string]interface{}{
			"first_name":    "John",
			"last_name":     "Smith",
			"date_of_birth": "1985-03-10",
			"ndis_number":   "1234567890", // Same as previous participant
			"email":         "john.smith@test.com",
		}

		w := suite.makeAuthenticatedRequest("POST", "/api/v1/participants", duplicateData)
		suite.Equal(http.StatusConflict, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.False(response["success"].(bool))
	})
}

// Test Shifts Management
func (suite *IntegrationTestSuite) TestShiftsManagement() {
	// First create a participant for the shift
	participant := models.Participant{
		ID:             "test-participant-id",
		FirstName:      "Test",
		LastName:       "Participant",
		DateOfBirth:    time.Date(1990, 5, 15, 0, 0, 0, 0, time.UTC),
		OrganizationID: suite.orgID,
		IsActive:       true,
	}
	suite.db.Create(&participant)

	var shiftID string

	suite.Run("Create shift", func() {
		shiftData := map[string]interface{}{
			"participant_id": participant.ID,
			"staff_id":       suite.userID,
			"start_time":     "2023-12-15T09:00:00Z",
			"end_time":       "2023-12-15T17:00:00Z",
			"service_type":   "Personal Care",
			"location":       "Participant's Home",
			"hourly_rate":    45.50,
			"notes":          "Test shift",
		}

		w := suite.makeAuthenticatedRequest("POST", "/api/v1/shifts", shiftData)
		suite.Equal(http.StatusCreated, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		shiftID = data["id"].(string)
		suite.Equal("scheduled", data["status"])
		suite.Equal(364.0, data["total_cost"]) // 8 hours * 45.50
	})

	suite.Run("Update shift status", func() {
		statusData := map[string]interface{}{
			"status":             "in_progress",
			"actual_start_time":  "2023-12-15T09:05:00Z",
		}

		w := suite.makeAuthenticatedRequest("PATCH", fmt.Sprintf("/api/v1/shifts/%s/status", shiftID), statusData)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		suite.Equal("in_progress", data["status"])
	})

	suite.Run("Create conflicting shift", func() {
		conflictData := map[string]interface{}{
			"participant_id": participant.ID,
			"staff_id":       suite.userID,
			"start_time":     "2023-12-15T10:00:00Z", // Overlaps with existing shift
			"end_time":       "2023-12-15T18:00:00Z",
			"service_type":   "Personal Care",
			"location":       "Participant's Home",
			"hourly_rate":    45.50,
		}

		w := suite.makeAuthenticatedRequest("POST", "/api/v1/shifts", conflictData)
		suite.Equal(http.StatusConflict, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.False(response["success"].(bool))
	})
}

// Test Emergency Contacts
func (suite *IntegrationTestSuite) TestEmergencyContacts() {
	// Create a participant first
	participant := models.Participant{
		ID:             "test-participant-contacts",
		FirstName:      "Test",
		LastName:       "Participant",
		DateOfBirth:    time.Date(1990, 5, 15, 0, 0, 0, 0, time.UTC),
		OrganizationID: suite.orgID,
		IsActive:       true,
	}
	suite.db.Create(&participant)

	var contactID string

	suite.Run("Create emergency contact", func() {
		contactData := map[string]interface{}{
			"participant_id": participant.ID,
			"name":           "John Doe",
			"relationship":   "Father",
			"phone":          "+61412345678",
			"email":          "john.doe@test.com",
			"is_primary":     true,
		}

		w := suite.makeAuthenticatedRequest("POST", "/api/v1/emergency-contacts", contactData)
		suite.Equal(http.StatusCreated, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		contactID = data["id"].(string)
		suite.Equal("John Doe", data["name"])
		suite.True(data["is_primary"].(bool))
	})

	suite.Run("Get emergency contacts for participant", func() {
		w := suite.makeAuthenticatedRequest("GET", fmt.Sprintf("/api/v1/emergency-contacts?participant_id=%s", participant.ID), nil)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		contacts := data["emergency_contacts"].([]interface{})
		suite.Len(contacts, 1)
	})

	suite.Run("Update emergency contact", func() {
		updateData := map[string]interface{}{
			"phone": "+61987654321",
		}

		w := suite.makeAuthenticatedRequest("PUT", fmt.Sprintf("/api/v1/emergency-contacts/%s", contactID), updateData)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		suite.Equal("+61987654321", data["phone"])
	})
}

// Test Care Plans
func (suite *IntegrationTestSuite) TestCarePlans() {
	// Create a participant first
	participant := models.Participant{
		ID:             "test-participant-care-plans",
		FirstName:      "Test",
		LastName:       "Participant",
		DateOfBirth:    time.Date(1990, 5, 15, 0, 0, 0, 0, time.UTC),
		OrganizationID: suite.orgID,
		IsActive:       true,
	}
	suite.db.Create(&participant)

	var carePlanID string

	suite.Run("Create care plan", func() {
		carePlanData := map[string]interface{}{
			"participant_id": participant.ID,
			"title":          "Test Care Plan",
			"description":    "A comprehensive care plan for testing",
			"goals":          "Improve daily living skills",
			"start_date":     "2023-12-01T00:00:00Z",
			"end_date":       "2024-11-30T23:59:59Z",
		}

		w := suite.makeAuthenticatedRequest("POST", "/api/v1/care-plans", carePlanData)
		suite.Equal(http.StatusCreated, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		carePlanID = data["id"].(string)
		suite.Equal("Test Care Plan", data["title"])
		suite.Equal("active", data["status"])
	})

	suite.Run("Approve care plan", func() {
		approvalData := map[string]interface{}{
			"approval_action": "approve",
		}

		w := suite.makeAuthenticatedRequest("PATCH", fmt.Sprintf("/api/v1/care-plans/%s/approve", carePlanID), approvalData)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		suite.NotNil(data["approved_by"])
		suite.NotNil(data["approved_at"])
	})
}

// Test Organization Management
func (suite *IntegrationTestSuite) TestOrganizationManagement() {
	suite.Run("Get organization", func() {
		w := suite.makeAuthenticatedRequest("GET", "/api/v1/organization", nil)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		suite.Equal("Test Organization", data["name"])
	})

	suite.Run("Update organization", func() {
		updateData := map[string]interface{}{
			"name":  "Updated Test Organization",
			"phone": "+61887654321",
		}

		w := suite.makeAuthenticatedRequest("PUT", "/api/v1/organization", updateData)
		suite.Equal(http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		suite.NoError(err)
		suite.True(response["success"].(bool))

		data := response["data"].(map[string]interface{})
		suite.Equal("Updated Test Organization", data["name"])
		suite.Equal("+61887654321", data["phone"])
	})
}

// Test unauthorized access
func (suite *IntegrationTestSuite) TestUnauthorizedAccess() {
	suite.Run("Access protected endpoint without token", func() {
		req := httptest.NewRequest("GET", "/api/v1/users/me", nil)
		w := httptest.NewRecorder()
		suite.router.ServeHTTP(w, req)

		suite.Equal(http.StatusUnauthorized, w.Code)
	})

	suite.Run("Access with invalid token", func() {
		req := httptest.NewRequest("GET", "/api/v1/users/me", nil)
		req.Header.Set("Authorization", "Bearer invalid-token")
		w := httptest.NewRecorder()
		suite.router.ServeHTTP(w, req)

		suite.Equal(http.StatusUnauthorized, w.Code)
	})
}

// TestIntegrationSuite runs the integration test suite
func TestIntegrationSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration tests in short mode")
	}

	suite.Run(t, new(IntegrationTestSuite))
}

// Individual test functions for running specific tests
func TestMain(m *testing.M) {
	// Setup
	gin.SetMode(gin.TestMode)
	
	// Run tests
	code := m.Run()
	
	// Cleanup
	os.Exit(code)
}