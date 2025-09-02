#!/bin/bash

# GoFiber CRM API Test Script
# This script tests all API endpoints using curl commands

set -e  # Exit on any error

# Configuration
BASE_URL="http://localhost:8080/api/v1"
HEALTH_URL="http://localhost:8080/health"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Global variables
ACCESS_TOKEN=""
REFRESH_TOKEN=""
USER_ID=""
PARTICIPANT_ID=""
SHIFT_ID=""
DOCUMENT_ID=""
CONTACT_ID=""
CARE_PLAN_ID=""

# Helper functions
print_header() {
    echo -e "\n${BLUE}=== $1 ===${NC}"
}

print_success() {
    echo -e "${GREEN}✅ $1${NC}"
}

print_error() {
    echo -e "${RED}❌ $1${NC}"
}

print_warning() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

print_info() {
    echo -e "${BLUE}ℹ️  $1${NC}"
}

# Test function that displays results
test_endpoint() {
    local method=$1
    local url=$2
    local data=$3
    local description=$4
    local auth_header=""
    
    if [ ! -z "$ACCESS_TOKEN" ]; then
        auth_header="Authorization: Bearer $ACCESS_TOKEN"
    fi
    
    print_info "Testing: $description"
    echo "Request: $method $url"
    
    if [ "$method" = "GET" ] || [ "$method" = "DELETE" ]; then
        response=$(curl -s -w "\n%{http_code}" -X "$method" \
            -H "Content-Type: application/json" \
            -H "$auth_header" \
            "$url")
    else
        response=$(curl -s -w "\n%{http_code}" -X "$method" \
            -H "Content-Type: application/json" \
            -H "$auth_header" \
            -d "$data" \
            "$url")
    fi
    
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | head -n -1)
    
    if [ "$http_code" -ge 200 ] && [ "$http_code" -lt 300 ]; then
        print_success "Response: $http_code"
        echo "$body" | jq . 2>/dev/null || echo "$body"
    else
        print_error "Response: $http_code"
        echo "$body" | jq . 2>/dev/null || echo "$body"
        return 1
    fi
}

# Function that returns just the response body for parsing
call_api() {
    local method=$1
    local url=$2
    local data=$3
    local auth_header=""
    
    if [ ! -z "$ACCESS_TOKEN" ]; then
        auth_header="Authorization: Bearer $ACCESS_TOKEN"
    fi
    
    if [ "$method" = "GET" ] || [ "$method" = "DELETE" ]; then
        response=$(curl -s -w "\n%{http_code}" -X "$method" \
            -H "Content-Type: application/json" \
            -H "$auth_header" \
            "$url")
    else
        response=$(curl -s -w "\n%{http_code}" -X "$method" \
            -H "Content-Type: application/json" \
            -H "$auth_header" \
            -d "$data" \
            "$url")
    fi
    
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | head -n -1)
    
    if [ "$http_code" -ge 200 ] && [ "$http_code" -lt 300 ]; then
        echo "$body"
        return 0
    else
        return 1
    fi
}

# Health Check
test_health() {
    print_header "Health Check"
    test_endpoint "GET" "$HEALTH_URL" "" "Health check endpoint"
}

# Authentication Tests
test_authentication() {
    print_header "Authentication Tests"
    
    # Login
    login_data='{
        "email": "kennedy@dasyin.com.au",
        "password": "password"
    }'
    
    # Display the login test
    test_endpoint "POST" "$BASE_URL/auth/login" "$login_data" "User login"
    
    # Get the response for parsing
    response=$(call_api "POST" "$BASE_URL/auth/login" "$login_data")
    if [ $? -eq 0 ]; then
        ACCESS_TOKEN=$(echo "$response" | jq -r '.data.token' 2>/dev/null)
        REFRESH_TOKEN=$(echo "$response" | jq -r '.data.refresh_token' 2>/dev/null)
        USER_ID=$(echo "$response" | jq -r '.data.user.id' 2>/dev/null)
        
        if [ "$ACCESS_TOKEN" != "null" ] && [ ! -z "$ACCESS_TOKEN" ]; then
            print_success "Login successful - Token acquired"
            echo "Token: ${ACCESS_TOKEN:0:20}..."
        else
            print_error "Login response parsing failed"
            exit 1
        fi
    else
        print_error "Login failed - Cannot proceed with other tests"
        exit 1
    fi
    
    # Test refresh token
    refresh_data="{\"refresh_token\": \"$REFRESH_TOKEN\"}"
    test_endpoint "POST" "$BASE_URL/auth/refresh" "$refresh_data" "Refresh token"
}

