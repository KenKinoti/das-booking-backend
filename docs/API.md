# API Documentation

This document provides detailed information about all API endpoints, including request/response schemas, data types, and examples.

## Base URL

```
Development: http://localhost:8080
Production: https://your-domain.com
```

## Authentication

All protected endpoints require a valid JWT token in the Authorization header:

```
Authorization: Bearer <your-jwt-token>
```

## Response Format

All responses follow a consistent format:

```json
{
  "success": true|false,
  "data": {},
  "message": "string",
  "error": {
    "code": "ERROR_CODE",
    "message": "Error description",
    "details": "Additional error details"
  }
}
```

## Data Types

### Common Types

- **UUID**: String in UUID format (e.g., "123e4567-e89b-12d3-a456-426614174000")
- **DateTime**: ISO 8601 format (e.g., "2023-12-01T10:30:00Z")
- **Date**: Date only format (e.g., "2023-12-01")
- **Decimal**: Floating point numbers for monetary values
- **Phone**: String format (e.g., "+61412345678")
- **Email**: Valid email format (e.g., "user@example.com")

---

## 🔐 Authentication Endpoints

### POST /api/v1/auth/login

User login with email and password.

**Request Body:**
```json
{
  "email": "string (required, email format)",
  "password": "string (required, min 8 characters)"
}
```

**Example Request:**
```json
{
  "email": "kennedy@dasyin.com.au",
  "password": "securepassword123"
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "token": "string (JWT access token)",
    "refresh_token": "string (JWT refresh token)",
    "expires_in": "number (seconds until token expires)",
    "user": {
      "id": "string (UUID)",
      "email": "string",
      "first_name": "string",
      "last_name": "string",
      "phone": "string|null",
      "role": "string (admin|manager|care_worker|support_coordinator)",
      "organization_id": "string (UUID)",
      "is_active": "boolean",
      "last_login_at": "string (DateTime)|null",
      "created_at": "string (DateTime)",
      "updated_at": "string (DateTime)"
    }
  },
  "message": "Login successful"
}
```

**Error Response (401):**
```json
{
  "success": false,
  "error": {
    "code": "INVALID_CREDENTIALS",
    "message": "Invalid email or password"
  }
}
```

### POST /api/v1/auth/refresh

Refresh JWT access token using refresh token.

**Request Body:**
```json
{
  "refresh_token": "string (required)"
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "token": "string (new JWT access token)",
    "expires_in": "number (seconds until token expires)"
  },
  "message": "Token refreshed successfully"
}
```

### POST /api/v1/auth/logout

Logout user and revoke refresh tokens.

**Headers:** `Authorization: Bearer <token>` (required)

**Success Response (200):**
```json
{
  "success": true,
  "message": "Logged out successfully"
}
```

---

## 👤 Users Management

### GET /api/v1/users

List all users with filtering and pagination.

**Headers:** `Authorization: Bearer <token>` (required)

**Query Parameters:**
- `page`: number (optional, default: 1)
- `limit`: number (optional, default: 10, max: 100)
- `role`: string (optional, values: admin|manager|care_worker|support_coordinator)
- `is_active`: boolean (optional)
- `search`: string (optional, searches first_name, last_name, email)

**Example Request:**
```
GET /api/v1/users?page=1&limit=10&role=care_worker&is_active=true&search=john
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "users": [
      {
        "id": "string (UUID)",
        "email": "string",
        "first_name": "string",
        "last_name": "string",
        "phone": "string|null",
        "role": "string",
        "organization_id": "string (UUID)",
        "is_active": "boolean",
        "last_login_at": "string (DateTime)|null",
        "created_at": "string (DateTime)",
        "updated_at": "string (DateTime)"
      }
    ],
    "pagination": {
      "page": "number",
      "limit": "number",
      "total": "number",
      "total_pages": "number"
    }
  }
}
```

### GET /api/v1/users/me

Get current authenticated user details.

**Headers:** `Authorization: Bearer <token>` (required)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "email": "string",
    "first_name": "string",
    "last_name": "string",
    "phone": "string|null",
    "role": "string",
    "organization_id": "string (UUID)",
    "is_active": "boolean",
    "last_login_at": "string (DateTime)|null",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)"
  }
}
```

### POST /api/v1/users

Create a new user (admin only).

**Headers:** `Authorization: Bearer <token>` (required, admin role)

**Request Body:**
```json
{
  "email": "string (required, email format, unique)",
  "password": "string (required, min 8 characters)",
  "first_name": "string (required)",
  "last_name": "string (required)",
  "phone": "string (optional)",
  "role": "string (required, values: admin|manager|care_worker|support_coordinator)"
}
```

**Example Request:**
```json
{
  "email": "john.doe@dasyin.com.au",
  "password": "securepassword123",
  "first_name": "John",
  "last_name": "Doe",
  "phone": "+61412345678",
  "role": "care_worker"
}
```

**Success Response (201):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "email": "string",
    "first_name": "string",
    "last_name": "string",
    "phone": "string|null",
    "role": "string",
    "organization_id": "string (UUID)",
    "is_active": "boolean",
    "last_login_at": "null",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)"
  },
  "message": "User created successfully"
}
```

