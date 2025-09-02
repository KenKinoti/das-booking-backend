package main

import (
	"fmt"
	"log"

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

	var users []models.User
	if err := db.Where("email LIKE ?", "%@test.com").Or("email LIKE ?", "%@system.com").Find(&users).Error; err != nil {
		log.Fatal("Failed to query users:", err)
	}

	fmt.Println("🔐 SEEDED TEST USER CREDENTIALS")
	fmt.Println("=====================================")
	fmt.Println("Password for ALL users: Test123!@#")
	fmt.Println("=====================================")
	
	roleGroups := make(map[string][]models.User)
	for _, user := range users {
		roleGroups[user.Role] = append(roleGroups[user.Role], user)
	}
	
	for role, roleUsers := range roleGroups {
		fmt.Printf("\n%s USERS:\n", role)
		for _, user := range roleUsers {
			fmt.Printf("  📧 %s (%s %s)\n", user.Email, user.FirstName, user.LastName)
		}
	}
	fmt.Println("\n=====================================")
	fmt.Println("Use any email above with password: Test123!@#")
}