package main

import (
    "fmt"
    "log"
    "net/http"
    "encoding/json"
    "io"
    "time"
    "strconv"
)

// In-memory storage for bookings
var bookings []map[string]interface{}
var nextBookingID = 3

func init() {
    // Initialize with mock data
    bookings = []map[string]interface{}{
        {
            "id": "1",
            "start_time": "2024-09-02T10:00:00Z",
            "end_time": "2024-09-02T11:30:00Z",
            "status": "confirmed",
            "total_price": 85.00,
            "customer": map[string]string{"first_name": "John", "last_name": "Smith", "phone": "+61412345678"},
            "services": []map[string]interface{}{{"id": "1", "name": "Oil Change"}},
            "vehicle": map[string]string{"make": "Toyota", "model": "Camry", "license_plate": "ABC123"},
            "staff": map[string]string{"first_name": "Mike", "last_name": "Johnson"},
        },
        {
            "id": "2",
            "start_time": "2024-09-02T14:00:00Z",
            "end_time": "2024-09-02T15:30:00Z",
            "status": "scheduled",
            "total_price": 180.00,
            "customer": map[string]string{"first_name": "Sarah", "last_name": "Jones", "phone": "+61423456789"},
            "services": []map[string]interface{}{{"id": "2", "name": "Brake Service"}},
            "vehicle": map[string]string{"make": "Honda", "model": "Civic", "license_plate": "DEF456"},
            "staff": map[string]string{"first_name": "Tom", "last_name": "Wilson"},
        },
    }
}

