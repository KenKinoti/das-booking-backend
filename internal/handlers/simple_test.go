package handlers

import (
	"testing"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"time"
)

func TestDatabaseConnection(t *testing.T) {
	// Setup test database
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	assert.NoError(t, err, "Should be able to connect to test database")
	
	// Run basic migration
	err = db.AutoMigrate(
		&models.Organization{},
		&models.User{},
		&models.Customer{},
	)
	assert.NoError(t, err, "Should be able to run migrations")
}

func TestConfigCreation(t *testing.T) {
	cfg := &config.Config{
		JWTSecret:          "test-secret-key",
		JWTExpiry:          24 * time.Hour,
		RefreshTokenExpiry: 7 * 24 * time.Hour,
	}
	
	assert.NotNil(t, cfg, "Config should be created")
	assert.Equal(t, "test-secret-key", cfg.JWTSecret, "JWT Secret should be set correctly")
	assert.Equal(t, 24 * time.Hour, cfg.JWTExpiry, "JWT Expiry should be set correctly")
}

func TestHandlerCreation(t *testing.T) {
	// Setup test database
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	assert.NoError(t, err, "Should be able to connect to test database")
	
	// Setup test configuration
	cfg := &config.Config{
		JWTSecret:          "test-secret-key",
		JWTExpiry:          24 * time.Hour,
		RefreshTokenExpiry: 7 * 24 * time.Hour,
	}
	
	// Create handler
	handler := NewHandler(db, cfg)
	
	assert.NotNil(t, handler, "Handler should be created successfully")
	assert.NotNil(t, handler.DB, "Handler should have database connection")
}