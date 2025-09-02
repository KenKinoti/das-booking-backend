package main

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/database"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
)

var (
	firstNames = []string{"John", "Jane", "Michael", "Sarah", "David", "Emma", "James", "Lisa", "Robert", "Maria", "William", "Jennifer", "Thomas", "Patricia", "Christopher", "Linda"}
	lastNames  = []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Miller", "Davis", "Wilson", "Moore", "Taylor", "Anderson", "Thomas", "Jackson", "White", "Harris", "Martin"}
	streets    = []string{"Main St", "High St", "Park Ave", "Oak Rd", "Elm St", "Church St", "First Ave", "Second Ave", "Third Ave", "King St", "Queen St", "Victoria St"}
	suburbs    = []string{"Sydney", "Melbourne", "Brisbane", "Perth", "Adelaide", "Gold Coast", "Newcastle", "Canberra", "Wollongong", "Sunshine Coast"}
	
	// Medical conditions for participants
	conditions = []string{"Diabetes Type 2", "Hypertension", "Autism Spectrum Disorder", "Cerebral Palsy", "Down Syndrome", "Epilepsy", "Multiple Sclerosis", "Spinal Cord Injury"}
	medications = []string{"Metformin", "Lisinopril", "Risperidone", "Baclofen", "Valproate", "Carbamazepine", "Interferon beta", "Gabapentin"}
	allergies = []string{"None", "Penicillin", "Peanuts", "Latex", "Shellfish", "None", "None"} // More "None" for realistic distribution
)

type Seeder struct {
	db *gorm.DB
}

func NewSeeder(db *gorm.DB) *Seeder {
	return &Seeder{db: db}
}

func main() {
	// Load environment variables
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, using environment variables")
	}

	// Load config
	cfg := config.Load()

	// Connect to database using the database package
	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	seeder := NewSeeder(db)

	fmt.Println("Starting database seeding...")
	fmt.Println("=====================================")

	// Seed data
	if err := seeder.SeedAll(); err != nil {
		log.Fatal("Seeding failed:", err)
	}

	fmt.Println("=====================================")
	fmt.Println("Database seeding completed successfully!")
	fmt.Println("\nTo remove all test data, run: go run cmd/seed/main.go --clean")
}

func (s *Seeder) SeedAll() error {
	// Check if we should clean instead
	if len(os.Args) > 1 && os.Args[1] == "--clean" {
		return s.CleanTestData()
	}

	// Create organizations
	orgs, err := s.SeedOrganizations()
	if err != nil {
		return fmt.Errorf("failed to seed organizations: %w", err)
	}

	// Create users for each organization
	users := make(map[string][]models.User)
	for _, org := range orgs {
		orgUsers, err := s.SeedUsers(org.ID)
		if err != nil {
			return fmt.Errorf("failed to seed users for org %s: %w", org.Name, err)
		}
		users[org.ID] = orgUsers
	}

	// Create participants for each organization
	participants := make(map[string][]models.Participant)
	for _, org := range orgs {
		orgParticipants, err := s.SeedParticipants(org.ID)
		if err != nil {
			return fmt.Errorf("failed to seed participants for org %s: %w", org.Name, err)
		}
		participants[org.ID] = orgParticipants
	}

	// Create shifts for each organization
	for _, org := range orgs {
		if err := s.SeedShifts(org.ID, users[org.ID], participants[org.ID]); err != nil {
			return fmt.Errorf("failed to seed shifts for org %s: %w", org.Name, err)
		}
	}

	// Create care plans
	for _, orgParticipants := range participants {
		for _, participant := range orgParticipants {
			if err := s.SeedCarePlan(participant.ID); err != nil {
				return fmt.Errorf("failed to seed care plan: %w", err)
			}
		}
	}

	// Print summary
	s.PrintSummary()

	return nil
}

