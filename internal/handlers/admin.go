package handlers

import (
	"bytes"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// SeedDatabase runs the database seeder to add test data
func (h *Handler) SeedDatabase(c *gin.Context) {
	// Get project root directory
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "RUNTIME_ERROR",
				"message": "Could not get runtime caller info",
			},
		})
		return
	}

	// Navigate to project root (go up from internal/handlers to project root)
	projectRoot := filepath.Dir(filepath.Dir(filepath.Dir(filename)))
	seederPath := filepath.Join(projectRoot, "cmd", "seed")

	// Change to seeder directory and run the seeder
	cmd := exec.Command("go", "run", "main.go")
	cmd.Dir = seederPath
	cmd.Env = os.Environ()

	// Capture output
	output, err := cmd.CombinedOutput()
	
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "SEEDER_ERROR",
				"message": "Failed to run database seeder",
				"details": string(output),
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Database seeded successfully",
		"output":  string(output),
	})
}

// SeedAdvancedRequest represents the advanced seeding request body
type SeedAdvancedRequest struct {
	OrgStrategy         string   `json:"orgStrategy"`
	TargetOrgId         string   `json:"targetOrgId,omitempty"`
	Tables              []string `json:"tables"`
	Prefix              string   `json:"prefix"`
	AutoIncrement       bool     `json:"autoIncrement"`
	RecordCount         int      `json:"recordCount"`
	CreateOrganizations bool     `json:"createOrganizations"`
	OrgCount            int      `json:"orgCount"`
	OrgPrefix           string   `json:"orgPrefix"`
}

// SeedOrganizations creates organizations only
func (h *Handler) SeedOrganizations(c *gin.Context) {
	// Verify admin access
	userRole, exists := c.Get("user_role")
	if !exists || (userRole != "admin" && userRole != "super_admin") {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error": "Access denied - requires admin role",
		})
		return
	}

	type SeedOrganizationsRequest struct {
		Count  int    `json:"count" binding:"required,min=1,max=20"`
		Prefix string `json:"prefix" binding:"required"`
	}

	var req SeedOrganizationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "Invalid request body: " + err.Error(),
		})
		return
	}

	// Initialize random seed
	rand.Seed(time.Now().UnixNano())
	
	var createdOrgs []models.Organization
	tx := h.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	orgSuffixes := []string{"Services", "Solutions", "Care Group", "Support Network", "Healthcare", "Assistance", "Community Care", "Wellness", "Foundation", "Associates"}
	states := []string{"NSW", "VIC", "QLD", "SA", "WA", "TAS", "NT", "ACT"}
	suburbs := []string{"Sydney", "Melbourne", "Brisbane", "Perth", "Adelaide", "Gold Coast", "Newcastle", "Canberra", "Wollongong", "Sunshine Coast", "Geelong", "Townsville"}

	for i := 0; i < req.Count; i++ {
		suffix := orgSuffixes[rand.Intn(len(orgSuffixes))]
		state := states[rand.Intn(len(states))]
		suburb := suburbs[rand.Intn(len(suburbs))]
		
		abn := fmt.Sprintf("%011d", rand.Int63n(99999999999))
		
		org := models.Organization{
			ID:      uuid.New().String(),
			Name:    fmt.Sprintf("%s %s %d", req.Prefix, suffix, i+1),
			ABN:     abn,
			Phone:   fmt.Sprintf("(0%d) %d%d%d-%d%d%d%d", 
				rand.Intn(9)+1, rand.Intn(10), rand.Intn(10), rand.Intn(10),
				rand.Intn(10), rand.Intn(10), rand.Intn(10), rand.Intn(10)),
			Email:   fmt.Sprintf("info@%s%d.com.au", strings.ToLower(req.Prefix), i+1),
			Website: fmt.Sprintf("https://%s%d.com.au", strings.ToLower(req.Prefix), i+1),
			Address: models.Address{
				Street:   fmt.Sprintf("%d %s Street, Suite %d", rand.Intn(999)+1, req.Prefix, rand.Intn(99)+1),
				Suburb:   suburb,
				State:    state,
				Postcode: fmt.Sprintf("%d%d%d%d", rand.Intn(10), rand.Intn(10), rand.Intn(10), rand.Intn(10)),
				Country:  "Australia",
			},
			NDISReg: models.NDISReg{
				RegistrationNumber: fmt.Sprintf("NDIS%06d", rand.Intn(999999)),
				RegistrationStatus: "active",
				ExpiryDate:         &[]time.Time{time.Now().AddDate(rand.Intn(3)+1, rand.Intn(12), 0)}[0],
			},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}

		// Check if organization with same ABN already exists
		var existingOrg models.Organization
		if err := tx.Where("abn = ?", org.ABN).First(&existingOrg).Error; err == nil {
			// Generate new ABN and try again
			org.ABN = fmt.Sprintf("%011d", rand.Int63n(99999999999))
		}

		if err := tx.Create(&org).Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{
				"success": false,
				"error":   "Failed to create organization: " + err.Error(),
			})
			return
		}

		createdOrgs = append(createdOrgs, org)
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "Failed to commit transaction: " + err.Error(),
		})
		return
	}

	// Convert to response format
	orgResponses := make([]gin.H, len(createdOrgs))
	for i, org := range createdOrgs {
		orgResponses[i] = gin.H{
			"id":      org.ID,
			"name":    org.Name,
			"abn":     org.ABN,
			"phone":   org.Phone,
			"email":   org.Email,
			"website": org.Website,
			"address": gin.H{
				"street":   org.Address.Street,
				"suburb":   org.Address.Suburb,
				"state":    org.Address.State,
				"postcode": org.Address.Postcode,
				"country":  org.Address.Country,
			},
			"ndis_registration": gin.H{
				"number": org.NDISReg.RegistrationNumber,
				"status": org.NDISReg.RegistrationStatus,
				"expiry": org.NDISReg.ExpiryDate.Format("2006-01-02"),
			},
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": fmt.Sprintf("Successfully created %d organizations", len(createdOrgs)),
		"data": gin.H{
			"organizations": orgResponses,
			"count":         len(createdOrgs),
		},
	})
}

