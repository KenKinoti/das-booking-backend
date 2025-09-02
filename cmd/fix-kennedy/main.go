package main

import (
	"fmt"
	"log"

	"golang.org/x/crypto/bcrypt"
	"github.com/joho/godotenv"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/database"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found")
	}

	cfg := config.Load()
	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Get kennedy's current info
	var kennedy models.User
	if err := db.Where("email = ?", "kennedy@dasyin.com.au").First(&kennedy).Error; err != nil {
		log.Fatal("Kennedy not found:", err)
	}

	fmt.Printf("Kennedy before update:\n")
	fmt.Printf("ID: %s\nEmail: %s\nRole: %s\nOrg ID: %s\n\n", kennedy.ID, kennedy.Email, kennedy.Role, kennedy.OrganizationID)

	// Get first organization from seeded data
	var org models.Organization
	if err := db.First(&org).Error; err != nil {
		log.Fatal("No organization found:", err)
	}

	fmt.Printf("Using organization: %s (ID: %s)\n\n", org.Name, org.ID)

	// Hash the new password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte("Test123!@#"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatal("Failed to hash password:", err)
	}

	// Update kennedy with new password and organization
	kennedy.PasswordHash = string(hashedPassword)
	kennedy.OrganizationID = org.ID
	kennedy.Role = "super_admin" // Ensure super admin role

	if err := db.Save(&kennedy).Error; err != nil {
		log.Fatal("Failed to update kennedy:", err)
	}

	fmt.Printf("Kennedy updated successfully!\n")
	fmt.Printf("New password: Test123!@#\n")
	fmt.Printf("New organization: %s\n", org.Name)
	fmt.Printf("Role: %s\n", kennedy.Role)

	// Show organization data counts
	var userCount, participantCount, shiftCount int64
	db.Model(&models.User{}).Where("organization_id = ?", org.ID).Count(&userCount)
	db.Model(&models.Participant{}).Where("organization_id = ?", org.ID).Count(&participantCount)
	
	// For shifts, we need to join with users to get the organization
	db.Table("shifts").
		Joins("JOIN users ON shifts.staff_id = users.id").
		Where("users.organization_id = ?", org.ID).
		Count(&shiftCount)

	fmt.Printf("\nOrganization %s data:\n", org.Name)
	fmt.Printf("Users: %d\n", userCount)
	fmt.Printf("Participants: %d\n", participantCount)
	fmt.Printf("Shifts: %d\n", shiftCount)
}