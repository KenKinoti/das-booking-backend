package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

func main() {
	// Initialize database
	db, err := gorm.Open(sqlite.Open("org_test.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Auto-migrate models
	if err := db.AutoMigrate(&models.Organization{}); err != nil {
		log.Fatal("Migration failed:", err)
	}

	// Create test organizations
	organizations := []models.Organization{
		{
			ID:          uuid.New().String(),
			Name:        "DAS Transport Solutions",
			BusinessType: "transportation",
			ABN:         "123456789",
			Phone:       "+61412345678",
			Email:       "admin@dastransport.com",
			Website:     "https://dastransport.com",
			Description: "Professional transportation and logistics services",
			Address: models.Address{
				Street:   "123 Transport Street",
				Suburb:   "Adelaide",
				State:    "SA",
				Postcode: "5000",
				Country:  "Australia",
			},
			BusinessHours: models.BusinessHours{
				Timezone: "Australia/Adelaide",
			},
			BookingSettings: models.BookingSettings{
				EnableOnlineBooking: true,
				BookingWindow:      30,
				MinAdvanceBooking:  1,
				MaxAdvanceBooking:  720,
				DefaultSlotDuration: 60,
				BufferTime:         15,
				RequireApproval:    false,
				SendConfirmations:  true,
				SendReminders:      true,
				ReminderHours:      24,
				AllowCancellation:  true,
				CancellationWindow: 24,
			},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		{
			ID:          uuid.New().String(),
			Name:        "Adelaide Auto Services",
			BusinessType: "automotive",
			ABN:         "987654321",
			Phone:       "+61412345677",
			Email:       "admin@adelaideauto.com",
			Website:     "https://adelaideauto.com",
			Description: "Complete automotive repair and maintenance services",
			Address: models.Address{
				Street:   "456 Mechanic Road",
				Suburb:   "North Adelaide",
				State:    "SA",
				Postcode: "5006",
				Country:  "Australia",
			},
			BusinessHours: models.BusinessHours{
				Timezone: "Australia/Adelaide",
			},
			BookingSettings: models.BookingSettings{
				EnableOnlineBooking: true,
				BookingWindow:      14,
				MinAdvanceBooking:  2,
				MaxAdvanceBooking:  90,
				DefaultSlotDuration: 120,
				BufferTime:         30,
				RequireApproval:    true,
				SendConfirmations:  true,
				SendReminders:      true,
				ReminderHours:      48,
				AllowCancellation:  true,
				CancellationWindow: 48,
			},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		{
			ID:          uuid.New().String(),
			Name:        "Premier Hair Studio",
			BusinessType: "beauty",
			ABN:         "555666777",
			Phone:       "+61412345676",
			Email:       "admin@premierhair.com",
			Website:     "https://premierhair.com",
			Description: "Premium hair styling and beauty services",
			Address: models.Address{
				Street:   "789 Beauty Boulevard",
				Suburb:   "Unley",
				State:    "SA",
				Postcode: "5061",
				Country:  "Australia",
			},
			BusinessHours: models.BusinessHours{
				Timezone: "Australia/Adelaide",
			},
			BookingSettings: models.BookingSettings{
				EnableOnlineBooking: true,
				BookingWindow:      21,
				MinAdvanceBooking:  1,
				MaxAdvanceBooking:  60,
				DefaultSlotDuration: 90,
				BufferTime:         15,
				RequireApproval:    false,
				SendConfirmations:  true,
				SendReminders:      true,
				ReminderHours:      24,
				AllowCancellation:  true,
				CancellationWindow: 12,
			},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}

	// Insert organizations
	for i, org := range organizations {
		// Handle modules as text fields for SQLite
		var enabledModulesJSON, modulesConfigJSON []byte

		if i == 0 {
			// DAS Transport Solutions - full modules
			enabledModulesJSON, _ = json.Marshal([]string{"booking", "crm", "hr", "inventory", "finance"})
			modulesConfigJSON, _ = json.Marshal(map[string]bool{
				"booking":    true,
				"crm":        true,
				"hr":         true,
				"inventory":  true,
				"finance":    true,
				"reports":    true,
			})
		} else if i == 1 {
			// Adelaide Auto Services - limited modules (booking, crm, hr)
			enabledModulesJSON, _ = json.Marshal([]string{"booking", "crm", "hr"})
			modulesConfigJSON, _ = json.Marshal(map[string]bool{
				"booking":    true,
				"crm":        true,
				"hr":         true,
			})
		} else {
			// Premier Hair Studio - beauty focused modules
			enabledModulesJSON, _ = json.Marshal([]string{"booking", "crm"})
			modulesConfigJSON, _ = json.Marshal(map[string]bool{
				"booking":    true,
				"crm":        true,
			})
		}

		// Manually set the JSON fields
		if err := db.Exec(`
			INSERT INTO organizations (
				id, name, business_type, abn, phone, email, website, description,
				address_street, address_suburb, address_state, address_postcode, address_country,
				hours_timezone,
				booking_enable_online_booking, booking_booking_window, booking_min_advance_booking,
				booking_max_advance_booking, booking_default_slot_duration, booking_buffer_time,
				booking_require_approval, booking_send_confirmations, booking_send_reminders,
				booking_reminder_hours, booking_allow_cancellation, booking_cancellation_window,
				enabled_modules, modules_config, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			org.ID, org.Name, org.BusinessType, org.ABN, org.Phone, org.Email, org.Website, org.Description,
			org.Address.Street, org.Address.Suburb, org.Address.State, org.Address.Postcode, org.Address.Country,
			org.BusinessHours.Timezone,
			org.BookingSettings.EnableOnlineBooking, org.BookingSettings.BookingWindow, org.BookingSettings.MinAdvanceBooking,
			org.BookingSettings.MaxAdvanceBooking, org.BookingSettings.DefaultSlotDuration, org.BookingSettings.BufferTime,
			org.BookingSettings.RequireApproval, org.BookingSettings.SendConfirmations, org.BookingSettings.SendReminders,
			org.BookingSettings.ReminderHours, org.BookingSettings.AllowCancellation, org.BookingSettings.CancellationWindow,
			string(enabledModulesJSON), string(modulesConfigJSON), org.CreatedAt, org.UpdatedAt,
		).Error; err != nil {
			log.Printf("Failed to insert organization %s: %v", org.Name, err)
		} else {
			fmt.Printf("✅ Created organization: %s (%s)\n", org.Name, org.ID)
		}
	}

	fmt.Printf("\n🎉 Successfully created %d test organizations!\n", len(organizations))

	// Verify organizations were created
	var count int64
	db.Model(&models.Organization{}).Count(&count)
	fmt.Printf("📊 Total organizations in database: %d\n", count)
}