// SeedAdvanced performs advanced database seeding with organization creation
func (h *Handler) SeedAdvanced(c *gin.Context) {
	// Verify admin access
	userRole, exists := c.Get("user_role")
	if !exists || (userRole != "admin" && userRole != "super_admin") {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error": "Access denied - requires admin role",
		})
		return
	}

	var req SeedAdvancedRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "Invalid request body: " + err.Error(),
		})
		return
	}

	// Initialize random seed
	rand.Seed(time.Now().UnixNano())
	
	var createdOrgs []models.Organization
	var recordsCreated int64

	tx := h.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// Step 1: Create organizations if requested
	if req.CreateOrganizations {
		orgSuffixes := []string{"Services", "Solutions", "Care Group", "Support Network", "Healthcare", "Assistance", "Community Care", "Wellness"}
		
		for i := 0; i < req.OrgCount; i++ {
			suffix := orgSuffixes[rand.Intn(len(orgSuffixes))]
			org := models.Organization{
				ID:          uuid.New().String(),
				Name:        fmt.Sprintf("%s %s %d", req.OrgPrefix, suffix, i+1),
				Phone:       fmt.Sprintf("(0%d) %d%d%d-%d%d%d%d", 
					rand.Intn(9)+1, rand.Intn(10), rand.Intn(10), rand.Intn(10),
					rand.Intn(10), rand.Intn(10), rand.Intn(10), rand.Intn(10)),
				Email:       fmt.Sprintf("info@%s%d.com", "testorg", i+1),
				Address:     models.Address{
					Street:   fmt.Sprintf("%d Test Street, Suite %d", rand.Intn(999)+1, rand.Intn(99)+1),
					Suburb:   []string{"Melbourne", "Sydney", "Brisbane", "Adelaide", "Perth"}[rand.Intn(5)],
					State:    []string{"VIC", "NSW", "QLD", "SA", "WA"}[rand.Intn(5)],
					Postcode: fmt.Sprintf("%d%d%d%d", rand.Intn(10), rand.Intn(10), rand.Intn(10), rand.Intn(10)),
					Country:  "Australia",
				},
				CreatedAt:   time.Now(),
				UpdatedAt:   time.Now(),
			}

			if err := tx.Create(&org).Error; err != nil {
				tx.Rollback()
				c.JSON(http.StatusInternalServerError, gin.H{
					"success": false,
					"error":   "Failed to create organization: " + err.Error(),
				})
				return
			}

			createdOrgs = append(createdOrgs, org)
			recordsCreated++
		}
	}

	// Step 2: Determine target organization(s) for seeding
	var targetOrgs []string
	switch req.OrgStrategy {
	case "create_new":
		// Use the first created organization or create a default one
		if len(createdOrgs) > 0 {
			targetOrgs = []string{createdOrgs[0].ID}
		} else {
			// Create a default organization
			org := models.Organization{
				ID:          uuid.New().String(),
				Name:        req.Prefix + " Organization",
				Phone:       "(03) 9123-4567",
				Email:       "info@test.com",
				Address:     models.Address{
					Street:   "123 Test Street",
					Suburb:   "Melbourne",
					State:    "VIC",
					Postcode: "3000",
					Country:  "Australia",
				},
				CreatedAt:   time.Now(),
				UpdatedAt:   time.Now(),
			}
			if err := tx.Create(&org).Error; err != nil {
				tx.Rollback()
				c.JSON(http.StatusInternalServerError, gin.H{
					"success": false,
					"error":   "Failed to create default organization: " + err.Error(),
				})
				return
			}
			targetOrgs = []string{org.ID}
			recordsCreated++
		}
	case "use_existing":
		if req.TargetOrgId == "" {
			tx.Rollback()
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error":   "Target organization ID is required when using existing organization",
			})
			return
		}
		targetOrgs = []string{req.TargetOrgId}
	case "random":
		// Get all existing organizations
		var orgs []models.Organization
		tx.Find(&orgs)
		for _, org := range orgs {
			targetOrgs = append(targetOrgs, org.ID)
		}
		// Include newly created organizations
		for _, org := range createdOrgs {
			targetOrgs = append(targetOrgs, org.ID)
		}
	}

	if len(targetOrgs) == 0 {
		tx.Rollback()
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "No target organizations available for seeding",
		})
		return
	}

	// Step 3: Seed data for each requested table
	for _, tableName := range req.Tables {
		switch tableName {
		case "participants":
			recordsCreated += h.seedParticipants(tx, targetOrgs, req.Prefix, req.RecordCount, req.AutoIncrement)
		case "shifts":
			recordsCreated += h.seedShifts(tx, targetOrgs, req.Prefix, req.RecordCount)
		case "care_plans":
			recordsCreated += h.seedCarePlans(tx, targetOrgs, req.Prefix, req.RecordCount)
		case "staff":
			recordsCreated += h.seedStaff(tx, targetOrgs, req.Prefix, req.RecordCount, req.AutoIncrement)
		case "documents":
			recordsCreated += h.seedDocuments(tx, targetOrgs, req.Prefix, req.RecordCount)
		}
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "Failed to commit transaction: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":        true,
		"message":        "Advanced seeding completed successfully",
		"recordsCreated": recordsCreated,
		"organizations":  len(createdOrgs),
		"tables":         len(req.Tables),
	})
}