func main() {
    // CORS middleware
    corsHandler := func(next http.HandlerFunc) http.HandlerFunc {
        return func(w http.ResponseWriter, r *http.Request) {
            w.Header().Set("Access-Control-Allow-Origin", "*")
            w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
            w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
            
            if r.Method == "OPTIONS" {
                w.WriteHeader(http.StatusOK)
                return
            }
            
            next(w, r)
        }
    }

    // Health check endpoint
    http.HandleFunc("/api/health", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        response := map[string]string{
            "status": "healthy",
            "message": "Backend is running",
        }
        json.NewEncoder(w).Encode(response)
    }))

    // Mock login endpoint
    http.HandleFunc("/api/auth/login", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        // Mock successful login
        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "token": "mock-jwt-token-12345",
                "user": map[string]interface{}{
                    "id": "1",
                    "email": "superadmin@dasyinbook.com",
                    "first_name": "Super",
                    "last_name": "Admin",
                    "role": "super_admin",
                },
            },
        }
        json.NewEncoder(w).Encode(response)
    }))

    // Mock organizations endpoint
    http.HandleFunc("/api/organizations", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "organizations": []map[string]interface{}{
                    {
                        "id": "1",
                        "name": "AutoCare Plus",
                        "email": "admin@autocare.com",
                        "status": "active",
                    },
                    {
                        "id": "2",
                        "name": "Beauty Haven Spa",
                        "email": "contact@beautyhaven.com",
                        "status": "active",
                    },
                },
            },
        }
        json.NewEncoder(w).Encode(response)
    }))

    // Mock bookings endpoint
    http.HandleFunc("/api/bookings", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        if r.Method == "GET" {
            response := map[string]interface{}{
                "success": true,
                "data": map[string]interface{}{
                    "bookings": bookings,
                    "pagination": map[string]interface{}{
                        "page": 1,
                        "pageSize": 100,
                        "totalCount": len(bookings),
                        "totalPages": 1,
                    },
                },
            }
            json.NewEncoder(w).Encode(response)
        } else if r.Method == "POST" {
            // Read request body
            body, err := io.ReadAll(r.Body)
            if err != nil {
                http.Error(w, "Failed to read request body", http.StatusBadRequest)
                return
            }

            // Parse the booking request
            var bookingRequest map[string]interface{}
            if err := json.Unmarshal(body, &bookingRequest); err != nil {
                http.Error(w, "Invalid JSON", http.StatusBadRequest)
                return
            }

            // Create new booking with incremented ID
            newBookingID := strconv.Itoa(nextBookingID)
            nextBookingID++

            // Extract customer info (either existing or new)
            var customerInfo map[string]string
            if newCustomer, exists := bookingRequest["new_customer"]; exists && newCustomer != nil {
                newCustomerMap := newCustomer.(map[string]interface{})
                customerInfo = map[string]string{
                    "first_name": newCustomerMap["first_name"].(string),
                    "last_name":  newCustomerMap["last_name"].(string),
                    "phone":      newCustomerMap["phone"].(string),
                }
            } else {
                // Use existing customer data (mock for now)
                customerInfo = map[string]string{
                    "first_name": "New",
                    "last_name":  "Customer", 
                    "phone":      "+61400000000",
                }
            }

            // Create end time by adding duration to start time
            startTime := bookingRequest["start_time"].(string)
            duration := int(bookingRequest["duration"].(float64))
            startTimeObj, _ := time.Parse("2006-01-02T15:04:05", startTime)
            endTimeObj := startTimeObj.Add(time.Duration(duration) * time.Minute)
            endTime := endTimeObj.Format("2006-01-02T15:04:05") + "Z"

            // Create the new booking
            newBooking := map[string]interface{}{
                "id":          newBookingID,
                "start_time":  startTime + "Z",
                "end_time":    endTime,
                "status":      bookingRequest["status"],
                "total_price": bookingRequest["total_price"],
                "customer":    customerInfo,
                "services":    []map[string]interface{}{{"id": bookingRequest["service_id"], "name": "New Service"}},
                "vehicle":     map[string]string{"make": "Unknown", "model": "Unknown", "license_plate": "NEW123"},
                "staff":       map[string]string{"first_name": "Staff", "last_name": "Member"},
                "notes":       bookingRequest["notes"],
            }

            // Add to bookings list
            bookings = append(bookings, newBooking)

            // Return success response
            response := map[string]interface{}{
                "success": true,
                "data": map[string]interface{}{
                    "booking": map[string]interface{}{
                        "id":         newBookingID,
                        "start_time": startTime,
                        "status":     bookingRequest["status"],
                        "total_price": bookingRequest["total_price"],
                        "created_at": time.Now().Format("2006-01-02T15:04:05Z"),
                    },
                },
                "message": "Booking created successfully",
            }
            json.NewEncoder(w).Encode(response)
        }
    }))

    // Mock customers endpoint
    http.HandleFunc("/api/customers", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "customers": []map[string]interface{}{
                    {"id": "1", "first_name": "John", "last_name": "Smith", "phone": "+61412345678"},
                    {"id": "2", "first_name": "Sarah", "last_name": "Jones", "phone": "+61423456789"},
                    {"id": "3", "first_name": "Mike", "last_name": "Brown", "phone": "+61434567890"},
                },
            },
        }
        json.NewEncoder(w).Encode(response)
    }))

    // Mock services endpoint
    http.HandleFunc("/api/services", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "services": []map[string]interface{}{
                    {"id": "1", "name": "Oil Change", "duration": 60, "price": 85.00},
                    {"id": "2", "name": "Brake Service", "duration": 90, "price": 180.00},
                    {"id": "3", "name": "Tire Rotation", "duration": 45, "price": 120.00},
                },
            },
        }
        json.NewEncoder(w).Encode(response)
    }))

    // Mock staff endpoint
    http.HandleFunc("/api/staff", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "staff": []map[string]interface{}{
                    {"id": "1", "first_name": "Mike", "last_name": "Johnson"},
                    {"id": "2", "first_name": "Tom", "last_name": "Wilson"},
                    {"id": "3", "first_name": "Lisa", "last_name": "Davis"},
                },
            },
        }
        json.NewEncoder(w).Encode(response)
    }))

    fmt.Println("🚀 Test server starting on http://localhost:8080")
    fmt.Println("📍 Endpoints:")
    fmt.Println("   - GET  /api/health")
    fmt.Println("   - POST /api/auth/login")
    fmt.Println("   - GET  /api/organizations")
    fmt.Println("   - GET  /api/bookings")
    fmt.Println("   - POST /api/bookings")
    fmt.Println("   - GET  /api/customers")
    fmt.Println("   - GET  /api/services")
    fmt.Println("   - GET  /api/staff")
    fmt.Println("\n✅ Server is ready to accept connections")
    
    log.Fatal(http.ListenAndServe(":8080", nil))
}