func (s *Seeder) SeedOrganizations() ([]models.Organization, error) {
	orgs := []models.Organization{
		{
			ID:      uuid.New().String(),
			Name:    "Sunshine Care Services",
			ABN:     "12345678901",
			Phone:   "+61 2 9876 5432",
			Email:   "admin@sunshinecare.com.au",
			Website: "https://sunshinecare.com.au",
			Address: models.Address{
				Street:   "123 Care Street",
				Suburb:   "Sydney",
				State:    "NSW",
				Postcode: "2000",
				Country:  "Australia",
			},
			NDISReg: models.NDISReg{
				RegistrationNumber: "NDIS123456",
				RegistrationStatus: "active",
				ExpiryDate:         &[]time.Time{time.Now().AddDate(2, 0, 0)}[0],
			},
		},
		{
			ID:      uuid.New().String(),
			Name:    "Melbourne Support Network",
			ABN:     "98765432109",
			Phone:   "+61 3 8765 4321",
			Email:   "info@melbournesupport.com.au",
			Website: "https://melbournesupport.com.au",
			Address: models.Address{
				Street:   "456 Support Avenue",
				Suburb:   "Melbourne",
				State:    "VIC",
				Postcode: "3000",
				Country:  "Australia",
			},
			NDISReg: models.NDISReg{
				RegistrationNumber: "NDIS789012",
				RegistrationStatus: "active",
				ExpiryDate:         &[]time.Time{time.Now().AddDate(1, 6, 0)}[0],
			},
		},
	}

	for i, org := range orgs {
		var existingOrg models.Organization
		if err := s.db.Where("abn = ?", org.ABN).First(&existingOrg).Error; err == nil {
			// Organization already exists, use existing one
			fmt.Printf("• Organization already exists: %s\n", existingOrg.Name)
			orgs[i] = existingOrg
		} else {
			// Create new organization
			if err := s.db.Create(&org).Error; err != nil {
				return nil, err
			}
			fmt.Printf("✓ Created organization: %s\n", org.Name)
			orgs[i] = org
		}
	}

	return orgs, nil
}

func (s *Seeder) SeedUsers(orgID string) ([]models.User, error) {
	// Use 'password' to match frontend mock authentication
	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.DefaultCost)
	
	users := []models.User{}

	// Get org info to determine which org we're creating users for
	var org models.Organization
	if err := s.db.First(&org, "id = ?", orgID).Error; err != nil {
		return nil, err
	}

	// Create frontend test users for the first organization (regardless of name)
	var firstOrg models.Organization
	if err := s.db.First(&firstOrg).Error; err == nil && org.ID == firstOrg.ID {
		frontendTestUsers := []models.User{
			{
				ID:             uuid.New().String(),
				Email:          "admin@dasyin.com.au",
				PasswordHash:   string(hashedPassword),
				FirstName:      "System",
				LastName:       "Administrator",
				Phone:          "+61 400 000 001",
				Role:           "super_admin",
				OrganizationID: orgID,
				IsActive:       true,
			},
			{
				ID:             uuid.New().String(),
				Email:          "kennedy@dasyin.com.au",
				PasswordHash:   string(hashedPassword),
				FirstName:      "Ken",
				LastName:       "Kinoti",
				Phone:          "+61 400 000 002",
				Role:           "admin",
				OrganizationID: orgID,
				IsActive:       true,
			},
			{
				ID:             uuid.New().String(),
				Email:          "manager@dasyin.com.au",
				PasswordHash:   string(hashedPassword),
				FirstName:      "Sarah",
				LastName:       "Wilson",
				Phone:          "+61 400 000 003",
				Role:           "manager",
				OrganizationID: orgID,
				IsActive:       true,
			},
			{
				ID:             uuid.New().String(),
				Email:          "coordinator@dasyin.com.au",
				PasswordHash:   string(hashedPassword),
				FirstName:      "Lisa",
				LastName:       "Johnson",
				Phone:          "+61 400 000 004",
				Role:           "support_coordinator",
				OrganizationID: orgID,
				IsActive:       true,
			},
			{
				ID:             uuid.New().String(),
				Email:          "careworker@dasyin.com.au",
				PasswordHash:   string(hashedPassword),
				FirstName:      "John",
				LastName:       "Smith",
				Phone:          "+61 400 000 005",
				Role:           "care_worker",
				OrganizationID: orgID,
				IsActive:       true,
			},
		}

		// Create each frontend test user
		for _, user := range frontendTestUsers {
			var existingUser models.User
			if err := s.db.Where("email = ?", user.Email).First(&existingUser).Error; err == nil {
				// User already exists
				fmt.Printf("• Frontend test user already exists: %s\n", existingUser.Email)
				users = append(users, existingUser)
			} else {
				// Create new user
				if err := s.db.Create(&user).Error; err != nil {
					return nil, err
				}
				users = append(users, user)
				fmt.Printf("✓ Created frontend test user: %s (%s)\n", user.Email, user.Role)
			}
		}
	}

	// Create org 2 admin for second organization (regardless of name)
	var secondOrg models.Organization
	if err := s.db.Offset(1).First(&secondOrg).Error; err == nil && org.ID == secondOrg.ID {
		org2Admin := models.User{
			ID:             uuid.New().String(),
			Email:          "org2admin@dasyin.com.au",
			PasswordHash:   string(hashedPassword),
			FirstName:      "Mark",
			LastName:       "Davis",
			Phone:          "+61 400 000 006",
			Role:           "admin",
			OrganizationID: orgID,
			IsActive:       true,
		}

		var existingUser models.User
		if err := s.db.Where("email = ?", org2Admin.Email).First(&existingUser).Error; err == nil {
			// User already exists
			fmt.Printf("• Org 2 admin already exists: %s\n", existingUser.Email)
			users = append(users, existingUser)
		} else {
			// Create new user
			if err := s.db.Create(&org2Admin).Error; err != nil {
				return nil, err
			}
			users = append(users, org2Admin)
			fmt.Printf("✓ Created org 2 admin: %s\n", org2Admin.Email)
		}
	}

	// Also create some random test users for bulk data
	roles := []string{"admin", "manager", "support_coordinator", "care_worker"}
	hashedTestPassword, _ := bcrypt.GenerateFromPassword([]byte("Test123!@#"), bcrypt.DefaultCost)

	// Create 2 random test users per role for bulk testing data
	for _, role := range roles {
		for i := 0; i < 2; i++ {
			firstName := firstNames[rand.Intn(len(firstNames))]
			lastName := lastNames[rand.Intn(len(lastNames))]
			
			// Make some users inactive for testing (10% chance)
			isActive := true
			if rand.Intn(10) == 0 { // 10% chance to be inactive
				isActive = false
			}
			
			user := models.User{
				ID:             uuid.New().String(),
				Email:          fmt.Sprintf("%s.%s%d@test.com", firstName, lastName, rand.Intn(100)),
				PasswordHash:   string(hashedTestPassword), // Use Test123!@# for random users
				FirstName:      firstName,
				LastName:       lastName,
				Phone:          fmt.Sprintf("+61 4%02d %03d %03d", rand.Intn(100), rand.Intn(1000), rand.Intn(1000)),
				Role:           role,
				OrganizationID: orgID,
				IsActive:       isActive,
			}
			
			var existingUser models.User
			if err := s.db.Where("email = ?", user.Email).First(&existingUser).Error; err == nil {
				// User already exists, skip
				fmt.Printf("• Random test user already exists: %s\n", existingUser.Email)
				users = append(users, existingUser)
			} else {
				// Create new user
				if err := s.db.Create(&user).Error; err != nil {
					return nil, err
				}
				users = append(users, user)
				fmt.Printf("✓ Created random test user: %s (%s)\n", user.Email, user.Role)
			}
		}
	}

	fmt.Printf("✓ Created %d users for organization\n", len(users))
	return users, nil
}