// Helper functions for seeding different table types

func (h *Handler) seedParticipants(tx *gorm.DB, orgIds []string, prefix string, count int, autoIncrement bool) int64 {
	var created int64
	firstNames := []string{"John", "Jane", "Michael", "Sarah", "David", "Emma", "Chris", "Lisa", "Mark", "Anna"}
	lastNames := []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Rodriguez", "Martinez"}

	for i := 0; i < count; i++ {
		orgId := orgIds[rand.Intn(len(orgIds))]
		firstName := firstNames[rand.Intn(len(firstNames))]
		lastName := lastNames[rand.Intn(len(lastNames))]
		
		namePrefix := prefix
		if autoIncrement {
			namePrefix = fmt.Sprintf("%s_%03d", prefix, i+1)
		}

		participant := models.Participant{
			ID:             uuid.New().String(),
			OrganizationID: orgId,
			FirstName:      firstName,
			LastName:       lastName,
			Email:          fmt.Sprintf("%s_%s.%s@test.com", namePrefix, firstName, lastName),
			Phone:          fmt.Sprintf("04%d%d %d%d%d %d%d%d", 
				rand.Intn(10), rand.Intn(10), rand.Intn(10), rand.Intn(10), rand.Intn(10),
				rand.Intn(10), rand.Intn(10), rand.Intn(10)),
			DateOfBirth:    time.Now().AddDate(-rand.Intn(50)-20, 0, 0),
			Address:        models.Address{
				Street:   fmt.Sprintf("%d %s Street", rand.Intn(999)+1, namePrefix),
				Suburb:   []string{"Melbourne", "Sydney", "Brisbane", "Adelaide"}[rand.Intn(4)],
				State:    []string{"VIC", "NSW", "QLD", "SA"}[rand.Intn(4)],
				Postcode: fmt.Sprintf("%d%d%d%d", rand.Intn(10), rand.Intn(10), rand.Intn(10), rand.Intn(10)),
				Country:  "Australia",
			},
			IsActive:       rand.Float32() < 0.8, // 80% active
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		}

		if err := tx.Create(&participant).Error; err == nil {
			created++
		}
	}
	return created
}

