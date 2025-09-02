package main

import (
    "fmt"
    "log"
    "net/http"
    "encoding/json"
)

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
        
        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "bookings": []map[string]interface{}{
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
                },
            },
        }
        json.NewEncoder(w).Encode(response)
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
    fmt.Println("   - GET  /api/customers")
    fmt.Println("   - GET  /api/services")
    fmt.Println("   - GET  /api/staff")
    fmt.Println("\n✅ Server is ready to accept connections")
    
    log.Fatal(http.ListenAndServe(":8080", nil))
}