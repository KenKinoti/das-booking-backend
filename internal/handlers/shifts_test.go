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

func TestCreateShift(t *testing.T) {
	handler, router := setupTestHandler()

	// Create test participant and staff
	testParticipant := models.Participant{
		ID:           "shift-participant",
		FirstName:    "Shift",
		LastName:     "Participant",
		DateOfBirth:  time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
		NDISNumber:   "SHIFT123",
	}
	handler.DB.Create(&testParticipant)

	testStaff := models.User{
		ID:             "shift-staff",
		Email:          "staff@example.com",
		FirstName:      "Shift",
		LastName:       "Staff",
		Role:           "care_worker",
		IsActive:       true,
	}
	handler.DB.Create(&testStaff)

	t.Run("Valid shift creation", func(t *testing.T) {
		futureTime := time.Now().Add(24 * time.Hour)
		shiftData := map[string]interface{}{
			"participant_id": "shift-participant",
			"staff_id":       "shift-staff",
			"start_time":     futureTime.Format(time.RFC3339),
			"end_time":       futureTime.Add(8 * time.Hour).Format(time.RFC3339),
			"service_type":   "Personal Care",
			"location":       "Participant's Home",
			"hourly_rate":    45.50,
			"notes":          "Test shift",
		}

		body, _ := json.Marshal(shiftData)
		req := httptest.NewRequest("POST", "/api/v1/shifts", bytes.NewBuffer(body))
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

	t.Run("Invalid time range (end before start)", func(t *testing.T) {
		futureTime := time.Now().Add(24 * time.Hour)
		shiftData := map[string]interface{}{
			"participant_id": "shift-participant",
			"staff_id":       "shift-staff",
			"start_time":     futureTime.Format(time.RFC3339),
			"end_time":       futureTime.Add(-1 * time.Hour).Format(time.RFC3339), // End before start
			"service_type":   "Personal Care",
			"hourly_rate":    45.50,
		}

		body, _ := json.Marshal(shiftData)
		req := httptest.NewRequest("POST", "/api/v1/shifts", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("Missing required fields", func(t *testing.T) {
		shiftData := map[string]interface{}{
			"participant_id": "shift-participant",
			// Missing staff_id, start_time, end_time
		}

		body, _ := json.Marshal(shiftData)
		req := httptest.NewRequest("POST", "/api/v1/shifts", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("Invalid participant ID", func(t *testing.T) {
		futureTime := time.Now().Add(24 * time.Hour)
		shiftData := map[string]interface{}{
			"participant_id": "nonexistent-participant",
			"staff_id":       "shift-staff",
			"start_time":     futureTime.Format(time.RFC3339),
			"end_time":       futureTime.Add(8 * time.Hour).Format(time.RFC3339),
			"service_type":   "Personal Care",
			"hourly_rate":    45.50,
		}

		body, _ := json.Marshal(shiftData)
		req := httptest.NewRequest("POST", "/api/v1/shifts", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestGetShifts(t *testing.T) {
	_, router := setupTestHandler()

	t.Run("Get shifts with pagination", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/shifts?page=1&limit=10", nil)
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

	t.Run("Filter by status", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/shifts?status=scheduled", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(nil))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("Filter by staff ID", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/shifts?staff_id=test-user", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(nil))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestGetShift(t *testing.T) {
	handler, router := setupTestHandler()

	// Create a test shift
	futureTime := time.Now().Add(24 * time.Hour)
	testShift := models.Shift{
		ID:            "test-shift",
		ParticipantID: "shift-participant",
		StaffID:       "shift-staff",
		StartTime:     futureTime,
		EndTime:       futureTime.Add(8 * time.Hour),
		ServiceType:   "Personal Care",
		Status:        "scheduled",
		HourlyRate:    45.50,
	}
	handler.DB.Create(&testShift)

	t.Run("Get existing shift", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/shifts/test-shift", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))

		data := response["data"].(map[string]interface{})
		assert.Equal(t, "Personal Care", data["service_type"])
		assert.Equal(t, "scheduled", data["status"])
	})

	t.Run("Shift not found", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/shifts/nonexistent", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestUpdateShift(t *testing.T) {
	handler, router := setupTestHandler()

	// Create a test shift to update
	futureTime := time.Now().Add(24 * time.Hour)
	testShift := models.Shift{
		ID:            "update-shift",
		ParticipantID: "shift-participant",
		StaffID:       "shift-staff",
		StartTime:     futureTime,
		EndTime:       futureTime.Add(8 * time.Hour),
		ServiceType:   "Personal Care",
		Status:        "scheduled",
		HourlyRate:    45.50,
	}
	handler.DB.Create(&testShift)

	t.Run("Valid shift update", func(t *testing.T) {
		updateData := map[string]interface{}{
			"hourly_rate": 50.00,
			"notes":       "Updated shift notes",
			"location":    "Updated location",
		}

		body, _ := json.Marshal(updateData)
		req := httptest.NewRequest("PUT", "/api/v1/shifts/update-shift", bytes.NewBuffer(body))
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

	t.Run("Shift not found", func(t *testing.T) {
		updateData := map[string]interface{}{
			"hourly_rate": 50.00,
		}

		body, _ := json.Marshal(updateData)
		req := httptest.NewRequest("PUT", "/api/v1/shifts/nonexistent", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestUpdateShiftStatus(t *testing.T) {
	handler, router := setupTestHandler()

	// Create a test shift for status updates
	futureTime := time.Now().Add(24 * time.Hour)
	testShift := models.Shift{
		ID:            "status-shift",
		ParticipantID: "shift-participant",
		StaffID:       "shift-staff",
		StartTime:     futureTime,
		EndTime:       futureTime.Add(8 * time.Hour),
		ServiceType:   "Personal Care",
		Status:        "scheduled",
		HourlyRate:    45.50,
	}
	handler.DB.Create(&testShift)

	t.Run("Valid status update to in_progress", func(t *testing.T) {
		statusData := map[string]interface{}{
			"status":              "in_progress",
			"actual_start_time":   time.Now().Format(time.RFC3339),
		}

		body, _ := json.Marshal(statusData)
		req := httptest.NewRequest("PATCH", "/api/v1/shifts/status-shift/status", bytes.NewBuffer(body))
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

	t.Run("Invalid status transition", func(t *testing.T) {
		// Try to update a scheduled shift directly to completed (should require in_progress first)
		testShift2 := models.Shift{
			ID:            "status-shift-2",
			ParticipantID: "shift-participant",
			StaffID:       "shift-staff",
			StartTime:     futureTime,
			EndTime:       futureTime.Add(8 * time.Hour),
			Status:        "scheduled",
			HourlyRate:    45.50,
			}
		handler.DB.Create(&testShift2)

		statusData := map[string]interface{}{
			"status": "completed", // Invalid: can't go from scheduled to completed
		}

		body, _ := json.Marshal(statusData)
		req := httptest.NewRequest("PATCH", "/api/v1/shifts/status-shift-2/status", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestDeleteShift(t *testing.T) {
	handler, router := setupTestHandler()

	// Create a test shift to delete
	futureTime := time.Now().Add(24 * time.Hour)
	testShift := models.Shift{
		ID:            "delete-shift",
		ParticipantID: "shift-participant",
		StaffID:       "shift-staff",
		StartTime:     futureTime,
		EndTime:       futureTime.Add(8 * time.Hour),
		ServiceType:   "Personal Care",
		Status:        "scheduled",
		HourlyRate:    45.50,
	}
	handler.DB.Create(&testShift)

	t.Run("Valid shift deletion", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "/api/v1/shifts/delete-shift", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var response map[string]interface{}
		err := json.Unmarshal(w.Body.Bytes(), &response)
		assert.NoError(t, err)
		assert.True(t, response["success"].(bool))
	})

	t.Run("Shift not found", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "/api/v1/shifts/nonexistent", nil)
		req.Header.Set("Authorization", "Bearer "+getTestToken(handler))

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}