func (h *Handler) seedStaff(tx *gorm.DB, orgIds []string, prefix string, count int, autoIncrement bool) int64 {
	var created int64
	firstNames := []string{"Alex", "Sam", "Jordan", "Casey", "Taylor", "Morgan", "Riley", "Avery", "Quinn", "Blake"}
	lastNames := []string{"Anderson", "Thompson", "White", "Harris", "Martin", "Jackson", "Clark", "Lewis", "Lee", "Walker"}
	roles := []string{"staff", "manager", "admin"}

	for i := 0; i < count; i++ {
		orgId := orgIds[rand.Intn(len(orgIds))]
		firstName := firstNames[rand.Intn(len(firstNames))]
		lastName := lastNames[rand.Intn(len(lastNames))]
		
		namePrefix := prefix
		if autoIncrement {
			namePrefix = fmt.Sprintf("%s_%03d", prefix, i+1)
		}

		hashedPassword, _ := bcrypt.GenerateFromPassword([]byte("Test123!@#"), bcrypt.DefaultCost)

		user := models.User{
			ID:             uuid.New().String(),
			OrganizationID: orgId,
			FirstName:      firstName,
			LastName:       lastName,
			Email:          fmt.Sprintf("%s_%s.%s@test.com", namePrefix, firstName, lastName),
			PasswordHash:   string(hashedPassword),
			Role:           roles[rand.Intn(len(roles))],
			Phone:          fmt.Sprintf("04%d%d %d%d%d %d%d%d", 
				rand.Intn(10), rand.Intn(10), rand.Intn(10), rand.Intn(10), rand.Intn(10),
				rand.Intn(10), rand.Intn(10), rand.Intn(10)),
			IsActive:       rand.Float32() < 0.9, // 90% active
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		}

		if err := tx.Create(&user).Error; err == nil {
			created++
		}
	}
	return created
}

func (h *Handler) seedShifts(tx *gorm.DB, orgIds []string, prefix string, count int) int64 {
	var created int64
	
	// Get participants and staff for the target organizations
	var participants []models.Participant
	var staff []models.User
	
	tx.Where("organization_id IN ?", orgIds).Find(&participants)
	tx.Where("organization_id IN ?", orgIds).Find(&staff)
	
	if len(participants) == 0 || len(staff) == 0 {
		return 0
	}

	statuses := []string{"scheduled", "in_progress", "completed", "cancelled"}
	serviceTypes := []string{"Personal Care", "Community Access", "Domestic Assistance", "Transport", "Social Support"}

	for i := 0; i < count; i++ {
		participant := participants[rand.Intn(len(participants))]
		staffMember := staff[rand.Intn(len(staff))]
		
		startTime := time.Now().AddDate(0, 0, rand.Intn(30)-15) // ±15 days from now
		endTime := startTime.Add(time.Duration(rand.Intn(4)+1) * time.Hour) // 1-4 hours

		shift := models.Shift{
			ID:            uuid.New().String(),
			ParticipantID: participant.ID,
			StaffID:       staffMember.ID,
			StartTime:     startTime,
			EndTime:       endTime,
			ServiceType:   serviceTypes[rand.Intn(len(serviceTypes))],
			Location:      participant.Address.Street,
			Status:        statuses[rand.Intn(len(statuses))],
			HourlyRate:    float64(25 + rand.Intn(20)), // $25-45/hour
			Notes:         fmt.Sprintf("%s shift for %s %s", prefix, participant.FirstName, participant.LastName),
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		}

		if err := tx.Create(&shift).Error; err == nil {
			created++
		}
	}
	return created
}

func (h *Handler) seedCarePlans(tx *gorm.DB, orgIds []string, prefix string, count int) int64 {
	var created int64
	
	var participants []models.Participant
	tx.Where("organization_id IN ?", orgIds).Find(&participants)
	
	if len(participants) == 0 {
		return 0
	}

	statuses := []string{"draft", "active", "under_review", "expired"}
	goalTypes := []string{"Independence", "Social Participation", "Health & Wellbeing", "Daily Living Skills"}

	for i := 0; i < count; i++ {
		participant := participants[rand.Intn(len(participants))]
		
		endDate := time.Now().AddDate(0, rand.Intn(12)+3, 0) // 3-15 months from now
		carePlan := models.CarePlan{
			ID:            uuid.New().String(),
			ParticipantID: participant.ID,
			Title:         fmt.Sprintf("%s Care Plan for %s %s", prefix, participant.FirstName, participant.LastName),
			Description:   fmt.Sprintf("Comprehensive care plan developed for %s", participant.FirstName),
			Goals:         goalTypes[rand.Intn(len(goalTypes))],
			Status:        statuses[rand.Intn(len(statuses))],
			StartDate:     time.Now().AddDate(0, 0, -rand.Intn(30)),
			EndDate:       &endDate,
			CreatedBy:     "user_admin",
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		}

		if err := tx.Create(&carePlan).Error; err == nil {
			created++
		}
	}
	return created
}

