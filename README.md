# DASYIN Unified ERP & Booking System

A comprehensive business management system combining booking appointments with ERP functionality.

## Features

### 📅 **Booking Management**
- Appointment scheduling and management
- Customer and service management
- Vehicle tracking (for garage businesses)
- Available time slot checking

### 💰 **Financial Management**
- Chart of accounts with double-entry bookkeeping
- Financial dashboards and reporting
- Account management system

### 👥 **Customer Relationship Management**
- Lead capture and conversion tracking
- Customer 360° view and management
- Sales pipeline and opportunity tracking

### 📦 **Inventory Management**
- Product catalog with SKU management
- Stock level tracking and alerts
- Inventory valuation and reporting

## Quick Start

### Prerequisites
- Go 1.21+
- PostgreSQL database

### Environment Setup
Create `.env` file:
```env
DATABASE_URL=postgres://postgres:admin@localhost:5432/das_booking_db?sslmode=disable
JWT_SECRET=your_jwt_secret_here
ENVIRONMENT=development
PORT=8080
```

### Run Application
```bash
# Start the unified system
go run cmd/app/main.go
```

### API Endpoints

**Health Check:** `GET /api/v1/health`

**Booking Module:** `/api/v1/booking/*`
- `GET|POST /bookings` - Manage appointments
- `GET|POST /customers` - Customer management
- `GET|POST /services` - Service catalog
- `GET|POST /vehicles` - Vehicle tracking
- `GET /dashboard` - Booking analytics

**Finance Module:** `/api/v1/finance/*`
- `GET|POST /chart-of-accounts` - Account management
- `GET /dashboard` - Financial metrics

**CRM Module:** `/api/v1/crm/*`
- `GET|POST /leads` - Lead management
- `GET|POST /customers` - CRM customers
- `GET|POST /opportunities` - Sales pipeline
- `GET /dashboard` - CRM analytics

**Inventory Module:** `/api/v1/inventory/*`
- `GET|POST /products` - Product catalog
- `GET /dashboard` - Inventory metrics

**Admin Module:** `/api/v1/admin/*`
- `GET /overview` - System statistics
- `GET /modules` - Module status

## Database Setup

The system uses your existing `das_booking_db` database and automatically creates the necessary ERP tables on first run.

## Architecture

- **Single Binary:** One Go application handling all modules
- **Modular Design:** Clean separation of booking, finance, CRM, and inventory
- **Shared Database:** Enhanced PostgreSQL schema supporting both booking and ERP
- **RESTful APIs:** Consistent JSON API across all modules

## Development

```bash
# Install dependencies
go mod tidy

# Run in development mode
ENVIRONMENT=development go run cmd/app/main.go

# Build for production
go build -o bin/dasyin-erp cmd/app/main.go
```

## Production Deployment

```bash
# Build optimized binary
CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bin/dasyin-erp cmd/app/main.go

# Run with production settings
ENVIRONMENT=production ./bin/dasyin-erp
```

---

**Built with Go, Gin, GORM, and PostgreSQL**