func (s *Seeder) SeedParticipants(orgID string) ([]models.Participant, error) {
	participants := []models.Participant{}
	
	// Create 15-20 participants per organization
	numParticipants := 15 + rand.Intn(6)
	
	for i := 0; i < numParticipants; i++ {
		firstName := firstNames[rand.Intn(len(firstNames))]
		lastName := lastNames[rand.Intn(len(lastNames))]
		
		// Random date of birth between 18 and 80 years ago
		age := 18 + rand.Intn(63)
		dob := time.Now().AddDate(-age, -rand.Intn(12), -rand.Intn(28))
		
		participant := models.Participant{
			ID:          uuid.New().String(),
			FirstName:   firstName,
			LastName:    lastName,
			DateOfBirth: dob,
			NDISNumber:  fmt.Sprintf("%09d", rand.Intn(1000000000)),
			Email:       fmt.Sprintf("%s.%s.participant@email.com", firstName, lastName),
			Phone:       fmt.Sprintf("+61 4%02d %03d %03d", rand.Intn(100), rand.Intn(1000), rand.Intn(1000)),
			Address: models.Address{
				Street:   fmt.Sprintf("%d %s", rand.Intn(999)+1, streets[rand.Intn(len(streets))]),
				Suburb:   suburbs[rand.Intn(len(suburbs))],
				State:    []string{"NSW", "VIC", "QLD", "WA", "SA", "TAS"}[rand.Intn(6)],
				Postcode: fmt.Sprintf("%04d", 2000+rand.Intn(6000)),
				Country:  "Australia",
			},
			MedicalInfo: models.MedicalInformation{
				Conditions:  conditions[rand.Intn(len(conditions))],
				Medications: medications[rand.Intn(len(medications))],
				Allergies:   allergies[rand.Intn(len(allergies))],
			},
			Funding: models.FundingInformation{
				PlanStartDate: &[]time.Time{time.Now().AddDate(0, -rand.Intn(12), 0)}[0],
				PlanEndDate:   &[]time.Time{time.Now().AddDate(1, rand.Intn(12), 0)}[0],
				TotalBudget:   float64(50000 + rand.Intn(150000)),
				UsedBudget:    float64(10000 + rand.Intn(40000)),
				BudgetYear:    "2024-2025",
			},
			OrganizationID: orgID,
			IsActive:       rand.Float32() > 0.1, // 90% active
		}
		
		if err := s.db.Create(&participant).Error; err != nil {
			return nil, err
		}
		
		// Create 1-2 emergency contacts per participant
		numContacts := 1 + rand.Intn(2)
		for j := 0; j < numContacts; j++ {
			contact := models.EmergencyContact{
				ID:            uuid.New().String(),
				ParticipantID: participant.ID,
				Name:          fmt.Sprintf("%s %s", firstNames[rand.Intn(len(firstNames))], lastNames[rand.Intn(len(lastNames))]),
				Relationship:  []string{"Parent", "Sibling", "Spouse", "Child", "Friend", "Guardian"}[rand.Intn(6)],
				Phone:         fmt.Sprintf("+61 4%02d %03d %03d", rand.Intn(100), rand.Intn(1000), rand.Intn(1000)),
				Email:         fmt.Sprintf("emergency%d@contact.com", rand.Intn(1000)),
				IsPrimary:     j == 0,
				IsActive:      true,
			}
			if err := s.db.Create(&contact).Error; err != nil {
				return nil, err
			}
		}
		
		participants = append(participants, participant)
	}
	
	fmt.Printf("✓ Created %d participants with emergency contacts\n", len(participants))
	return participants, nil
}

