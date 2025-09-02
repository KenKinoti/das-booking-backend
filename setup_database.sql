-- DAS Booking Database Setup Script
-- Run this in PostgreSQL to create the database

-- Create database (run this as postgres user)
CREATE DATABASE das_booking_db
    WITH 
    OWNER = postgres
    ENCODING = 'UTF8'
    LOCALE_PROVIDER = 'libc'
    CONNECTION LIMIT = -1
    IS_TEMPLATE = False;

-- Grant privileges
GRANT ALL ON DATABASE das_booking_db TO postgres;
GRANT TEMPORARY, CONNECT ON DATABASE das_booking_db TO PUBLIC;

-- Connect to the database
\c das_booking_db;

-- The Go application will automatically create tables using GORM AutoMigrate
-- Tables that will be created:
-- - users
-- - organizations  
-- - customers
-- - bookings
-- - services
-- - staff

-- Create indexes for better performance (optional, GORM will handle basic ones)
-- These will be created after running the Go application

COMMENT ON DATABASE das_booking_db IS 'DAS Booking - DASYIN Booking Platform Database';

-- Show database info
SELECT 
    pg_database.datname as "Database",
    pg_size_pretty(pg_database_size(pg_database.datname)) as "Size",
    pg_encoding_to_char(encoding) as "Encoding"
FROM pg_database 
WHERE datname = 'das_booking_db';

\echo 'Database das_booking_db created successfully!'
\echo 'You can now run the Go backend server.'