### PUT /api/v1/users/:id

Update user details.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Request Body:**
```json
{
  "first_name": "string (optional)",
  "last_name": "string (optional)",
  "phone": "string (optional)",
  "role": "string (optional, values: admin|manager|care_worker|support_coordinator)",
  "is_active": "boolean (optional)"
}
```

**Example Request:**
```json
{
  "first_name": "Johnny",
  "phone": "+61487654321",
  "is_active": true
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "email": "string",
    "first_name": "string",
    "last_name": "string",
    "phone": "string|null",
    "role": "string",
    "organization_id": "string (UUID)",
    "is_active": "boolean",
    "last_login_at": "string (DateTime)|null",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)"
  },
  "message": "User updated successfully"
}
```

### DELETE /api/v1/users/:id

Delete user (admin only, soft delete).

**Headers:** `Authorization: Bearer <token>` (required, admin role)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "message": "User deleted successfully"
}
```

---

## 👥 Participants Management

### GET /api/v1/participants

List participants with filtering and search.

**Headers:** `Authorization: Bearer <token>` (required)

**Query Parameters:**
- `page`: number (optional, default: 1)
- `limit`: number (optional, default: 10, max: 100)
- `is_active`: boolean (optional)
- `search`: string (optional, searches first_name, last_name, email)
- `ndis_number`: string (optional, exact match)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "participants": [
      {
        "id": "string (UUID)",
        "first_name": "string",
        "last_name": "string",
        "date_of_birth": "string (Date)",
        "ndis_number": "string|null",
        "email": "string|null",
        "phone": "string|null",
        "address": {
          "street": "string|null",
          "suburb": "string|null",
          "state": "string|null",
          "postcode": "string|null",
          "country": "string"
        },
        "medical_information": {
          "conditions": "string|null",
          "medications": "string|null",
          "allergies": "string|null",
          "doctor_name": "string|null",
          "doctor_phone": "string|null",
          "notes": "string|null"
        },
        "funding": {
          "total_budget": "number (decimal)",
          "used_budget": "number (decimal)",
          "remaining_budget": "number (decimal)",
          "budget_year": "string|null",
          "plan_start_date": "string (DateTime)|null",
          "plan_end_date": "string (DateTime)|null"
        },
        "organization_id": "string (UUID)",
        "is_active": "boolean",
        "created_at": "string (DateTime)",
        "updated_at": "string (DateTime)",
        "emergency_contacts": [
          {
            "id": "string (UUID)",
            "name": "string",
            "relationship": "string",
            "phone": "string",
            "email": "string|null",
            "is_primary": "boolean",
            "is_active": "boolean"
          }
        ]
      }
    ],
    "pagination": {
      "page": "number",
      "limit": "number",
      "total": "number",
      "total_pages": "number"
    }
  }
}
```

### GET /api/v1/participants/:id