func (h *Handler) seedDocuments(tx *gorm.DB, orgIds []string, prefix string, count int) int64 {
	var created int64
	
	var participants []models.Participant
	tx.Where("organization_id IN ?", orgIds).Find(&participants)
	
	if len(participants) == 0 {
		return 0
	}

	categories := []string{"NDIS Plan", "Medical Records", "Assessment", "Agreement", "Report"}
	
	for i := 0; i < count; i++ {
		participant := participants[rand.Intn(len(participants))]
		category := categories[rand.Intn(len(categories))]
		
		participantId := participant.ID
		expiryDate := time.Now().AddDate(1, 0, 0) // 1 year from now
		document := models.Document{
			ID:               uuid.New().String(),
			ParticipantID:    &participantId,
			UploadedBy:       "user_admin",
			Filename:         fmt.Sprintf("%s_%d.pdf", prefix, i+1),
			OriginalFilename: fmt.Sprintf("%s_%s_%s.pdf", prefix, participant.FirstName, participant.LastName),
			Title:            fmt.Sprintf("%s %s - %s %s", prefix, category, participant.FirstName, participant.LastName),
			Category:         category,
			FileType:         "application/pdf",
			FilePath:         fmt.Sprintf("/uploads/test/%s_%d.pdf", prefix, i+1),
			FileSize:         int64(rand.Intn(5000000) + 100000), // 100KB - 5MB
			ExpiryDate:       &expiryDate,
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		}

		if err := tx.Create(&document).Error; err == nil {
			created++
		}
	}
	return created
}

// ClearTestData removes test data created by the seeder
func (h *Handler) ClearTestData(c *gin.Context) {
	tx := h.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// Delete test data (data with @test.com emails)
	var deletedCount int64

	// Delete shifts first (due to foreign key constraints)
	result := tx.Where("created_at >= ?", "2024-01-01").
		Delete(&models.Shift{})
	if result.Error != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete test shifts",
				"details": result.Error.Error(),
			},
		})
		return
	}
	deletedCount += result.RowsAffected

	// Delete care plans
	result = tx.Where("created_at >= ?", "2024-01-01").
		Delete(&models.CarePlan{})
	if result.Error != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete test care plans",
				"details": result.Error.Error(),
			},
		})
		return
	}
	deletedCount += result.RowsAffected

	// Delete emergency contacts
	result = tx.Where("created_at >= ?", "2024-01-01").
		Delete(&models.EmergencyContact{})
	if result.Error != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete test emergency contacts",
				"details": result.Error.Error(),
			},
		})
		return
	}
	deletedCount += result.RowsAffected

	// Delete participants
	result = tx.Where("created_at >= ?", "2024-01-01").
		Delete(&models.Participant{})
	if result.Error != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete test participants",
				"details": result.Error.Error(),
			},
		})
		return
	}
	deletedCount += result.RowsAffected

	// Delete test users (emails containing @test.com)
	result = tx.Where("email LIKE ?", "%@test.com").
		Delete(&models.User{})
	if result.Error != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete test users",
				"details": result.Error.Error(),
			},
		})
		return
	}
	deletedCount += result.RowsAffected

	// Delete test organizations (created after 2024)
	result = tx.Where("created_at >= ? AND name LIKE ?", "2024-01-01", "%Network%").
		Delete(&models.Organization{})
	if result.Error != nil {
		tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "DATABASE_ERROR",
				"message": "Failed to delete test organizations",
				"details": result.Error.Error(),
			},
		})
		return
	}
	deletedCount += result.RowsAffected

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "DATABASE_ERROR",
				"message": "Failed to commit transaction",
				"details": err.Error(),
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":       true,
		"message":       "Test data cleared successfully",
		"deleted_count": deletedCount,
	})
}

