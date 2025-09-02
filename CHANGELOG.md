# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **Authentication System & Super Admin Access**
  - Fixed kennedy@dasyin.com.au login credentials - updated password from "password" to "Test123!@#"
  - Fixed organization assignment for kennedy account to ensure proper data access
  - Updated Vue frontend login form default password to match backend credentials
  - Resolved "no data displayed" issue for super admin users after login

- **Role-Based Data Access Control**
  - Implemented super admin role hierarchy allowing full system access across all organizations
  - Fixed participants handler to allow super_admin users to see all 103 participants system-wide
  - Fixed users handler to allow super_admin users to see all 52 users system-wide
  - Fixed shifts handler to allow super_admin users to see all 1719 shifts system-wide
  - Maintained organization-based restrictions for admin, manager, and other non-super admin roles
  - Added role-based query logic in GetUsers, UpdateUser, DeleteUser, GetParticipants, and GetShifts handlers

- **Database Permission System**
  - Created fix-kennedy utility script to update super admin credentials and organization assignment
  - Verified role hierarchy: super_admin > admin > manager > support_coordinator > care_worker
  - Ensured proper data isolation for regular organizational users while providing system-wide access for super admins

- **Frontend Debug Console Statements**
  - Removed all debug console.log statements from Vue components and stores
  - Kept essential error logging (console.error and console.warn) for proper error handling
  - Improved code cleanliness and reduced console noise in production

- **Shift Management System**
  - Fixed start shift button functionality on both frontend and backend
  - Updated frontend services to use correct API endpoints for shift status updates
  - Simplified shift status transitions using proper store methods
  - Corrected API calls for starting, completing, and cancelling shifts

- **Time Handling Improvements**
  - Enhanced API time parsing to accept multiple date/time formats for easier frontend integration
  - Added support for local timezone handling without requiring complex timezone conversions
  - Improved CreateShiftRequest and UpdateShiftRequest to accept time strings instead of complex Time objects
  - Added parseTimeFromString utility function supporting ISO, RFC3339, and local time formats

### Enhanced
- **Super Admin Capabilities**
  - kennedy@dasyin.com.au now has full system CRUD access across all organizations
  - Super admin can view and manage participants from all organizations (103 total)
  - Super admin can view and manage users from all organizations (52 total)
  - Super admin can view and manage shifts from all organizations (1719 total)
  - Regular admin users remain restricted to their organization's data

- **Participant-Staff Allocation**
  - Verified participant-staff allocation functionality works correctly
  - Confirmed proper organization-based access control for shifts
  - Validated staff overlap conflict detection prevents double-booking
  - Ensured participant and staff validation during shift creation

### Technical Improvements
- **Role-Based Access Control**: Implemented hierarchical permission system with super admin override
- **API Reliability**: All shift operations now use consistent status endpoint (`PATCH /shifts/:id/status`)
- **Error Handling**: Improved error messages and validation for time format issues
- **Code Quality**: Removed redundant optimistic UI updates in favor of server-driven state management
- **Authentication Flow**: Complete login → JWT → role-based data access flow verified working

## [1.0.1] - 2025-08-29