Get participant details with all related data.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "first_name": "string",
    "last_name": "string",
    "date_of_birth": "string (Date)",
    "ndis_number": "string|null",
    "email": "string|null",
    "phone": "string|null",
    "address": {
      "street": "string|null",
      "suburb": "string|null",
      "state": "string|null",
      "postcode": "string|null",
      "country": "string"
    },
    "medical_information": {
      "conditions": "string|null",
      "medications": "string|null",
      "allergies": "string|null",
      "doctor_name": "string|null",
      "doctor_phone": "string|null",
      "notes": "string|null"
    },
    "funding": {
      "total_budget": "number (decimal)",
      "used_budget": "number (decimal)",
      "remaining_budget": "number (decimal)",
      "budget_year": "string|null",
      "plan_start_date": "string (DateTime)|null",
      "plan_end_date": "string (DateTime)|null"
    },
    "organization_id": "string (UUID)",
    "is_active": "boolean",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "emergency_contacts": [],
    "shifts": [],
    "documents": [],
    "care_plans": []
  }
}
```

### POST /api/v1/participants

Create a new participant.

**Headers:** `Authorization: Bearer <token>` (required)

**Request Body:**
```json
{
  "first_name": "string (required)",
  "last_name": "string (required)",
  "date_of_birth": "string (required, Date format YYYY-MM-DD)",
  "ndis_number": "string (optional, unique if provided)",
  "email": "string (optional, email format)",
  "phone": "string (optional)",
  "address": {
    "street": "string (optional)",
    "suburb": "string (optional)",
    "state": "string (optional)",
    "postcode": "string (optional)",
    "country": "string (optional, default: Australia)"
  },
  "medical_information": {
    "conditions": "string (optional, JSON array as string)",
    "medications": "string (optional, JSON array as string)",
    "allergies": "string (optional, JSON array as string)",
    "doctor_name": "string (optional)",
    "doctor_phone": "string (optional)",
    "notes": "string (optional)"
  },
  "funding": {
    "total_budget": "number (optional, decimal)",
    "used_budget": "number (optional, decimal)",
    "remaining_budget": "number (optional, decimal)",
    "budget_year": "string (optional, e.g., '2023-2024')",
    "plan_start_date": "string (optional, DateTime)",
    "plan_end_date": "string (optional, DateTime)"
  },
  "emergency_contacts": [
    {
      "name": "string (required)",
      "relationship": "string (required)",
      "phone": "string (required)",
      "email": "string (optional, email format)",
      "is_primary": "boolean (optional, default: false)"
    }
  ]
}
```

**Example Request:**
```json
{
  "first_name": "Jane",
  "last_name": "Smith",
  "date_of_birth": "1990-05-15",
  "ndis_number": "4321098765",
  "email": "jane.smith@email.com",
  "phone": "+61456789123",
  "address": {
    "street": "123 Main Street",
    "suburb": "Adelaide",
    "state": "SA",
    "postcode": "5000",
    "country": "Australia"
  },
  "medical_information": {
    "conditions": "[\"Autism\", \"Anxiety\"]",
    "medications": "[\"Medication A\"]",
    "allergies": "[\"Peanuts\"]",
    "doctor_name": "Dr. Johnson",
    "doctor_phone": "+61887654321",
    "notes": "Requires gentle approach"
  },
  "funding": {
    "total_budget": 50000.00,
    "used_budget": 12500.00,
    "remaining_budget": 37500.00,
    "budget_year": "2023-2024",
    "plan_start_date": "2023-07-01T00:00:00Z",
    "plan_end_date": "2024-06-30T23:59:59Z"
  },
  "emergency_contacts": [
    {
      "name": "John Smith",
      "relationship": "Father",
      "phone": "+61423456789",
      "email": "john.smith@email.com",
      "is_primary": true
    }
  ]
}
```

**Success Response (201):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "first_name": "string",
    "last_name": "string",
    "date_of_birth": "string (Date)",
    "ndis_number": "string|null",
    "email": "string|null",
    "phone": "string|null",
    "address": {},
    "medical_information": {},
    "funding": {},
    "organization_id": "string (UUID)",
    "is_active": "boolean",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "emergency_contacts": []
  },
  "message": "Participant created successfully"
}
```

### PUT /api/v1/participants/:id

Update participant details.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Request Body:**
```json
{
  "first_name": "string (optional)",
  "last_name": "string (optional)",
  "date_of_birth": "string (optional, Date format)",
  "ndis_number": "string (optional, unique if provided)",
  "email": "string (optional, email format)",
  "phone": "string (optional)",
  "address": {
    "street": "string (optional)",
    "suburb": "string (optional)",
    "state": "string (optional)",
    "postcode": "string (optional)",
    "country": "string (optional)"
  },
  "medical_information": {
    "conditions": "string (optional)",
    "medications": "string (optional)",
    "allergies": "string (optional)",
    "doctor_name": "string (optional)",
    "doctor_phone": "string (optional)",
    "notes": "string (optional)"
  },
  "funding": {
    "total_budget": "number (optional, decimal)",
    "used_budget": "number (optional, decimal)",
    "remaining_budget": "number (optional, decimal)",
    "budget_year": "string (optional)",
    "plan_start_date": "string (optional, DateTime)",
    "plan_end_date": "string (optional, DateTime)"
  },
  "is_active": "boolean (optional)"
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    // Updated participant object
  },
  "message": "Participant updated successfully"
}
```

### DELETE /api/v1/participants/:id