# Users Management Tests
test_users() {
    print_header "Users Management Tests"
    
    # Get current user
    test_endpoint "GET" "$BASE_URL/users/me" "" "Get current user"
    
    # Get all users
    test_endpoint "GET" "$BASE_URL/users?page=1&limit=5" "" "Get all users with pagination"
    
    # Create user (admin only)
    create_user_data='{
        "email": "test.user@dasyin.com.au",
        "password": "testpassword123",
        "first_name": "Test",
        "last_name": "User",
        "phone": "+61412345678",
        "role": "care_worker"
    }'
    
    # Display the create user test
    test_endpoint "POST" "$BASE_URL/users" "$create_user_data" "Create new user"
    
    # Get the response for parsing
    response=$(call_api "POST" "$BASE_URL/users" "$create_user_data")
    if [ $? -eq 0 ]; then
        CREATED_USER_ID=$(echo "$response" | jq -r '.data.id' 2>/dev/null)
        if [ "$CREATED_USER_ID" != "null" ] && [ ! -z "$CREATED_USER_ID" ]; then
            print_success "User created with ID: $CREATED_USER_ID"
        else
            print_warning "User creation response parsing failed"
        fi
        
        # Update user
        update_user_data='{
            "first_name": "Updated",
            "phone": "+61487654321"
        }'
        test_endpoint "PUT" "$BASE_URL/users/$CREATED_USER_ID" "$update_user_data" "Update user"
        
        # Delete user (will do this later to avoid breaking other tests)
    fi
}

# Participants Management Tests
test_participants() {
    print_header "Participants Management Tests"
    
    # Get all participants
    test_endpoint "GET" "$BASE_URL/participants?page=1&limit=5" "" "Get all participants"
    
    # Create participant
    create_participant_data='{
        "first_name": "Test",
        "last_name": "Participant",
        "date_of_birth": "1990-05-15",
        "ndis_number": "9876543210",
        "email": "test.participant@email.com",
        "phone": "+61456789123",
        "address": {
            "street": "123 Test Street",
            "suburb": "Adelaide",
            "state": "SA",
            "postcode": "5000",
            "country": "Australia"
        },
        "medical_information": {
            "conditions": "[\"Test Condition\"]",
            "medications": "[\"Test Medication\"]",
            "doctor_name": "Dr. Test",
            "doctor_phone": "+61887654321"
        },
        "funding": {
            "total_budget": 30000.00,
            "used_budget": 5000.00,
            "remaining_budget": 25000.00,
            "budget_year": "2023-2024"
        },
        "emergency_contacts": [
            {
                "name": "Test Contact",
                "relationship": "Parent",
                "phone": "+61423456789",
                "email": "test.contact@email.com",
                "is_primary": true
            }
        ]
    }'
    
    # Display the create participant test
    test_endpoint "POST" "$BASE_URL/participants" "$create_participant_data" "Create participant"
    
    # Get the response for parsing
    response=$(call_api "POST" "$BASE_URL/participants" "$create_participant_data")
    if [ $? -eq 0 ]; then
        PARTICIPANT_ID=$(echo "$response" | jq -r '.data.id' 2>/dev/null)
        if [ "$PARTICIPANT_ID" != "null" ] && [ ! -z "$PARTICIPANT_ID" ]; then
            print_success "Participant created with ID: $PARTICIPANT_ID"
        else
            print_warning "Participant creation response parsing failed"
        fi
        
        # Get participant by ID
        test_endpoint "GET" "$BASE_URL/participants/$PARTICIPANT_ID" "" "Get participant by ID"
        
        # Update participant
        update_participant_data='{
            "phone": "+61456789999",
            "address": {
                "street": "456 Updated Street",
                "suburb": "Adelaide",
                "state": "SA",
                "postcode": "5001",
                "country": "Australia"
            }
        }'
        test_endpoint "PUT" "$BASE_URL/participants/$PARTICIPANT_ID" "$update_participant_data" "Update participant"
    fi
}