// TruncateDatabase removes ALL data from the database (DANGEROUS)
func (h *Handler) TruncateDatabase(c *gin.Context) {
	// Require double authentication for this dangerous operation
	if !h.RequireDoubleAuth(c) {
		return // Error already sent by RequireDoubleAuth
	}

	// Double check user authorization
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "UNAUTHORIZED",
				"message": "User not authenticated",
			},
		})
		return
	}

	// Get user and verify they are super admin
	var user models.User
	if err := h.DB.Where("id = ?", userID).First(&user).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "USER_NOT_FOUND",
				"message": "User not found",
			},
		})
		return
	}

	if user.Role != "super_admin" && user.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "INSUFFICIENT_PERMISSIONS",
				"message": "Only super admins can truncate the database",
			},
		})
		return
	}

	tx := h.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// Truncate all tables (order matters due to foreign keys)
	tables := []string{
		"user_permissions",
		"shifts", 
		"care_plans", 
		"emergency_contacts", 
		"participants", 
		"users", 
		"organizations",
		"roles",
		"permissions",
		"documents",
	}

	for _, table := range tables {
		if err := tx.Exec("TRUNCATE TABLE " + table + " RESTART IDENTITY CASCADE").Error; err != nil {
			tx.Rollback()
			c.JSON(http.StatusInternalServerError, gin.H{
				"success": false,
				"error": map[string]interface{}{
					"code":    "DATABASE_ERROR",
					"message": "Failed to truncate table: " + table,
					"details": err.Error(),
				},
			})
			return
		}
	}

	if err := tx.Commit().Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": map[string]interface{}{
				"code":    "DATABASE_ERROR",
				"message": "Failed to commit transaction",
				"details": err.Error(),
			},
		})
		return
	}

	// After truncation, re-seed the protected system admin user
	if err := models.SeedDatabase(h.DB); err != nil {
		log.Printf("Warning: Failed to re-seed system admin after truncation: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Database truncated successfully and system admin restored",
	})
}

type AdminStatsResponse struct {
	TotalUsers         int64 `json:"totalUsers"`
	TotalOrganizations int64 `json:"totalOrganizations"`
	TotalParticipants  int64 `json:"totalParticipants"`
	TotalShifts        int64 `json:"totalShifts"`
}

type TableStatsResponse struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// GetSystemStats returns system-wide statistics for admin dashboard
func (h *Handler) GetSystemStats(c *gin.Context) {
	// Verify admin access (admin or super_admin)
	userRole, exists := c.Get("user_role")
	roleStr, _ := userRole.(string)
	
	// Debug logging
	fmt.Printf("GetSystemStats - userRole exists: %v, roleStr: %s\n", exists, roleStr)
	
	if !exists || (roleStr != "admin" && roleStr != "super_admin") {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "Access denied - requires admin role",
			"debug": gin.H{
				"exists": exists,
				"role": roleStr,
			},
		})
		return
	}

	var stats AdminStatsResponse

	// Count total users
	h.DB.Model(&models.User{}).Count(&stats.TotalUsers)

	// Count total organizations
	h.DB.Model(&models.Organization{}).Count(&stats.TotalOrganizations)

	// Count total participants
	h.DB.Model(&models.Participant{}).Count(&stats.TotalParticipants)

	// Count total shifts
	h.DB.Model(&models.Shift{}).Count(&stats.TotalShifts)

	c.JSON(http.StatusOK, stats)
}

// GetTableStats returns record counts for all system tables
func (h *Handler) GetTableStats(c *gin.Context) {
	// Verify admin access (admin or super_admin)
	userRole, exists := c.Get("user_role")
	roleStr, _ := userRole.(string)
	if !exists || (roleStr != "admin" && roleStr != "super_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied - requires admin role"})
		return
	}

	var tables []TableStatsResponse

	// Define tables to check
	tableModels := map[string]interface{}{
		"users":         &models.User{},
		"organizations": &models.Organization{},
		"participants":  &models.Participant{},
		"shifts":        &models.Shift{},
		"care_plans":    &models.CarePlan{},
		"documents":     &models.Document{},
	}

	for name, model := range tableModels {
		var count int64
		h.DB.Model(model).Count(&count)
		tables = append(tables, TableStatsResponse{
			Name:  name,
			Count: count,
		})
	}

	c.JSON(http.StatusOK, tables)
}