Delete participant (soft delete).

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "message": "Participant deleted successfully"
}
```

---

## 📅 Shifts Management

### GET /api/v1/shifts

List shifts with filtering and date range.

**Headers:** `Authorization: Bearer <token>` (required)

**Query Parameters:**
- `page`: number (optional, default: 1)
- `limit`: number (optional, default: 10, max: 100)
- `participant_id`: string (UUID, optional)
- `staff_id`: string (UUID, optional)
- `status`: string (optional, values: scheduled|in_progress|completed|cancelled|no_show)
- `service_type`: string (optional)
- `start_date`: string (optional, Date format YYYY-MM-DD)
- `end_date`: string (optional, Date format YYYY-MM-DD)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "shifts": [
      {
        "id": "string (UUID)",
        "participant_id": "string (UUID)",
        "staff_id": "string (UUID)",
        "start_time": "string (DateTime)",
        "end_time": "string (DateTime)",
        "actual_start_time": "string (DateTime)|null",
        "actual_end_time": "string (DateTime)|null",
        "service_type": "string",
        "location": "string",
        "status": "string",
        "hourly_rate": "number (decimal)",
        "total_cost": "number (decimal)",
        "notes": "string|null",
        "completion_notes": "string|null",
        "created_at": "string (DateTime)",
        "updated_at": "string (DateTime)",
        "participant": {
          "id": "string (UUID)",
          "first_name": "string",
          "last_name": "string"
        },
        "staff": {
          "id": "string (UUID)",
          "first_name": "string",
          "last_name": "string",
          "role": "string"
        }
      }
    ],
    "pagination": {
      "page": "number",
      "limit": "number",
      "total": "number",
      "total_pages": "number"
    }
  }
}
```

### GET /api/v1/shifts/:id

Get shift details.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "participant_id": "string (UUID)",
    "staff_id": "string (UUID)",
    "start_time": "string (DateTime)",
    "end_time": "string (DateTime)",
    "actual_start_time": "string (DateTime)|null",
    "actual_end_time": "string (DateTime)|null",
    "service_type": "string",
    "location": "string",
    "status": "string",
    "hourly_rate": "number (decimal)",
    "total_cost": "number (decimal)",
    "notes": "string|null",
    "completion_notes": "string|null",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "participant": {},
    "staff": {}
  }
}
```

### POST /api/v1/shifts

Create a new shift.

**Headers:** `Authorization: Bearer <token>` (required)

**Request Body:**
```json
{
  "participant_id": "string (required, UUID)",
  "staff_id": "string (required, UUID)",
  "start_time": "string (required, DateTime ISO format)",
  "end_time": "string (required, DateTime ISO format)",
  "service_type": "string (required)",
  "location": "string (required)",
  "hourly_rate": "number (required, decimal > 0)",
  "notes": "string (optional)"
}
```

**Example Request:**
```json
{
  "participant_id": "123e4567-e89b-12d3-a456-426614174001",
  "staff_id": "123e4567-e89b-12d3-a456-426614174002",
  "start_time": "2023-12-15T09:00:00Z",
  "end_time": "2023-12-15T17:00:00Z",
  "service_type": "Personal Care",
  "location": "Participant's Home",
  "hourly_rate": 45.50,
  "notes": "First session with this participant"
}
```

**Success Response (201):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "participant_id": "string (UUID)",
    "staff_id": "string (UUID)",
    "start_time": "string (DateTime)",
    "end_time": "string (DateTime)",
    "actual_start_time": "null",
    "actual_end_time": "null",
    "service_type": "string",
    "location": "string",
    "status": "scheduled",
    "hourly_rate": "number (decimal)",
    "total_cost": "number (decimal, calculated)",
    "notes": "string|null",
    "completion_notes": "null",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "participant": {},
    "staff": {}
  },
  "message": "Shift created successfully"
}
```

### PUT /api/v1/shifts/:id

Update shift details.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Request Body:**
```json
{
  "start_time": "string (optional, DateTime)",
  "end_time": "string (optional, DateTime)",
  "actual_start_time": "string (optional, DateTime)",
  "actual_end_time": "string (optional, DateTime)",
  "service_type": "string (optional)",
  "location": "string (optional)",
  "hourly_rate": "number (optional, decimal > 0)",
  "notes": "string (optional)",
  "completion_notes": "string (optional)"
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    // Updated shift object
  },
  "message": "Shift updated successfully"
}
```

### PATCH /api/v1/shifts/:id/status

Update shift status with transition validation.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Request Body:**
```json
{
  "status": "string (required, values: scheduled|in_progress|completed|cancelled|no_show)",
  "completion_notes": "string (optional)",
  "actual_start_time": "string (optional, DateTime)",
  "actual_end_time": "string (optional, DateTime)"
}
```

**Valid Status Transitions:**
- `scheduled` → `in_progress`, `cancelled`, `no_show`
- `in_progress` → `completed`, `cancelled`
- `completed` → (final state)
- `cancelled` → `scheduled` (rescheduled)
- `no_show` → `scheduled` (rescheduled)

