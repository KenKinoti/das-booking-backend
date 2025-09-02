package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Environment        string
	DatabaseURL        string
	JWTSecret          string
	JWTExpiry          time.Duration
	RefreshTokenExpiry time.Duration
	Port               string
	MaxFileSize        int64
	UploadPath         string
	CORSAllowedOrigins []string
	SMTPHost           string
	SMTPPort           int
	SMTPUsername       string
	SMTPPassword       string
}

func Load() *Config {
	return &Config{
		Environment:        getEnv("ENVIRONMENT", "development"),
		DatabaseURL:        getEnv("DATABASE_URL", "sqlite://./care_crm.db"),
		JWTSecret:          getEnv("JWT_SECRET", "default-secret-change-me"),
		JWTExpiry:          parseDuration(getEnv("JWT_EXPIRY", "24h")),
		RefreshTokenExpiry: parseDuration(getEnv("REFRESH_TOKEN_EXPIRY", "168h")),
		Port:               getEnv("PORT", "8080"),
		MaxFileSize:        parseSize(getEnv("MAX_FILE_SIZE", "10MB")),
		UploadPath:         getEnv("UPLOAD_PATH", "./uploads"),
		SMTPHost:           getEnv("SMTP_HOST", ""),
		SMTPPort:           parseInt(getEnv("SMTP_PORT", "587")),
		SMTPUsername:       getEnv("SMTP_USERNAME", ""),
		SMTPPassword:       getEnv("SMTP_PASSWORD", ""),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 24 * time.Hour
	}
	return d
}

func parseInt(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return i
}

func parseSize(s string) int64 {
	// Simple MB parser
	if len(s) > 2 && s[len(s)-2:] == "MB" {
		if i, err := strconv.ParseInt(s[:len(s)-2], 10, 64); err == nil {
			return i * 1024 * 1024
		}
	}
	return 10 * 1024 * 1024 // Default 10MB
}
