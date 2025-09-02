@echo off
echo ========================================
echo DAS Booking - PostgreSQL Backend Server
echo ========================================
echo.

REM Check if Go is installed
where go >nul 2>nul
if %errorlevel% neq 0 (
    echo ERROR: Go is not installed or not in PATH
    echo Please install Go from https://golang.org/dl/
    pause
    exit /b 1
)

echo [1/4] Setting environment variables...
set ENVIRONMENT=development
set DATABASE_URL=postgres://postgres:admin@localhost:5432/das_booking_db?sslmode=disable
set DB_HOST=localhost
set DB_PORT=5432
set DB_USER=postgres
set DB_PASSWORD=admin
set DB_NAME=das_booking_db
set DB_SSLMODE=disable
set JWT_SECRET=your-secret-key-change-in-production
set JWT_EXPIRY=24h
set REFRESH_TOKEN_EXPIRY=168h
set PORT=8080
set MAX_FILE_SIZE=10MB
set UPLOAD_PATH=./uploads

echo [2/4] Checking PostgreSQL connection...
echo   Database: %DB_NAME%
echo   Host: %DB_HOST%:%DB_PORT%
echo   User: %DB_USER%
echo.
echo IMPORTANT: Make sure PostgreSQL is running and database exists!
echo   - Start PostgreSQL service
echo   - Create database: CREATE DATABASE das_booking_db;

echo.
echo [3/4] Downloading Go dependencies...
go mod download

echo [4/4] Starting PostgreSQL server on http://localhost:%PORT%...
echo.
echo Press Ctrl+C to stop the server
echo ----------------------------------------
go run postgres_server.go