**Example Request:**
```json
{
  "status": "in_progress",
  "actual_start_time": "2023-12-15T09:05:00Z"
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    // Updated shift with new status
  },
  "message": "Shift status updated successfully"
}
```

### DELETE /api/v1/shifts/:id

Delete shift (only scheduled or cancelled shifts).

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "message": "Shift deleted successfully"
}
```

**Error Response (400):**
```json
{
  "success": false,
  "error": {
    "code": "INVALID_OPERATION",
    "message": "Only scheduled or cancelled shifts can be deleted"
  }
}
```

---

## 📄 Documents Management

### GET /api/v1/documents

List documents with filtering.

**Headers:** `Authorization: Bearer <token>` (required)

**Query Parameters:**
- `page`: number (optional, default: 1)
- `limit`: number (optional, default: 10, max: 100)
- `participant_id`: string (UUID, optional)
- `category`: string (optional, e.g., care_plan, medical_record, incident_report, assessment)
- `file_type`: string (optional, MIME type)
- `is_active`: boolean (optional)
- `search`: string (optional, searches title, description, filename)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "documents": [
      {
        "id": "string (UUID)",
        "participant_id": "string (UUID)|null",
        "uploaded_by": "string (UUID)",
        "filename": "string (system generated)",
        "original_filename": "string (user uploaded name)",
        "title": "string",
        "description": "string|null",
        "category": "string",
        "file_type": "string (MIME type)",
        "file_size": "number (bytes)",
        "file_path": "string (system path)",
        "url": "string (download URL)",
        "is_active": "boolean",
        "expiry_date": "string (DateTime)|null",
        "created_at": "string (DateTime)",
        "updated_at": "string (DateTime)",
        "participant": {
          "id": "string (UUID)",
          "first_name": "string",
          "last_name": "string"
        },
        "uploaded_by_user": {
          "id": "string (UUID)",
          "first_name": "string",
          "last_name": "string"
        }
      }
    ],
    "pagination": {
      "page": "number",
      "limit": "number",
      "total": "number",
      "total_pages": "number"
    }
  }
}
```

### GET /api/v1/documents/:id

Get document metadata.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "participant_id": "string (UUID)|null",
    "uploaded_by": "string (UUID)",
    "filename": "string",
    "original_filename": "string",
    "title": "string",
    "description": "string|null",
    "category": "string",
    "file_type": "string",
    "file_size": "number",
    "file_path": "string",
    "url": "string",
    "is_active": "boolean",
    "expiry_date": "string (DateTime)|null",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "participant": {},
    "uploaded_by_user": {}
  }
}
```

### POST /api/v1/documents

Upload a new document.

**Headers:** `Authorization: Bearer <token>` (required)

**Content-Type:** `multipart/form-data`

**Form Fields:**
- `file`: File (required, max 10MB)
- `title`: string (required)
- `description`: string (optional)
- `category`: string (required)
- `participant_id`: string (UUID, optional)
- `expiry_date`: string (optional, Date format YYYY-MM-DD)

**Example Request (using curl):**
```bash
curl -X POST \
  -H "Authorization: Bearer <token>" \
  -F "file=@document.pdf" \
  -F "title=Care Plan Document" \
  -F "description=Updated care plan for Jane Smith" \
  -F "category=care_plan" \
  -F "participant_id=123e4567-e89b-12d3-a456-426614174001" \
  -F "expiry_date=2024-12-31" \
  http://localhost:8080/api/v1/documents
```

**Success Response (201):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "participant_id": "string (UUID)|null",
    "uploaded_by": "string (UUID)",
    "filename": "string (system generated)",
    "original_filename": "string",
    "title": "string",
    "description": "string|null",
    "category": "string",
    "file_type": "string",
    "file_size": "number",
    "file_path": "string",
    "url": "string",
    "is_active": "boolean",
    "expiry_date": "string (DateTime)|null",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "participant": {},
    "uploaded_by_user": {}
  },
  "message": "Document uploaded successfully"
}
```

**Error Response (400):**
```json
{
  "success": false,
  "error": {
    "code": "FILE_TOO_LARGE",
    "message": "File size exceeds 10MB limit"
  }
}
```

### PUT /api/v1/documents/:id

Update document metadata.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Request Body:**
```json
{
  "title": "string (optional)",
  "description": "string (optional)",
  "category": "string (optional)",
  "is_active": "boolean (optional)",
  "expiry_date": "string (optional, DateTime)"
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    // Updated document object
  },
  "message": "Document updated successfully"
}
```

