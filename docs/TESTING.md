# Testing Guide

This document provides comprehensive testing instructions for the GoFiber CRM API, including automated tests, Postman collections, and manual testing procedures.

## 🚀 Quick Start

### Prerequisites

- Go 1.21+
- Running GoFiber CRM API server (`make run` or `make dev`)
- curl (for bash scripts)
- jq (optional, for JSON formatting)
- Postman (for GUI testing)

### Default Test User

The system comes with a default admin user for testing:
- **Email**: `kennedy@dasyin.com.au`
- **Password**: `password`
- **Role**: `admin`

## 🧪 Testing Methods

### 1. Automated Bash Script

Run the comprehensive test suite that covers all API endpoints:

```bash
# Make script executable (if not already)
chmod +x scripts/test_api.sh

# Run all tests
./scripts/test_api.sh
```

**Features:**
- ✅ Tests all CRUD operations for every endpoint
- ✅ Handles authentication automatically
- ✅ Creates and cleans up test data
- ✅ Validates HTTP response codes
- ✅ Formats JSON responses (if jq is installed)
- ✅ Color-coded output for easy reading

### 2. Postman Collection

Import the pre-configured Postman collection and environment:

#### Import Steps:
1. Open Postman
2. Click **Import** → **Upload Files**
3. Import both files:
   - `postman/GoFiber-CRM-API.postman_collection.json`
   - `postman/Environment.postman_environment.json`
4. Select the **GoFiber CRM - Development** environment
5. Run the **Login** request first to get authentication token
6. All subsequent requests will use the token automatically

#### Collection Features:
- 📁 **Organized by modules** (Auth, Users, Participants, etc.)
- 🔐 **Automatic token management** (login saves tokens to variables)
- 📋 **Pre-filled request bodies** with sample data
- 🔄 **Dynamic variables** (IDs are saved and reused)
- ✅ **Test scripts** that validate responses

### 3. Manual curl Commands

Test individual endpoints using curl:

#### Authentication
```bash
# Login
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "email": "kennedy@dasyin.com.au",
    "password": "password"
  }'

# Save the token from response
export TOKEN="your_jwt_token_here"
```

#### Users Management
```bash
# Get current user
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/users/me

# Get all users with pagination
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/users?page=1&limit=10&role=care_worker"

# Create new user
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "test@example.com",
    "password": "testpass123",
    "first_name": "Test",
    "last_name": "User",
    "role": "care_worker"
  }' \
  http://localhost:8080/api/v1/users
```

#### Participants Management
```bash
# Create participant with full data
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "first_name": "Jane",
    "last_name": "Doe",
    "date_of_birth": "1990-05-15",
    "ndis_number": "1234567890",
    "email": "jane.doe@email.com",
    "phone": "+61456789123",
    "address": {
      "street": "123 Main St",
      "suburb": "Adelaide",
      "state": "SA",
      "postcode": "5000"
    },
    "medical_information": {
      "conditions": "[\"Autism\"]",
      "doctor_name": "Dr. Smith"
    },
    "funding": {
      "total_budget": 50000.00,
      "budget_year": "2023-2024"
    }
  }' \
  http://localhost:8080/api/v1/participants
```

#### File Upload (Documents)
```bash
# Upload document
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -F "file=@sample.pdf" \
  -F "title=Care Plan Document" \
  -F "description=Sample care plan" \
  -F "category=care_plan" \
  -F "participant_id=PARTICIPANT_ID_HERE" \
  http://localhost:8080/api/v1/documents
```

## 🧩 Test Scenarios

### Complete CRUD Test Flow

#### 1. Authentication Flow
```bash
# Login → Get Token → Use Token → Refresh Token → Logout
```

#### 2. User Management Flow
```bash
# Login as admin → Create user → Update user → List users → Delete user
```

#### 3. Participant Management Flow
```bash
# Create participant → Add emergency contacts → Upload documents → Create care plan → Assign shifts
```

#### 4. Shift Management Flow
```bash
# Create shift → Update details → Change status (scheduled → in_progress → completed) → Generate reports
```

### Error Testing Scenarios

#### Authentication Errors
```bash
# Test invalid credentials
curl -X POST http://localhost:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email": "invalid@test.com", "password": "wrong"}'

# Test expired token
curl -H "Authorization: Bearer invalid_token" \
  http://localhost:8080/api/v1/users/me
```

#### Validation Errors
```bash
# Test missing required fields
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"email": "invalid-email"}' \
  http://localhost:8080/api/v1/users

# Test invalid data types
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"date_of_birth": "invalid-date"}' \
  http://localhost:8080/api/v1/participants
```

#### Permission Errors
```bash
# Test admin-only endpoints with non-admin user
curl -X POST \
  -H "Authorization: Bearer $NON_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"email": "test@test.com"}' \
  http://localhost:8080/api/v1/users
```

## 📊 Response Validation

### Expected Response Format
All responses follow this structure:
```json
{
  "success": true|false,
  "data": {},
  "message": "string",
  "error": {
    "code": "ERROR_CODE",
    "message": "Error description",
    "details": "Additional details"
  }
}
```

