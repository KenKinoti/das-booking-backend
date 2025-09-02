@echo off
echo ========================================
echo DAS Booking - PostgreSQL Database Setup
echo ========================================
echo.

echo This script will create the PostgreSQL database.
echo.
echo Prerequisites:
echo   - PostgreSQL is installed and running
echo   - PostgreSQL is accessible on localhost:5432
echo   - You have postgres user credentials
echo.

pause

echo [1/2] Testing PostgreSQL connection...
psql -h localhost -p 5432 -U postgres -c "SELECT version();"

if %errorlevel% neq 0 (
    echo.
    echo ERROR: Could not connect to PostgreSQL!
    echo.
    echo Please check:
    echo   - PostgreSQL service is running
    echo   - Port 5432 is available  
    echo   - User 'postgres' exists with password 'admin'
    echo.
    pause
    exit /b 1
)

echo.
echo [2/2] Creating database das_booking_db...
psql -h localhost -p 5432 -U postgres -f setup_database.sql

if %errorlevel% equ 0 (
    echo.
    echo ✅ Database setup completed successfully!
    echo.
    echo Next steps:
    echo   1. Run: run-postgres.bat
    echo   2. The Go server will auto-create tables
    echo   3. Sample data will be inserted automatically
    echo.
) else (
    echo.
    echo ❌ Database setup failed!
    echo Please check the error messages above.
    echo.
)

pause