### GET /api/v1/documents/:id/download

Download document file.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
- **Content-Type**: Original file MIME type
- **Content-Disposition**: `attachment; filename="original-filename.ext"`
- **Content-Length**: File size in bytes
- **Body**: Binary file data

### DELETE /api/v1/documents/:id

Delete document (soft delete).

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "message": "Document deleted successfully"
}
```

---

## 🆘 Emergency Contacts

### GET /api/v1/emergency-contacts

List emergency contacts for a participant.

**Headers:** `Authorization: Bearer <token>` (required)

**Query Parameters:**
- `participant_id`: string (UUID, required)
- `page`: number (optional, default: 1)
- `limit`: number (optional, default: 10, max: 100)
- `is_primary`: boolean (optional)
- `is_active`: boolean (optional)
- `search`: string (optional, searches name, relationship, phone, email)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "emergency_contacts": [
      {
        "id": "string (UUID)",
        "participant_id": "string (UUID)",
        "name": "string",
        "relationship": "string",
        "phone": "string",
        "email": "string|null",
        "is_primary": "boolean",
        "is_active": "boolean",
        "created_at": "string (DateTime)",
        "updated_at": "string (DateTime)",
        "participant": {
          "id": "string (UUID)",
          "first_name": "string",
          "last_name": "string"
        }
      }
    ],
    "pagination": {
      "page": "number",
      "limit": "number",
      "total": "number",
      "total_pages": "number"
    }
  }
}
```

### GET /api/v1/emergency-contacts/:id

Get emergency contact details.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "participant_id": "string (UUID)",
    "name": "string",
    "relationship": "string",
    "phone": "string",
    "email": "string|null",
    "is_primary": "boolean",
    "is_active": "boolean",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "participant": {}
  }
}
```

### POST /api/v1/emergency-contacts

Create a new emergency contact.

**Headers:** `Authorization: Bearer <token>` (required)

**Request Body:**
```json
{
  "participant_id": "string (required, UUID)",
  "name": "string (required)",
  "relationship": "string (required)",
  "phone": "string (required)",
  "email": "string (optional, email format)",
  "is_primary": "boolean (optional, default: false)"
}
```

**Example Request:**
```json
{
  "participant_id": "123e4567-e89b-12d3-a456-426614174001",
  "name": "Mary Smith",
  "relationship": "Mother",
  "phone": "+61412345678",
  "email": "mary.smith@email.com",
  "is_primary": true
}
```

**Success Response (201):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "participant_id": "string (UUID)",
    "name": "string",
    "relationship": "string",
    "phone": "string",
    "email": "string|null",
    "is_primary": "boolean",
    "is_active": "boolean",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "participant": {}
  },
  "message": "Emergency contact created successfully"
}
```

### PUT /api/v1/emergency-contacts/:id

Update emergency contact.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Request Body:**
```json
{
  "name": "string (optional)",
  "relationship": "string (optional)",
  "phone": "string (optional)",
  "email": "string (optional, email format)",
  "is_primary": "boolean (optional)",
  "is_active": "boolean (optional)"
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    // Updated emergency contact object
  },
  "message": "Emergency contact updated successfully"
}
```

### DELETE /api/v1/emergency-contacts/:id

Delete emergency contact (hard delete).

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "message": "Emergency contact deleted successfully"
}
```

---

## 🏥 Care Plans

### GET /api/v1/care-plans

List care plans with filtering.

**Headers:** `Authorization: Bearer <token>` (required)

**Query Parameters:**
- `page`: number (optional, default: 1)
- `limit`: number (optional, default: 10, max: 100)
- `participant_id`: string (UUID, optional)
- `status`: string (optional, values: active|completed|cancelled)
- `created_by`: string (UUID, optional)
- `search`: string (optional, searches title, description)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "care_plans": [
      {
        "id": "string (UUID)",
        "participant_id": "string (UUID)",
        "title": "string",
        "description": "string|null",
        "goals": "string|null",
        "start_date": "string (DateTime)",
        "end_date": "string (DateTime)|null",
        "status": "string",
        "created_by": "string (UUID)",
        "approved_by": "string (UUID)|null",
        "approved_at": "string (DateTime)|null",
        "created_at": "string (DateTime)",
        "updated_at": "string (DateTime)",
        "participant": {
          "id": "string (UUID)",
          "first_name": "string",
          "last_name": "string"
        },
        "creator": {
          "id": "string (UUID)",
          "first_name": "string",
          "last_name": "string"
        },
        "approver": {
          "id": "string (UUID)",
          "first_name": "string",
          "last_name": "string"
        }
      }
    ],
    "pagination": {
      "page": "number",
      "limit": "number",
      "total": "number",
      "total_pages": "number"
    }
  }
}
```