# Shifts Management Tests
test_shifts() {
    print_header "Shifts Management Tests"
    
    if [ -z "$PARTICIPANT_ID" ] || [ -z "$USER_ID" ]; then
        print_warning "Skipping shifts tests - missing participant or user ID"
        return
    fi
    
    # Get all shifts
    test_endpoint "GET" "$BASE_URL/shifts?page=1&limit=5" "" "Get all shifts"
    
    # Create shift
    create_shift_data="{
        \"participant_id\": \"$PARTICIPANT_ID\",
        \"staff_id\": \"$USER_ID\",
        \"start_time\": \"2023-12-15T09:00:00Z\",
        \"end_time\": \"2023-12-15T17:00:00Z\",
        \"service_type\": \"Personal Care\",
        \"location\": \"Participant's Home\",
        \"hourly_rate\": 45.50,
        \"notes\": \"Test shift\"
    }"
    
    # Display the create shift test
    test_endpoint "POST" "$BASE_URL/shifts" "$create_shift_data" "Create shift"
    
    # Get the response for parsing
    response=$(call_api "POST" "$BASE_URL/shifts" "$create_shift_data")
    if [ $? -eq 0 ]; then
        SHIFT_ID=$(echo "$response" | jq -r '.data.id' 2>/dev/null)
        if [ "$SHIFT_ID" != "null" ] && [ ! -z "$SHIFT_ID" ]; then
            print_success "Shift created with ID: $SHIFT_ID"
        else
            print_warning "Shift creation response parsing failed"
        fi
        
        # Get shift by ID
        test_endpoint "GET" "$BASE_URL/shifts/$SHIFT_ID" "" "Get shift by ID"
        
        # Update shift
        update_shift_data='{
            "hourly_rate": 50.00,
            "notes": "Updated test shift"
        }'
        test_endpoint "PUT" "$BASE_URL/shifts/$SHIFT_ID" "$update_shift_data" "Update shift"
        
        # Update shift status
        status_update_data='{
            "status": "in_progress",
            "actual_start_time": "2023-12-15T09:05:00Z"
        }'
        test_endpoint "PATCH" "$BASE_URL/shifts/$SHIFT_ID/status" "$status_update_data" "Update shift status"
    fi
}

# Emergency Contacts Tests
test_emergency_contacts() {
    print_header "Emergency Contacts Tests"
    
    if [ -z "$PARTICIPANT_ID" ]; then
        print_warning "Skipping emergency contacts tests - missing participant ID"
        return
    fi
    
    # Get emergency contacts
    test_endpoint "GET" "$BASE_URL/emergency-contacts?participant_id=$PARTICIPANT_ID" "" "Get emergency contacts"
    
    # Create emergency contact
    create_contact_data="{
        \"participant_id\": \"$PARTICIPANT_ID\",
        \"name\": \"Test Emergency Contact\",
        \"relationship\": \"Friend\",
        \"phone\": \"+61412999888\",
        \"email\": \"emergency@test.com\",
        \"is_primary\": false
    }"
    
    # Display the create emergency contact test
    test_endpoint "POST" "$BASE_URL/emergency-contacts" "$create_contact_data" "Create emergency contact"
    
    # Get the response for parsing
    response=$(call_api "POST" "$BASE_URL/emergency-contacts" "$create_contact_data")
    if [ $? -eq 0 ]; then
        CONTACT_ID=$(echo "$response" | jq -r '.data.id' 2>/dev/null)
        if [ "$CONTACT_ID" != "null" ] && [ ! -z "$CONTACT_ID" ]; then
            print_success "Emergency contact created with ID: $CONTACT_ID"
        else
            print_warning "Emergency contact creation response parsing failed"
        fi
        
        # Get contact by ID
        test_endpoint "GET" "$BASE_URL/emergency-contacts/$CONTACT_ID" "" "Get emergency contact by ID"
        
        # Update contact
        update_contact_data='{
            "phone": "+61412888999",
            "email": "updated.emergency@test.com"
        }'
        test_endpoint "PUT" "$BASE_URL/emergency-contacts/$CONTACT_ID" "$update_contact_data" "Update emergency contact"
    fi
}