### Fixed
- **Build and Compilation Issues**
  - Fixed duplicate `CreateEmergencyContactRequest` struct declarations in participants.go and emergency_contacts.go
  - Renamed participants.go struct to `CreateParticipantEmergencyContactRequest` to avoid conflicts
  - Removed unused imports (`time` from organization.go, `strings` from shifts.go)
  - Fixed invalid date string literals in test files - converted to proper `time.Date()` calls
  - Removed invalid `OrganizationID` field references in shift struct literals (field doesn't exist in Shift model)
  - Fixed missing `generateToken` method in users_test.go - implemented proper JWT token generation for tests

- **Authentication System**  
  - Verified JWT authentication middleware is working correctly
  - Confirmed `/users/me` endpoint properly validates authentication tokens
  - Validated proper error responses for missing/invalid tokens
  - Fixed test compilation issues preventing authentication validation

- **Test Infrastructure**
  - Fixed all Go compilation errors preventing test execution
  - Updated test imports and dependencies 
  - Fixed date format issues in participant and shift test fixtures
  - Corrected JWT token generation in test helper functions
  - All integration tests now compile and run successfully

### Technical
- **Code Quality**: Resolved all compilation warnings and errors
- **Test Coverage**: All test files now compile without errors
- **Authentication Flow**: Validated complete login → JWT → protected endpoint flow
- **API Endpoints**: Confirmed `/api/v1/users/me` endpoint works correctly with Bearer token authentication

### Added
- Comprehensive CRUD operations for all CRM entities
- Complete API endpoints for Users, Participants, Shifts, Documents, Organizations
- Emergency Contacts management system
- Care Plans management with approval workflow
- File upload and download functionality for documents
- Advanced filtering and pagination for all endpoints
- Organization-based access control across all endpoints
- Role-based permissions (admin, manager, care_worker, support_coordinator)
- Proper error handling with consistent error response format
- Input validation for all request types
- Shift status management with transition validation
- Document categorization and expiry date tracking
- Emergency contact priority management
- Care plan approval workflow for admin/manager roles

### Enhanced
- **Users Handler** (`/api/v1/users`)
  - `GET /` - List all users with filtering, pagination, and search
  - `POST /` - Create new user with role validation and password hashing
  - `PUT /:id` - Update user details with access control
  - `DELETE /:id` - Soft delete users with self-protection
  - `GET /me` - Get current authenticated user details

- **Participants Handler** (`/api/v1/participants`)
  - `GET /` - List participants with comprehensive filtering
  - `GET /:id` - Get participant details with related data
  - `POST /` - Create participant with embedded emergency contacts
  - `PUT /:id` - Update participant with NDIS number validation
  - `DELETE /:id` - Soft delete participant

- **Shifts Handler** (`/api/v1/shifts`)
  - `GET /` - List shifts with date range and status filtering
  - `GET /:id` - Get detailed shift information
  - `POST /` - Create shift with overlap validation
  - `PUT /:id` - Update shift details with conflict checking
  - `PATCH /:id/status` - Update shift status with transition validation
  - `DELETE /:id` - Delete scheduled/cancelled shifts only

- **Documents Handler** (`/api/v1/documents`)
  - `GET /` - List documents with category and participant filtering
  - `GET /:id` - Get document metadata
  - `POST /` - Upload document with file validation (10MB limit)
  - `PUT /:id` - Update document metadata
  - `DELETE /:id` - Soft delete document
  - `GET /:id/download` - Download document file

- **Organization Handler** (`/api/v1/organization`)
  - `GET /` - Get organization details
  - `PUT /` - Update organization information (admin only)

- **Emergency Contacts Handler** (`/api/v1/emergency-contacts`)
  - `GET /` - List emergency contacts for participant
  - `GET /:id` - Get emergency contact details
  - `POST /` - Create emergency contact with primary contact management
  - `PUT /:id` - Update emergency contact information
  - `DELETE /:id` - Delete emergency contact

- **Care Plans Handler** (`/api/v1/care-plans`)
  - `GET /` - List care plans with status and participant filtering
  - `GET /:id` - Get detailed care plan information
  - `POST /` - Create new care plan
  - `PUT /:id` - Update care plan details
  - `PATCH /:id/approve` - Approve/reject care plan (admin/manager only)
  - `DELETE /:id` - Delete care plan (exclude completed ones)

### Security
- Organization-based data isolation - users can only access data from their organization
- Role-based access control with middleware enforcement
- Proper authentication checks on all protected endpoints
- Self-protection mechanisms (users can't delete/deactivate themselves)
- File upload validation with size limits and type checking
- Input sanitization and validation across all endpoints

### Technical Improvements
- Consistent error response format across all endpoints
- Comprehensive request validation using struct tags
- Proper HTTP status codes for different scenarios
- Transaction support for complex operations
- Optimized database queries with proper joins and preloading
- Consistent pagination implementation
- Search functionality with case-insensitive matching
- Date range filtering with proper validation
- Soft delete implementation for audit trails

### Database Enhancements
- Enhanced models with proper relationships
- Composite indexes for better query performance
- Proper foreign key constraints
- Embedded structs for address, medical info, and funding details
- UUID-based primary keys with auto-generation
- Timestamps tracking (created_at, updated_at)
- Soft delete support with deleted_at field

### API Features
- RESTful API design following standard conventions
- JSON request/response format with consistent structure
- Query parameter support for filtering and pagination
- File upload support with multipart/form-data
- Proper Content-Type headers for file downloads
- CORS support for frontend integration
- Middleware-based authentication and authorization

### Data Management
- Participant medical information tracking
- NDIS funding budget management with calculations
- Shift cost calculation based on hourly rates and duration
- Document expiry date tracking and validation
- Emergency contact priority system
- Care plan lifecycle management with approval workflow

### Validation & Business Logic
- NDIS number uniqueness validation
- Email format validation
- Date range validation for shifts and care plans
- Staff schedule conflict detection
- Primary emergency contact management
- Care plan status transition validation
- File size and type validation for uploads

## [Previous Versions]

### [0.1.0] - Initial Setup
- Basic project structure with Go Fiber framework
- Initial database models
- Authentication system with JWT
- Basic health check endpoint
- Database migration support
- Environment configuration setup