### GET /api/v1/care-plans/:id

Get care plan details.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "participant_id": "string (UUID)",
    "title": "string",
    "description": "string|null",
    "goals": "string|null",
    "start_date": "string (DateTime)",
    "end_date": "string (DateTime)|null",
    "status": "string",
    "created_by": "string (UUID)",
    "approved_by": "string (UUID)|null",
    "approved_at": "string (DateTime)|null",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "participant": {},
    "creator": {},
    "approver": {}
  }
}
```

### POST /api/v1/care-plans

Create a new care plan.

**Headers:** `Authorization: Bearer <token>` (required)

**Request Body:**
```json
{
  "participant_id": "string (required, UUID)",
  "title": "string (required)",
  "description": "string (optional)",
  "goals": "string (optional)",
  "start_date": "string (required, DateTime)",
  "end_date": "string (optional, DateTime)"
}
```

**Example Request:**
```json
{
  "participant_id": "123e4567-e89b-12d3-a456-426614174001",
  "title": "Personal Care and Social Support Plan",
  "description": "Comprehensive plan focusing on daily living skills and community integration",
  "goals": "Improve independence in personal care activities and increase social participation",
  "start_date": "2023-12-01T00:00:00Z",
  "end_date": "2024-11-30T23:59:59Z"
}
```

**Success Response (201):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "participant_id": "string (UUID)",
    "title": "string",
    "description": "string|null",
    "goals": "string|null",
    "start_date": "string (DateTime)",
    "end_date": "string (DateTime)|null",
    "status": "active",
    "created_by": "string (UUID)",
    "approved_by": "null",
    "approved_at": "null",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "participant": {},
    "creator": {}
  },
  "message": "Care plan created successfully"
}
```

### PUT /api/v1/care-plans/:id

Update care plan.

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Request Body:**
```json
{
  "title": "string (optional)",
  "description": "string (optional)",
  "goals": "string (optional)",
  "start_date": "string (optional, DateTime)",
  "end_date": "string (optional, DateTime)",
  "status": "string (optional, values: active|completed|cancelled)"
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    // Updated care plan object
  },
  "message": "Care plan updated successfully"
}
```

### PATCH /api/v1/care-plans/:id/approve

Approve or reject care plan (admin/manager only).

**Headers:** `Authorization: Bearer <token>` (required, admin or manager role)

**Path Parameters:**
- `id`: string (UUID, required)

**Request Body:**
```json
{
  "approval_action": "string (required, values: approve|reject)"
}
```

**Example Request:**
```json
{
  "approval_action": "approve"
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "participant_id": "string (UUID)",
    "title": "string",
    "description": "string|null",
    "goals": "string|null",
    "start_date": "string (DateTime)",
    "end_date": "string (DateTime)|null",
    "status": "string",
    "created_by": "string (UUID)",
    "approved_by": "string (UUID)",
    "approved_at": "string (DateTime)",
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)",
    "participant": {},
    "creator": {},
    "approver": {}
  },
  "message": "Care plan approved successfully"
}
```

### DELETE /api/v1/care-plans/:id

Delete care plan (cannot delete completed ones).

**Headers:** `Authorization: Bearer <token>` (required)

**Path Parameters:**
- `id`: string (UUID, required)

**Success Response (200):**
```json
{
  "success": true,
  "message": "Care plan deleted successfully"
}
```

---

## 🏢 Organization Management

### GET /api/v1/organization

Get organization details.

