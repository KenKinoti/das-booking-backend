package main

import (
	"fmt"
	"log"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func main() {
	// Database connection using your actual credentials
	dsn := "host=localhost user=postgres password=postgres dbname=das_booking_db port=5432 sslmode=disable"
	
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	fmt.Println("Connected to database successfully!")

	// Check if user exists first
	var count int64
	db.Table("users").Where("email = ?", "kennedy@dasyin.com.au").Count(&count)
	if count == 0 {
		fmt.Println("User kennedy@dasyin.com.au not found in database")
		return
	}

	fmt.Printf("Found user kennedy@dasyin.com.au in database\n")

	// Hash the password "password"
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatal("Failed to hash password:", err)
	}

	fmt.Printf("Generated password hash: %s\n", string(hashedPassword))

	// Update the user's password
	result := db.Exec("UPDATE users SET password_hash = ?, updated_at = NOW() WHERE email = ?", string(hashedPassword), "kennedy@dasyin.com.au")
	if result.Error != nil {
		log.Fatal("Failed to update password:", result.Error)
	}

	fmt.Printf("Password updated successfully for kennedy@dasyin.com.au\n")
	fmt.Printf("Rows affected: %d\n", result.RowsAffected)
	
	// Verify the update
	var user struct {
		Email        string `json:"email"`
		FirstName    string `json:"first_name"`
		LastName     string `json:"last_name"`
		PasswordHash string `json:"password_hash"`
	}
	
	db.Table("users").Select("email, first_name, last_name, password_hash").Where("email = ?", "kennedy@dasyin.com.au").First(&user)
	
	fmt.Printf("User details:\n")
	fmt.Printf("  Email: %s\n", user.Email)
	fmt.Printf("  Name: %s %s\n", user.FirstName, user.LastName)
	fmt.Printf("  Password hash updated: %s\n", user.PasswordHash[:50]+"...")
}