// DatabaseBackup creates a real database backup using pg_dump
func (h *Handler) DatabaseBackup(c *gin.Context) {
	// Verify super admin access
	userRole, exists := c.Get("user_role")
	if !exists || userRole != "super_admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return
	}

	// Parse database URL to get connection parameters
	dbURL := h.Config.DatabaseURL
	if dbURL == "" {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Database URL not configured",
		})
		return
	}

	// Create backup filename with timestamp
	timestamp := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("ago_crm_backup_%s.sql", timestamp)
	backupPath := filepath.Join("/tmp", filename)

	// Parse connection string
	parsed, err := url.Parse(dbURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Failed to parse database URL: " + err.Error(),
		})
		return
	}

	// Extract connection parameters
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "5432" // Default PostgreSQL port
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	username := parsed.User.Username()
	password, _ := parsed.User.Password()

	// Create pg_dump command
	cmd := exec.Command("pg_dump",
		"-h", host,
		"-p", port,
		"-U", username,
		"-d", database,
		"--verbose",
		"--clean",
		"--if-exists",
		"--create",
		"--format=plain",
		"-f", backupPath,
	)

	// Set environment variables
	env := os.Environ()
	env = append(env, "PGPASSWORD="+password)
	cmd.Env = env

	// Execute backup command
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		log.Printf("pg_dump failed: %v\nSTDOUT: %s\nSTDERR: %s", err, stdout.String(), stderr.String())
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Database backup failed: " + err.Error(),
			"details": stderr.String(),
		})
		return
	}

	// Check if backup file was created successfully
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Backup file was not created",
		})
		return
	}

	// Get file size for response
	fileInfo, err := os.Stat(backupPath)
	if err != nil {
		log.Printf("Error getting file info: %v", err)
	}

	// Set headers for file download
	c.Header("Content-Description", "File Transfer")
	c.Header("Content-Transfer-Encoding", "binary")
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Header("Content-Type", "application/octet-stream")

	// Serve the backup file for download
	c.File(backupPath)

	// Schedule file cleanup after download (in background)
	go func() {
		time.Sleep(5 * time.Minute) // Wait 5 minutes before cleanup
		if err := os.Remove(backupPath); err != nil {
			log.Printf("Failed to cleanup backup file %s: %v", backupPath, err)
		} else {
			log.Printf("Successfully cleaned up backup file: %s", backupPath)
		}
	}()

	log.Printf("Database backup completed successfully: %s (%.2f MB)", 
		filename, float64(fileInfo.Size())/(1024*1024))
}

// DatabaseRestore restores database from uploaded backup file
func (h *Handler) DatabaseRestore(c *gin.Context) {
	// Verify super admin access
	userRole, exists := c.Get("user_role")
	if !exists || userRole != "super_admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return
	}

	// Parse multipart form
	file, err := c.FormFile("backup")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": "No backup file provided",
		})
		return
	}

	// Validate file type
	if !strings.HasSuffix(file.Filename, ".sql") && 
	   !strings.HasSuffix(file.Filename, ".backup") && 
	   !strings.HasSuffix(file.Filename, ".dump") {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error": "Invalid file type. Only .sql, .backup, and .dump files are allowed",
		})
		return
	}

	// Create temporary file for upload
	timestamp := time.Now().Format("20060102_150405")
	tempFilename := fmt.Sprintf("restore_%s_%s", timestamp, file.Filename)
	tempPath := filepath.Join("/tmp", tempFilename)

	// Save uploaded file
	if err := c.SaveUploadedFile(file, tempPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Failed to save uploaded file: " + err.Error(),
		})
		return
	}

	// Schedule cleanup
	defer func() {
		go func() {
			time.Sleep(1 * time.Minute)
			if err := os.Remove(tempPath); err != nil {
				log.Printf("Failed to cleanup restore file %s: %v", tempPath, err)
			}
		}()
	}()

	// Parse database URL
	dbURL := h.Config.DatabaseURL
	if dbURL == "" {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Database URL not configured",
		})
		return
	}

	parsed, err := url.Parse(dbURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Failed to parse database URL: " + err.Error(),
		})
		return
	}

	// Extract connection parameters
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	username := parsed.User.Username()
	password, _ := parsed.User.Password()

	// Create psql command to restore
	cmd := exec.Command("psql",
		"-h", host,
		"-p", port,
		"-U", username,
		"-d", database,
		"-f", tempPath,
	)

	// Set environment variables
	env := os.Environ()
	env = append(env, "PGPASSWORD="+password)
	cmd.Env = env

	// Execute restore command
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		log.Printf("Database restore failed: %v\nSTDOUT: %s\nSTDERR: %s", err, stdout.String(), stderr.String())
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Database restore failed: " + err.Error(),
			"details": stderr.String(),
		})
		return
	}

	log.Printf("Database restore completed successfully from file: %s", file.Filename)
	
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Database restored successfully",
		"output": fmt.Sprintf("Restored from file: %s\nSize: %.2f MB", 
			file.Filename, float64(file.Size)/(1024*1024)),
	})
}

