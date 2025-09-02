# Dynamic Booking Platform - Backend API

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org/doc/devel/release.html)
[![Gin Framework](https://img.shields.io/badge/Gin-v1.9+-00ADD8?style=flat&logo=gin)](https://gin-gonic.com/)
[![GORM](https://img.shields.io/badge/GORM-v1.25+-00ADD8?style=flat)](https://gorm.io/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Build Status](https://img.shields.io/badge/Build-Passing-brightgreen.svg)](https://github.com/kenkinoti/gofiber-ago-crm-backend)

A comprehensive **Multi-Business Booking Platform** built with Go, Gin Framework, and GORM. Designed for businesses of all types - automotive services, salons, professional services, and more - to manage customers, bookings, services, and staff scheduling with complete flexibility.

## 🌟 Features

### Core Functionality
- 🔐 **User Authentication & Authorization** - JWT-based auth with role-based access control
- 👥 **Customer Management** - Complete customer profiles with service history tracking
- 🚗 **Vehicle Management** - For automotive businesses with VIN, mileage, and service tracking
- 📅 **Advanced Booking System** - Time slot management with availability checking
- 🛠️ **Service Catalog** - Flexible service definitions with pricing and duration
- 📊 **Business Types** - Support for garage, salon, healthcare, professional services
- 🏢 **Organization Management** - Multi-tenant architecture with business-specific settings
- ⏰ **Business Hours** - Configurable operating hours and booking windows

### Technical Features
- 🚀 **RESTful API** - Clean, standard-compliant REST endpoints
- 🔍 **Advanced Filtering** - Comprehensive search and filter capabilities
- 📄 **Pagination** - Efficient data loading with page-based navigation
- 🔒 **Data Security** - Organization-based data isolation and role permissions
- 📝 **Input Validation** - Comprehensive request validation and sanitization
- 🗄️ **Database Support** - PostgreSQL and SQLite with optimized queries
- 📤 **File Handling** - Secure file upload with validation and download
- 🎯 **Conflict Prevention** - Automatic booking conflict detection

## 📋 Table of Contents

- [Requirements](#requirements)
- [Installation](#installation)
- [Configuration](#configuration)
- [Running the Application](#running-the-application)
- [API Documentation](#api-documentation)
- [Testing](#testing)
- [Database Schema](#database-schema)
- [Security](#security)
- [Contributing](#contributing)
- [License](#license)

## 🔧 Requirements

- Go 1.21 or higher
- PostgreSQL 13+ or SQLite 3.35+
- Git

## 🚀 Installation

### 1. Clone the Repository

```bash
git clone https://github.com/yourusername/booking-platform-backend.git
cd booking-platform-backend
```

### 2. Install Dependencies

```bash
go mod download
```

### 3. Set Up Environment Variables

Create a `.env` file in the root directory:

```env
# Application
ENVIRONMENT=development
PORT=8080

# Database (PostgreSQL)
DATABASE_URL=postgres://username:password@localhost:5432/booking_platform?sslmode=disable

# Or SQLite (for development)
DATABASE_URL=sqlite://./booking_platform.db

# JWT Configuration
JWT_SECRET=your-secret-key-change-in-production
JWT_EXPIRY=24h
REFRESH_TOKEN_EXPIRY=168h

# File Upload
MAX_FILE_SIZE=10MB
UPLOAD_PATH=./uploads
```

### 4. Run Database Migrations

```bash
go run main.go
```

The application will automatically run migrations on startup.

## 🏃 Running the Application

### Development Mode

```bash
go run main.go
```

### Production Build

```bash
go build -o booking-platform
./booking-platform
```

The API will be available at `http://localhost:8080`

## 📚 API Documentation

### Base URL
```
http://localhost:8080/api/v1
```

### Authentication Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/auth/register` | Register new user |
| POST | `/auth/login` | User login |
| POST | `/auth/refresh` | Refresh access token |
| POST | `/auth/logout` | User logout |
| GET | `/auth/me` | Get current user |

### Customer Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/customers` | List all customers |
| POST | `/customers` | Create new customer |
| GET | `/customers/:id` | Get customer details |
| PUT | `/customers/:id` | Update customer |
| DELETE | `/customers/:id` | Delete customer |
| GET | `/customers/:id/vehicles` | List customer vehicles |
| POST | `/customers/:id/vehicles` | Add vehicle to customer |

### Booking Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/bookings` | List all bookings |
| POST | `/bookings` | Create new booking |
| GET | `/bookings/:id` | Get booking details |
| PUT | `/bookings/:id` | Update booking |
| DELETE | `/bookings/:id` | Cancel booking |
| GET | `/bookings/availability` | Check time slot availability |
| POST | `/bookings/:id/confirm` | Confirm booking |
| POST | `/bookings/:id/complete` | Mark booking as complete |

### Service Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/services` | List all services |
| POST | `/services` | Create new service |
| GET | `/services/:id` | Get service details |
| PUT | `/services/:id` | Update service |
| DELETE | `/services/:id` | Delete service |

### Vehicle Management (for automotive businesses)

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/vehicles` | List all vehicles |
| POST | `/vehicles` | Create new vehicle |
| GET | `/vehicles/:id` | Get vehicle details |
| PUT | `/vehicles/:id` | Update vehicle |
| DELETE | `/vehicles/:id` | Delete vehicle |
| GET | `/vehicles/:id/history` | Get vehicle service history |

### Business Configuration

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/organizations/:id` | Get organization details |
| PUT | `/organizations/:id` | Update organization settings |
| PUT | `/organizations/:id/hours` | Update business hours |
| PUT | `/organizations/:id/booking-settings` | Update booking settings |

## 🧪 Testing

### Run All Tests

```bash
go test ./...
```

### Run Tests with Coverage

```bash
go test -cover ./...
```

### Run Specific Test

```bash
go test -run TestName ./internal/handlers
```

## 🗄️ Database Schema

### Core Tables

- **organizations** - Business/tenant information
- **users** - System users and staff
- **customers** - Customer profiles
- **vehicles** - Vehicle information (for automotive)
- **services** - Service catalog
- **bookings** - Booking records
- **booking_services** - Services included in bookings
- **business_hours** - Operating hours

### Key Features

- Multi-tenant data isolation
- Soft deletes for data recovery
- Audit timestamps (created_at, updated_at)
- Optimized indexes for performance

## 🔒 Security

- **JWT Authentication** - Secure token-based authentication
- **Role-Based Access Control** - Granular permission system
- **Data Isolation** - Organization-level data separation
- **Input Validation** - Comprehensive request validation
- **SQL Injection Prevention** - Parameterized queries via GORM
- **Password Hashing** - Bcrypt for secure password storage
- **CORS Configuration** - Configurable cross-origin policies

## 🤝 Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/AmazingFeature`)
3. Commit your changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## 🙏 Acknowledgments

- Gin Web Framework
- GORM ORM Library
- JWT-Go for authentication
- Go community

## 📞 Support

For support, email support@bookingplatform.com or open an issue in the GitHub repository.

---

Built with ❤️ for businesses worldwide