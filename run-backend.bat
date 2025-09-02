@echo off
echo ========================================
echo Starting AGO CRM Backend Server
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

echo [1/3] Setting environment variables...
set ENVIRONMENT=development
set DATABASE_URL=sqlite://./care_crm.db
set JWT_SECRET=your-secret-key-change-in-production
set JWT_EXPIRY=24h
set REFRESH_TOKEN_EXPIRY=168h
set PORT=8080
set MAX_FILE_SIZE=10MB
set UPLOAD_PATH=./uploads

echo [2/3] Checking dependencies...
go mod download

echo [3/3] Starting server on http://localhost:8080...
echo.
echo Press Ctrl+C to stop the server
echo ----------------------------------------
go run test_server.go