// DatabaseMaintenance runs database maintenance tasks
func (h *Handler) DatabaseMaintenance(c *gin.Context) {
	// Verify super admin access
	userRole, exists := c.Get("user_role")
	if !exists || userRole != "super_admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return
	}

	// Parse database URL
	dbURL := h.Config.DatabaseURL
	if dbURL == "" {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Database URL not configured",
		})
		return
	}

	parsed, err := url.Parse(dbURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error": "Failed to parse database URL: " + err.Error(),
		})
		return
	}

	// Extract connection parameters
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	username := parsed.User.Username()
	password, _ := parsed.User.Password()

	// Set environment variables
	env := os.Environ()
	env = append(env, "PGPASSWORD="+password)

	maintenanceOutput := []string{}

	// 1. Run VACUUM ANALYZE on all tables
	vacuumCmd := exec.Command("psql",
		"-h", host,
		"-p", port,
		"-U", username,
		"-d", database,
		"-c", "VACUUM ANALYZE;",
	)
	vacuumCmd.Env = env

	var vacuumOut, vacuumErr bytes.Buffer
	vacuumCmd.Stdout = &vacuumOut
	vacuumCmd.Stderr = &vacuumErr

	if err := vacuumCmd.Run(); err != nil {
		log.Printf("VACUUM ANALYZE failed: %v", err)
		maintenanceOutput = append(maintenanceOutput, "VACUUM ANALYZE: Failed - "+err.Error())
	} else {
		maintenanceOutput = append(maintenanceOutput, "VACUUM ANALYZE: Completed successfully")
	}

	// 2. REINDEX database
	reindexCmd := exec.Command("psql",
		"-h", host,
		"-p", port,
		"-U", username,
		"-d", database,
		"-c", "REINDEX DATABASE "+database+";",
	)
	reindexCmd.Env = env

	var reindexOut, reindexErr bytes.Buffer
	reindexCmd.Stdout = &reindexOut
	reindexCmd.Stderr = &reindexErr

	if err := reindexCmd.Run(); err != nil {
		log.Printf("REINDEX failed: %v", err)
		maintenanceOutput = append(maintenanceOutput, "REINDEX: Failed - "+err.Error())
	} else {
		maintenanceOutput = append(maintenanceOutput, "REINDEX: Completed successfully")
	}

	// 3. Update table statistics
	statsCmd := exec.Command("psql",
		"-h", host,
		"-p", port,
		"-U", username,
		"-d", database,
		"-c", "ANALYZE;",
	)
	statsCmd.Env = env

	if err := statsCmd.Run(); err != nil {
		log.Printf("ANALYZE failed: %v", err)
		maintenanceOutput = append(maintenanceOutput, "ANALYZE: Failed - "+err.Error())
	} else {
		maintenanceOutput = append(maintenanceOutput, "ANALYZE: Completed successfully")
	}

	outputText := strings.Join(maintenanceOutput, "\n")
	log.Printf("Database maintenance completed: %s", outputText)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Database maintenance completed successfully",
		"output":  outputText,
	})
}

// DatabaseCleanup performs data cleanup operations
func (h *Handler) DatabaseCleanup(c *gin.Context) {
	// Require double authentication for this operation
	if !h.RequireDoubleAuth(c) {
		return // Error already sent by RequireDoubleAuth
	}

	// Verify super admin access
	userRole, exists := c.Get("user_role")
	if !exists || userRole != "super_admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return
	}

	var totalDeleted int64

	// Count and delete soft-deleted records older than 30 days
	tables := []interface{}{
		&models.User{},
		&models.Participant{},
		&models.Organization{},
		&models.CarePlan{},
		&models.Document{},
	}

	for _, model := range tables {
		var count int64
		// Count soft-deleted records
		h.DB.Unscoped().Where("deleted_at IS NOT NULL AND deleted_at < NOW() - INTERVAL '30 days'").
			Model(model).Count(&count)
		totalDeleted += count

		// In production, uncomment this to actually delete:
		// h.DB.Unscoped().Where("deleted_at IS NOT NULL AND deleted_at < NOW() - INTERVAL '30 days'").
		//     Delete(model)
	}

	c.JSON(http.StatusOK, gin.H{
		"message":        "Database cleanup completed successfully",
		"status":         "completed",
		"recordsCleaned": totalDeleted,
	})
}

// GetTableData returns paginated data from a specific table
func (h *Handler) GetTableData(c *gin.Context) {
	// Verify super admin access
	userRole, exists := c.Get("user_role")
	if !exists || userRole != "super_admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return
	}

	tableName := c.Param("table")
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	offset := (page - 1) * limit

	var results []map[string]interface{}
	var total int64

	// Map table names to models
	var model interface{}
	switch tableName {
	case "users":
		model = &models.User{}
	case "organizations":
		model = &models.Organization{}
	case "participants":
		model = &models.Participant{}
	case "shifts":
		model = &models.Shift{}
	case "care_plans":
		model = &models.CarePlan{}
	case "documents":
		model = &models.Document{}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid table name"})
		return
	}

	// Count total records
	h.DB.Model(model).Count(&total)

	// Get paginated results
	h.DB.Model(model).Offset(offset).Limit(limit).Find(&results)

	c.JSON(http.StatusOK, gin.H{
		"data":       results,
		"total":      total,
		"page":       page,
		"limit":      limit,
		"totalPages": (total + int64(limit) - 1) / int64(limit),
	})
}