func (s *Seeder) SeedShifts(orgID string, users []models.User, participants []models.Participant) error {
	// Filter care workers only
	careWorkers := []models.User{}
	for _, user := range users {
		if user.Role == "care_worker" || user.Role == "support_coordinator" {
			careWorkers = append(careWorkers, user)
		}
	}
	
	if len(careWorkers) == 0 || len(participants) == 0 {
		return nil
	}
	
	shiftCount := 0
	// Create shifts for the past month and upcoming week
	for days := -30; days <= 7; days++ {
		// Create 5-10 shifts per day, more for critical days (next 2 days)
		numShifts := 5 + rand.Intn(6)
		if days >= 0 && days <= 2 {
			numShifts += 3 // Extra shifts in critical timeframe for ETA visibility
		}
		
		for i := 0; i < numShifts; i++ {
			shiftDate := time.Now().AddDate(0, 0, days)
			startHour := 6 + rand.Intn(12) // Between 6 AM and 6 PM
			duration := 2 + rand.Intn(7)    // 2-8 hours
			
			startTime := time.Date(shiftDate.Year(), shiftDate.Month(), shiftDate.Day(), startHour, 0, 0, 0, time.Local)
			endTime := startTime.Add(time.Duration(duration) * time.Hour)
			
			// Determine status based on date
			status := "scheduled"
			if days < -7 {
				status = "completed"
			} else if days < 0 {
				statusOptions := []string{"completed", "in_progress", "cancelled"}
				status = statusOptions[rand.Intn(len(statusOptions))]
			}
			
			shift := models.Shift{
				ID:            uuid.New().String(),
				ParticipantID: participants[rand.Intn(len(participants))].ID,
				StaffID:       careWorkers[rand.Intn(len(careWorkers))].ID,
				StartTime:     startTime,
				EndTime:       endTime,
				Status:        status,
				ServiceType:   []string{"Personal Care", "Community Access", "Domestic Assistance", "Transport", "Respite"}[rand.Intn(5)],
				Location:      []string{"Participant Home", "Community Center", "Day Program", "Medical Appointment"}[rand.Intn(4)],
				Notes:         "Seeded test shift - can be safely deleted",
				HourlyRate:    65.00 + float64(rand.Intn(30)),
			}
			
			if status == "completed" {
				actualStart := startTime.Add(time.Duration(-rand.Intn(15)) * time.Minute)
				actualEnd := endTime.Add(time.Duration(rand.Intn(30)-15) * time.Minute)
				shift.ActualStartTime = &actualStart
				shift.ActualEndTime = &actualEnd
			}
			
			if err := s.db.Create(&shift).Error; err != nil {
				return err
			}
			shiftCount++
		}
	}
	
	fmt.Printf("✓ Created %d shifts\n", shiftCount)
	return nil
}

