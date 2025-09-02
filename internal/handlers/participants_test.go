package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/stretchr/testify/assert"
)

func TestCreateParticipant(t *testing.T) {
	handler, router := setupTestHandler()

	t.Run("Valid participant creation", func(t *testing.T) {
		participantData := map[string]interface{}{
			"first_name":     "Jane",
			"last_name":      "Participant",
			"date_of_birth":  "1990-05-15T00:00:00Z",
			"ndis_number":    "1234567890",
			"email":          "jane.participant@email.com",
			"phone":          "+61456789123",
			"address": map[string]interface{}{
				"street":   "123 Test Street",
				"suburb":   "Adelaide",
				"state":    "SA",
				"postcode": "5000",
				"country":  "Australia",
			},
			"medical_information": map[string]interface{}{
				"conditions":    `["Test Condition"]`,
				"medications":   `["Test Medication"]`,
				"doctor_name":   "Dr. Test",
				"doctor_phone":  "+61887654321",
			},
			"funding": map[string]interface{}{
				"total_budget":     30000.00,
				"used_budget":      5000.00,
				"remaining_budget": 25000.00,
				"budget_year":      "2023-2024",
			},
		}

		body, _ := json.Marshal(participantData)
		req := httptest.NewRequest("POST", "/api/v1/participants", bytes.NewBuffer(body))
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

	t.Run("Invalid date format", func(t *testing.T) {
		participantData := map[string]interface{}{
			"first_name":    "Jane",
			"last_name":     "Participant",
			"date_of_birth": "invalid-date",
			"ndis_number":   "1234567891",
			"email":         "jane.test@email.com",
		}

		body, _ := json.Marshal(participantData)
		req := httptest.NewRequest("POST", "/api/v1/participants", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("Missing required fields", func(t *testing.T) {
		participantData := map[string]interface{}{
			"first_name": "Jane",
			// Missing last_name, date_of_birth, ndis_number
		}

		body, _ := json.Marshal(participantData)
		req := httptest.NewRequest("POST", "/api/v1/participants", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("Duplicate NDIS number", func(t *testing.T) {
		// First create a participant
		firstParticipant := models.Participant{
			ID:           "first-participant",
			FirstName:    "First",
			LastName:     "Participant",
			DateOfBirth:  time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
			NDISNumber:   "DUPLICATE123",
			OrganizationID: "test-org",
		}
		handler.DB.Create(&firstParticipant)

		// Try to create another with same NDIS number
		participantData := map[string]interface{}{
			"first_name":    "Second",
			"last_name":     "Participant",
			"date_of_birth": "1991-01-01",
			"ndis_number":   "DUPLICATE123",
			"email":         "second@email.com",
		}

		body, _ := json.Marshal(participantData)
		req := httptest.NewRequest("POST", "/api/v1/participants", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("Unauthorized access", func(t *testing.T) {
		participantData := map[string]interface{}{
			"first_name":    "Jane",
			"last_name":     "Participant",
			"date_of_birth": "1990-05-15T00:00:00Z",
			"ndis_number":   "1234567892",
		}

		body, _ := json.Marshal(participantData)
		req := httptest.NewRequest("POST", "/api/v1/participants", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

func TestGetParticipants(t *testing.T) {
	_, router := setupTestHandler()

	t.Run("Get participants with pagination", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/participants?page=1&limit=10", nil)
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

	t.Run("Search by name", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/participants?search=test", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(nil))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestGetParticipant(t *testing.T) {
	handler, router := setupTestHandler()

	// Create a test participant
	testParticipant := models.Participant{
		ID:           "test-participant",
		FirstName:    "Test",
		LastName:     "Participant",
		DateOfBirth:  time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
		NDISNumber:   "TEST123456",
		Email:        "test.participant@email.com",
		OrganizationID: "test-org",
	}
	handler.DB.Create(&testParticipant)

	t.Run("Get existing participant", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/participants/test-participant", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))

		data := response["data"].(map[string]interface{})
		assert.Equal(t, "Test", data["first_name"])
		assert.Equal(t, "Participant", data["last_name"])
	})

	t.Run("Participant not found", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/participants/nonexistent", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestUpdateParticipant(t *testing.T) {
	handler, router := setupTestHandler()

	// Create a test participant to update
	testParticipant := models.Participant{
		ID:           "update-participant",
		FirstName:    "Update",
		LastName:     "Participant",
		DateOfBirth:  time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
		NDISNumber:   "UPDATE123",
		Email:        "update@email.com",
		OrganizationID: "test-org",
	}
	handler.DB.Create(&testParticipant)

	t.Run("Valid participant update", func(t *testing.T) {
		updateData := map[string]interface{}{
			"phone": "+61987654321",
			"address": map[string]interface{}{
				"street":   "456 Updated Street",
				"suburb":   "Adelaide",
				"state":    "SA",
				"postcode": "5001",
				"country":  "Australia",
			},
		}

		body, _ := json.Marshal(updateData)
		req := httptest.NewRequest("PUT", "/api/v1/participants/update-participant", bytes.NewBuffer(body))
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

	t.Run("Participant not found", func(t *testing.T) {
		updateData := map[string]interface{}{
			"phone": "+61987654321",
		}

		body, _ := json.Marshal(updateData)
		req := httptest.NewRequest("PUT", "/api/v1/participants/nonexistent", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestDeleteParticipant(t *testing.T) {
	handler, router := setupTestHandler()

	// Create a test participant to delete
	testParticipant := models.Participant{
		ID:           "delete-participant",
		FirstName:    "Delete",
		LastName:     "Participant",
		DateOfBirth:  time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
		NDISNumber:   "DELETE123",
		Email:        "delete@email.com",
		OrganizationID: "test-org",
	}
	handler.DB.Create(&testParticipant)

	t.Run("Valid participant deletion", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "/api/v1/participants/delete-participant", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))
	})

	t.Run("Participant not found", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "/api/v1/participants/nonexistent", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}