# Care Plans Tests
test_care_plans() {
    print_header "Care Plans Tests"
    
    if [ -z "$PARTICIPANT_ID" ]; then
        print_warning "Skipping care plans tests - missing participant ID"
        return
    fi
    
    # Get all care plans
    test_endpoint "GET" "$BASE_URL/care-plans?page=1&limit=5" "" "Get all care plans"
    
    # Create care plan
    create_care_plan_data="{
        \"participant_id\": \"$PARTICIPANT_ID\",
        \"title\": \"Test Care Plan\",
        \"description\": \"A test care plan for API testing\",
        \"goals\": \"Test goals for the participant\",
        \"start_date\": \"2023-12-01T00:00:00Z\",
        \"end_date\": \"2024-11-30T23:59:59Z\"
    }"
    
    # Display the create care plan test
    test_endpoint "POST" "$BASE_URL/care-plans" "$create_care_plan_data" "Create care plan"
    
    # Get the response for parsing
    response=$(call_api "POST" "$BASE_URL/care-plans" "$create_care_plan_data")
    if [ $? -eq 0 ]; then
        CARE_PLAN_ID=$(echo "$response" | jq -r '.data.id' 2>/dev/null)
        if [ "$CARE_PLAN_ID" != "null" ] && [ ! -z "$CARE_PLAN_ID" ]; then
            print_success "Care plan created with ID: $CARE_PLAN_ID"
        else
            print_warning "Care plan creation response parsing failed"
        fi
        
        # Get care plan by ID
        test_endpoint "GET" "$BASE_URL/care-plans/$CARE_PLAN_ID" "" "Get care plan by ID"
        
        # Update care plan
        update_care_plan_data='{
            "description": "Updated test care plan description",
            "goals": "Updated goals"
        }'
        test_endpoint "PUT" "$BASE_URL/care-plans/$CARE_PLAN_ID" "$update_care_plan_data" "Update care plan"
        
        # Approve care plan (admin/manager only)
        approve_data='{
            "approval_action": "approve"
        }'
        test_endpoint "PATCH" "$BASE_URL/care-plans/$CARE_PLAN_ID/approve" "$approve_data" "Approve care plan"
    fi
}

# Documents Tests (limited without actual file)
test_documents() {
    print_header "Documents Tests"
    
    # Get all documents
    test_endpoint "GET" "$BASE_URL/documents?page=1&limit=5" "" "Get all documents"
    
    print_info "File upload test requires actual file - skipping upload test"
    print_info "To test file upload manually, use:"
    print_info "curl -X POST -H \"Authorization: Bearer \$ACCESS_TOKEN\" -F \"file=@test.pdf\" -F \"title=Test Document\" -F \"category=test\" $BASE_URL/documents"
}

# Organization Tests
test_organization() {
    print_header "Organization Tests"
    
    # Get organization
    test_endpoint "GET" "$BASE_URL/organization" "" "Get organization details"
    
    # Update organization (admin only)
    update_org_data='{
        "name": "DASYIN - ADL Services (Test Updated)",
        "phone": "+61887654999",
        "email": "test.info@dasyin.com.au"
    }'
    test_endpoint "PUT" "$BASE_URL/organization" "$update_org_data" "Update organization"
}

# Cleanup function
cleanup_test_data() {
    print_header "Cleanup Test Data"
    
    # Delete created resources (in reverse order of dependencies)
    [ ! -z "$CARE_PLAN_ID" ] && test_endpoint "DELETE" "$BASE_URL/care-plans/$CARE_PLAN_ID" "" "Delete test care plan"
    [ ! -z "$CONTACT_ID" ] && test_endpoint "DELETE" "$BASE_URL/emergency-contacts/$CONTACT_ID" "" "Delete test emergency contact"
    [ ! -z "$SHIFT_ID" ] && test_endpoint "DELETE" "$BASE_URL/shifts/$SHIFT_ID" "" "Delete test shift"
    [ ! -z "$DOCUMENT_ID" ] && test_endpoint "DELETE" "$BASE_URL/documents/$DOCUMENT_ID" "" "Delete test document"
    [ ! -z "$PARTICIPANT_ID" ] && test_endpoint "DELETE" "$BASE_URL/participants/$PARTICIPANT_ID" "" "Delete test participant"
    [ ! -z "$CREATED_USER_ID" ] && test_endpoint "DELETE" "$BASE_URL/users/$CREATED_USER_ID" "" "Delete test user"
    
    # Logout
    test_endpoint "POST" "$BASE_URL/auth/logout" "" "Logout user"
}

# Main execution
main() {
    print_header "GoFiber CRM API Test Suite"
    print_info "Starting comprehensive API testing..."
    
    # Check if jq is available
    if ! command -v jq &> /dev/null; then
        print_warning "jq is not installed - JSON responses will not be formatted"
    fi
    
    # Run tests in order
    test_health
    test_authentication
    test_users
    test_participants
    test_shifts
    test_emergency_contacts
    test_care_plans
    test_documents
    test_organization
    
    # Cleanup
    cleanup_test_data
    
    print_header "Test Suite Completed"
    print_success "All API tests completed successfully!"
    print_info "Check the output above for any errors or warnings"
}

# Run main function
main "$@"