func (s *Seeder) SeedCarePlan(participantID string) error {
	goals := []string{
		"Improve daily living skills",
		"Increase social participation", 
		"Maintain physical health",
		"Develop communication skills",
		"Achieve greater independence",
	}
	
	// Get a random coordinator to be the creator
	var coordinator models.User
	if err := s.db.Where("role = ?", "support_coordinator").First(&coordinator).Error; err != nil {
		return err
	}
	
	startDate := time.Now().AddDate(0, -rand.Intn(6), 0)
	endDate := startDate.AddDate(1, rand.Intn(6), 0)
	
	carePlan := models.CarePlan{
		ID:            uuid.New().String(),
		ParticipantID: participantID,
		Title:         "NDIS Support Plan " + time.Now().Format("2006"),
		Description:   "Comprehensive support framework designed to meet participant needs",
		StartDate:     startDate,
		EndDate:       &endDate,
		Goals:         goals[rand.Intn(len(goals))],
		Status:        []string{"active", "completed", "cancelled"}[rand.Intn(3)],
		CreatedBy:     coordinator.ID,
	}
	
	if err := s.db.Create(&carePlan).Error; err != nil {
		return err
	}
	
	return nil
}

func (s *Seeder) CleanTestData() error {
	fmt.Println("Cleaning all test data...")
	fmt.Println("=====================================")
	
	// Delete in reverse order of dependencies
	tables := []string{
		"care_plan_items",
		"care_plans",
		"shift_notes",
		"shifts",
		"emergency_contacts",
		"documents",
		"participants",
		"refresh_tokens",
		"user_permissions",
		"users",
		"organizations",
	}
	
	for _, table := range tables {
		result := s.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE notes LIKE '%%Seeded test%%' OR email LIKE '%%@test.com' OR email LIKE '%%@sunshinecare.com.au' OR email LIKE '%%@melbournesupport.com.au' OR name LIKE '%%Sunshine Care%%' OR name LIKE '%%Melbourne Support%%'", table))
		if result.Error != nil {
			// Try without WHERE clause for complete cleanup (be careful!)
			result = s.db.Exec(fmt.Sprintf("TRUNCATE TABLE %s", table))
			if result.Error != nil {
				fmt.Printf("⚠ Could not clean table %s: %v\n", table, result.Error)
				continue
			}
		}
		fmt.Printf("✓ Cleaned table: %s (removed %d records)\n", table, result.RowsAffected)
	}
	
	fmt.Println("=====================================")
	fmt.Println("Test data cleaned successfully!")
	return nil
}

func (s *Seeder) PrintSummary() {
	var orgCount, userCount, participantCount, shiftCount int64
	
	s.db.Model(&models.Organization{}).Count(&orgCount)
	s.db.Model(&models.User{}).Count(&userCount)
	s.db.Model(&models.Participant{}).Count(&participantCount)
	s.db.Model(&models.Shift{}).Count(&shiftCount)
	
	fmt.Println("\n=====================================")
	fmt.Println("SEEDING SUMMARY")
	fmt.Println("=====================================")
	fmt.Printf("Organizations: %d\n", orgCount)
	fmt.Printf("Users:         %d\n", userCount)
	fmt.Printf("Participants:  %d\n", participantCount)
	fmt.Printf("Shifts:        %d\n", shiftCount)
	fmt.Println("=====================================")
	fmt.Println("\nFRONTEND TEST USERS (password: 'password'):")
	fmt.Println("• admin@dasyin.com.au - Super Admin")
	fmt.Println("• kennedy@dasyin.com.au - Org Admin")
	fmt.Println("• manager@dasyin.com.au - Manager")
	fmt.Println("• coordinator@dasyin.com.au - Support Coordinator")
	fmt.Println("• careworker@dasyin.com.au - Care Worker")
	fmt.Println("• org2admin@dasyin.com.au - Org 2 Admin")
	fmt.Println("\nRANDOM TEST USERS (password: 'Test123!@#'):")
	fmt.Println("• Various @test.com users for bulk data testing")
	fmt.Println("=====================================")
}