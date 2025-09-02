package database

import (
	"fmt"
	"strings"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func Initialize(databaseURL string) (*gorm.DB, error) {
	var dialector gorm.Dialector

	if strings.HasPrefix(databaseURL, "postgres://") {
		dialector = postgres.Open(databaseURL)
	} else if strings.HasPrefix(databaseURL, "mysql://") {
		// Convert mysql:// to proper MySQL DSN
		dsn := strings.TrimPrefix(databaseURL, "mysql://")
		dialector = mysql.Open(dsn)
	} else if strings.HasPrefix(databaseURL, "sqlite://") {
		dbPath := strings.TrimPrefix(databaseURL, "sqlite://")
		dialector = sqlite.Open(dbPath)
	} else {
		return nil, fmt.Errorf("unsupported database URL: %s", databaseURL)
	}

	config := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	}

	db, err := gorm.Open(dialector, config)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	return db, nil
}