**Headers:** `Authorization: Bearer <token>` (required)

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    "id": "string (UUID)",
    "name": "string",
    "abn": "string|null",
    "phone": "string|null",
    "email": "string|null",
    "website": "string|null",
    "address": {
      "street": "string|null",
      "suburb": "string|null",
      "state": "string|null",
      "postcode": "string|null",
      "country": "string"
    },
    "ndis_registration": {
      "registration_number": "string|null",
      "registration_status": "string",
      "expiry_date": "string (DateTime)|null"
    },
    "created_at": "string (DateTime)",
    "updated_at": "string (DateTime)"
  }
}
```

### PUT /api/v1/organization

Update organization details (admin only).

**Headers:** `Authorization: Bearer <token>` (required, admin role)

**Request Body:**
```json
{
  "name": "string (optional)",
  "abn": "string (optional, unique)",
  "phone": "string (optional)",
  "email": "string (optional, email format)",
  "website": "string (optional)",
  "address": {
    "street": "string (optional)",
    "suburb": "string (optional)",
    "state": "string (optional)",
    "postcode": "string (optional)",
    "country": "string (optional)"
  },
  "ndis_registration": {
    "registration_number": "string (optional)",
    "registration_status": "string (optional)",
    "expiry_date": "string (optional, DateTime)"
  }
}
```

**Example Request:**
```json
{
  "name": "DASYIN - ADL Services",
  "abn": "12345678901",
  "phone": "+61887654321",
  "email": "info@dasyin.com.au",
  "website": "https://dasyin.com.au",
  "address": {
    "street": "789 Business Avenue",
    "suburb": "Adelaide",
    "state": "SA",
    "postcode": "5000",
    "country": "Australia"
  },
  "ndis_registration": {
    "registration_number": "REG123456",
    "registration_status": "active",
    "expiry_date": "2025-12-31T23:59:59Z"
  }
}
```

**Success Response (200):**
```json
{
  "success": true,
  "data": {
    // Updated organization object
  },
  "message": "Organization updated successfully"
}
```

---

## Error Codes

### Common Error Codes

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `UNAUTHORIZED` | 401 | Invalid or missing authentication token |
| `FORBIDDEN` | 403 | Insufficient permissions for the operation |
| `VALIDATION_ERROR` | 400 | Request validation failed |
| `NOT_FOUND` | 404 | Requested resource not found |
| `DATABASE_ERROR` | 500 | Database operation failed |
| `INTERNAL_ERROR` | 500 | Internal server error |

### Authentication Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `INVALID_CREDENTIALS` | 401 | Email or password is incorrect |
| `INVALID_TOKEN` | 401 | JWT token is invalid or expired |
| `TOKEN_GENERATION_ERROR` | 500 | Failed to generate JWT token |

### User Management Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `USER_NOT_FOUND` | 404 | User does not exist |
| `USER_EXISTS` | 409 | User with email already exists |
| `PASSWORD_HASH_ERROR` | 500 | Failed to hash password |
| `INVALID_OPERATION` | 400 | Operation not allowed (e.g., self-deletion) |

### Participant Management Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `PARTICIPANT_NOT_FOUND` | 404 | Participant does not exist |
| `NDIS_NUMBER_EXISTS` | 409 | NDIS number already in use |
| `INVALID_PARTICIPANT` | 400 | Participant not found or inactive |

### Shift Management Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `SHIFT_NOT_FOUND` | 404 | Shift does not exist |
| `SCHEDULE_CONFLICT` | 409 | Staff member has conflicting shift |
| `INVALID_TIME_RANGE` | 400 | End time must be after start time |
| `INVALID_STAFF` | 400 | Staff member not found or inactive |
| `INVALID_TRANSITION` | 400 | Invalid shift status transition |

### Document Management Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `DOCUMENT_NOT_FOUND` | 404 | Document does not exist |
| `FILE_REQUIRED` | 400 | File upload is required |
| `FILE_TOO_LARGE` | 400 | File exceeds 10MB limit |
| `FILE_NOT_FOUND` | 404 | Physical file not found |
| `FILE_SAVE_ERROR` | 500 | Failed to save file |
| `FORM_PARSE_ERROR` | 400 | Failed to parse multipart form |
| `DIRECTORY_ERROR` | 500 | Failed to create uploads directory |

### Organization Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `ORGANIZATION_NOT_FOUND` | 404 | Organization does not exist |
| `ABN_EXISTS` | 409 | ABN already in use by another organization |

### Contact & Care Plan Errors

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `CONTACT_NOT_FOUND` | 404 | Emergency contact does not exist |
| `CARE_PLAN_NOT_FOUND` | 404 | Care plan does not exist |
| `INVALID_DATE_RANGE` | 400 | End date must be after start date |
| `INSUFFICIENT_PERMISSIONS` | 403 | User lacks required permissions |

---

## Rate Limiting

API endpoints are rate-limited to prevent abuse:

- **Authentication endpoints**: 5 requests per minute
- **File upload endpoints**: 10 requests per minute
- **All other endpoints**: 100 requests per minute

Rate limit headers are included in responses:
- `X-RateLimit-Limit`: Maximum requests allowed
- `X-RateLimit-Remaining`: Requests remaining in window
- `X-RateLimit-Reset`: Unix timestamp when limit resets

---

## Pagination

All list endpoints support pagination with the following parameters:

- `page`: Page number (starting from 1)
- `limit`: Items per page (1-100, default: 10)

Pagination information is included in the response:

```json
{
  "pagination": {
    "page": 1,
    "limit": 10,
    "total": 150,
    "total_pages": 15
  }
}
```