### HTTP Status Codes

| Code | Meaning | When to Expect |
|------|---------|----------------|
| 200 | OK | Successful GET, PUT, PATCH, DELETE |
| 201 | Created | Successful POST (resource created) |
| 400 | Bad Request | Validation errors, invalid data |
| 401 | Unauthorized | Missing/invalid auth token |
| 403 | Forbidden | Insufficient permissions |
| 404 | Not Found | Resource doesn't exist |
| 409 | Conflict | Duplicate data (email, NDIS number) |
| 500 | Server Error | Database/server issues |

### Data Validation Checklist

#### User Creation
- ✅ Email format validation
- ✅ Password minimum length (8 characters)
- ✅ Role validation (admin, manager, care_worker, support_coordinator)
- ✅ Unique email constraint

#### Participant Creation
- ✅ Date of birth format (YYYY-MM-DD)
- ✅ NDIS number uniqueness
- ✅ Email format (if provided)
- ✅ Phone format validation
- ✅ Address structure validation

#### Shift Creation
- ✅ DateTime format validation
- ✅ End time after start time
- ✅ Staff schedule conflict detection
- ✅ Positive hourly rate
- ✅ Valid participant and staff IDs

## 🎯 Test Data Templates

### Sample User Data
```json
{
  "email": "test.user@dasyin.com.au",
  "password": "securepassword123",
  "first_name": "Test",
  "last_name": "User",
  "phone": "+61412345678",
  "role": "care_worker"
}
```

### Sample Participant Data
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
    "doctor_phone": "+61887654321"
  },
  "funding": {
    "total_budget": 50000.00,
    "used_budget": 12500.00,
    "remaining_budget": 37500.00,
    "budget_year": "2023-2024"
  }
}
```

### Sample Shift Data
```json
{
  "participant_id": "PARTICIPANT_UUID",
  "staff_id": "USER_UUID",
  "start_time": "2023-12-15T09:00:00Z",
  "end_time": "2023-12-15T17:00:00Z",
  "service_type": "Personal Care",
  "location": "Participant's Home",
  "hourly_rate": 45.50,
  "notes": "Regular shift"
}
```

## 🔍 Debugging Tips

### Common Issues and Solutions

#### 1. **Authentication Failed**
```bash
# Check token validity
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/users/me

# If expired, refresh token
curl -X POST \
  -H "Content-Type: application/json" \
  -d '{"refresh_token": "$REFRESH_TOKEN"}' \
  http://localhost:8080/api/v1/auth/refresh
```

#### 2. **Validation Errors**
- Check required fields in API documentation
- Verify data types (string, number, boolean, date)
- Ensure proper JSON formatting

#### 3. **Permission Denied**
- Verify user role has required permissions
- Check endpoint-specific role requirements
- Ensure using admin account for admin-only operations

#### 4. **Resource Not Found**
- Verify UUIDs are correct and exist
- Check organization data isolation (users can only access their org's data)
- Ensure resource hasn't been soft-deleted

### Logging and Monitoring

Check server logs for detailed error information:
```bash
# If running with make dev
tail -f logs/app.log

# If running with docker
docker logs container_name
```

## 🏃‍♂️ Performance Testing

### Basic Load Testing with curl

```bash
# Test concurrent requests
for i in {1..10}; do
  curl -H "Authorization: Bearer $TOKEN" \
    "http://localhost:8080/api/v1/users" &
done
wait
```

### Advanced Load Testing

Consider using tools like:
- **Apache Bench (ab)**
- **wrk**
- **Artillery**
- **k6**

Example with ab:
```bash
ab -n 1000 -c 10 -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/users
```

## 📋 Test Checklist

### Before Testing
- [ ] Server is running on correct port (8080)
- [ ] Database is connected and migrated
- [ ] Default admin user exists
- [ ] Environment variables are set

### Authentication Tests
- [ ] Login with valid credentials
- [ ] Login with invalid credentials (should fail)
- [ ] Token refresh works
- [ ] Expired token handling
- [ ] Logout functionality

### CRUD Operations (for each entity)
- [ ] Create with valid data
- [ ] Create with invalid data (should fail)
- [ ] Read single resource
- [ ] Read with pagination and filters
- [ ] Update with valid data
- [ ] Update with invalid data (should fail)
- [ ] Delete resource
- [ ] Delete non-existent resource (should fail)

### Business Logic
- [ ] Role-based permissions work correctly
- [ ] Organization data isolation
- [ ] Unique constraints (email, NDIS numbers)
- [ ] Date validations
- [ ] Shift conflict detection
- [ ] File upload size limits

### Error Handling
- [ ] Proper HTTP status codes
- [ ] Consistent error response format
- [ ] Meaningful error messages
- [ ] Input validation messages

---

## 🎉 Happy Testing!

This comprehensive testing guide should help you verify that all API endpoints are working correctly. For any issues or questions, refer to the [API Documentation](API.md) or check the server logs for detailed error information.