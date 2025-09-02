package main

import (
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "os"
    "strconv"
    "strings"
    "time"

    "github.com/joho/godotenv"
    "gorm.io/driver/postgres"
    "gorm.io/gorm"
)

type Booking struct {
    ID         uint      `json:"id" gorm:"primaryKey"`
    StartTime  time.Time `json:"start_time"`
    EndTime    time.Time `json:"end_time"`
    Status     string    `json:"status"`
    TotalPrice float64   `json:"total_price"`
    CustomerID uint      `json:"customer_id"`
    Customer   Customer  `json:"customer" gorm:"foreignKey:CustomerID"`
    CreatedAt  time.Time `json:"created_at"`
    UpdatedAt  time.Time `json:"updated_at"`
}

type Customer struct {
    ID        uint      `json:"id" gorm:"primaryKey"`
    FirstName string    `json:"first_name"`
    LastName  string    `json:"last_name"`
    Phone     string    `json:"phone"`
    Email     string    `json:"email"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

type Organization struct {
    ID     uint   `json:"id" gorm:"primaryKey"`
    Name   string `json:"name"`
    Email  string `json:"email"`
    Status string `json:"status"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

type Service struct {
    ID       uint    `json:"id" gorm:"primaryKey"`
    Name     string  `json:"name"`
    Duration int     `json:"duration"`
    Price    float64 `json:"price"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

type Staff struct {
    ID        uint   `json:"id" gorm:"primaryKey"`
    FirstName string `json:"first_name"`
    LastName  string `json:"last_name"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

type User struct {
    ID        uint      `json:"id" gorm:"primaryKey"`
    Email     string    `json:"email" gorm:"unique"`
    FirstName string    `json:"first_name"`
    LastName  string    `json:"last_name"`
    Role      string    `json:"role"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

var db *gorm.DB

func main() {
    // Load environment variables
    if err := godotenv.Load(); err != nil {
        log.Println("No .env file found")
    }

    // Connect to PostgreSQL database
    var err error
    dsn := os.Getenv("DATABASE_URL")
    if dsn == "" {
        dsn = fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=%s",
            os.Getenv("DB_HOST"),
            os.Getenv("DB_USER"), 
            os.Getenv("DB_PASSWORD"),
            os.Getenv("DB_NAME"),
            os.Getenv("DB_PORT"),
            os.Getenv("DB_SSLMODE"))
    }

    fmt.Println("🔗 Connecting to PostgreSQL...")
    fmt.Printf("   Database: %s\n", os.Getenv("DB_NAME"))
    fmt.Printf("   Host: %s:%s\n", os.Getenv("DB_HOST"), os.Getenv("DB_PORT"))

    db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
    if err != nil {
        log.Fatal("❌ Failed to connect to database:", err)
    }

    // Test the connection
    sqlDB, err := db.DB()
    if err != nil {
        log.Fatal("❌ Failed to get underlying sql.DB:", err)
    }

    if err := sqlDB.Ping(); err != nil {
        log.Fatal("❌ Failed to ping database:", err)
    }

    fmt.Println("✅ Successfully connected to PostgreSQL!")

    // Auto-migrate the schema
    fmt.Println("🔄 Running database migrations...")
    if err := db.AutoMigrate(&User{}, &Organization{}, &Customer{}, &Booking{}, &Service{}, &Staff{}); err != nil {
        log.Fatal("❌ Failed to migrate database:", err)
    }
    fmt.Println("✅ Database migrations completed!")

    // Seed initial data
    seedDatabase()

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
        response := map[string]interface{}{
            "status": "healthy",
            "message": "PostgreSQL Backend is running",
            "database": "PostgreSQL",
            "timestamp": time.Now(),
        }
        json.NewEncoder(w).Encode(response)
    }))

    // Login endpoint
    http.HandleFunc("/api/auth/login", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        // For demo purposes, accept any login
        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "token": "jwt-token-from-postgres-" + fmt.Sprintf("%d", time.Now().Unix()),
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

    // Organizations endpoint
    http.HandleFunc("/api/organizations", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        var organizations []Organization
        if err := db.Find(&organizations).Error; err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }

        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "organizations": organizations,
            },
        }
        json.NewEncoder(w).Encode(response)
    }))

    // Bookings endpoint - handle both GET (list all) and POST (create)
    http.HandleFunc("/api/bookings", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        if r.Method == "GET" {
            // Get pagination parameters
            page := 1
            pageSize := 20
            
            if pageStr := r.URL.Query().Get("page"); pageStr != "" {
                if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
                    page = p
                }
            }
            
            if pageSizeStr := r.URL.Query().Get("pageSize"); pageSizeStr != "" {
                if ps, err := strconv.Atoi(pageSizeStr); err == nil && ps > 0 && ps <= 100 {
                    pageSize = ps
                }
            }
            
            // Get total count
            var totalCount int64
            if err := db.Model(&Booking{}).Count(&totalCount).Error; err != nil {
                http.Error(w, "Failed to count bookings: "+err.Error(), http.StatusInternalServerError)
                return
            }
            
            // Calculate offset
            offset := (page - 1) * pageSize
            
            // Get paginated bookings
            var bookings []Booking
            if err := db.Preload("Customer").Order("created_at DESC").Limit(pageSize).Offset(offset).Find(&bookings).Error; err != nil {
                http.Error(w, err.Error(), http.StatusInternalServerError)
                return
            }

            response := map[string]interface{}{
                "success": true,
                "data": map[string]interface{}{
                    "bookings": bookings,
                    "pagination": map[string]interface{}{
                        "page": page,
                        "pageSize": pageSize,
                        "totalCount": totalCount,
                        "totalPages": (totalCount + int64(pageSize) - 1) / int64(pageSize),
                    },
                },
            }
            json.NewEncoder(w).Encode(response)
        } else if r.Method == "POST" {
            // Create new booking with support for new customer creation
            var requestData struct {
                ServiceID   uint      `json:"service_id"`
                StaffID     uint      `json:"staff_id"`
                CustomerID  interface{} `json:"customer_id"`
                StartTime   time.Time `json:"start_time"`
                Duration    int       `json:"duration"`
                Price       float64   `json:"price"`
                Notes       string    `json:"notes"`
                VehicleInfo string    `json:"vehicle_info"`
                Status      string    `json:"status"`
                NewCustomer *Customer `json:"new_customer,omitempty"`
            }
            
            if err := json.NewDecoder(r.Body).Decode(&requestData); err != nil {
                http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
                return
            }
            
            // Create the booking object
            booking := Booking{
                ServiceID:   requestData.ServiceID,
                StaffID:     requestData.StaffID,
                StartTime:   requestData.StartTime,
                Duration:    requestData.Duration,
                TotalPrice:  requestData.Price,
                Notes:       requestData.Notes,
                VehicleInfo: requestData.VehicleInfo,
                Status:      requestData.Status,
                CreatedAt:   time.Now(),
                UpdatedAt:   time.Now(),
            }
            
            // If new customer data is provided, create the customer first
            if requestData.NewCustomer != nil {
                newCustomer := requestData.NewCustomer
                newCustomer.CreatedAt = time.Now()
                newCustomer.UpdatedAt = time.Now()
                
                if err := db.Create(&newCustomer).Error; err != nil {
                    http.Error(w, "Failed to create customer: "+err.Error(), http.StatusInternalServerError)
                    return
                }
                
                // Update booking with the new customer ID
                booking.CustomerID = newCustomer.ID
                log.Printf("Created new customer with ID: %d", newCustomer.ID)
            } else {
                // Use existing customer ID
                switch v := requestData.CustomerID.(type) {
                case float64:
                    booking.CustomerID = uint(v)
                case string:
                    // Parse string ID if needed
                    if id, err := strconv.ParseUint(v, 10, 32); err == nil {
                        booking.CustomerID = uint(id)
                    }
                default:
                    booking.CustomerID = 0
                }
            }
            
            // Create booking in database
            if err := db.Create(&booking).Error; err != nil {
                http.Error(w, "Failed to create booking: "+err.Error(), http.StatusInternalServerError)
                return
            }
            
            // Load the created booking with customer data
            if err := db.Preload("Customer").First(&booking, booking.ID).Error; err != nil {
                http.Error(w, "Failed to load created booking: "+err.Error(), http.StatusInternalServerError)
                return
            }
            
            response := map[string]interface{}{
                "success": true,
                "data": map[string]interface{}{
                    "booking": booking,
                },
            }
            json.NewEncoder(w).Encode(response)
        } else {
            http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        }
    }))
    
    // Individual booking endpoint - handle GET, PUT, DELETE
    http.HandleFunc("/api/bookings/", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        // Extract booking ID from URL
        pathParts := strings.Split(r.URL.Path, "/")
        if len(pathParts) < 4 {
            http.Error(w, "Invalid booking ID", http.StatusBadRequest)
            return
        }
        
        bookingIDStr := pathParts[3]
        bookingID, err := strconv.ParseUint(bookingIDStr, 10, 64)
        if err != nil {
            http.Error(w, "Invalid booking ID format", http.StatusBadRequest)
            return
        }
        
        if r.Method == "PUT" {
            // Update existing booking
            var updatedBooking Booking
            if err := json.NewDecoder(r.Body).Decode(&updatedBooking); err != nil {
                http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
                return
            }
            
            // Ensure the ID matches
            updatedBooking.ID = uint(bookingID)
            updatedBooking.UpdatedAt = time.Now()
            
            // Update booking in database
            if err := db.Save(&updatedBooking).Error; err != nil {
                http.Error(w, "Failed to update booking: "+err.Error(), http.StatusInternalServerError)
                return
            }
            
            // Load the updated booking with customer data
            if err := db.Preload("Customer").First(&updatedBooking, bookingID).Error; err != nil {
                http.Error(w, "Failed to load updated booking: "+err.Error(), http.StatusInternalServerError)
                return
            }
            
            response := map[string]interface{}{
                "success": true,
                "data": map[string]interface{}{
                    "booking": updatedBooking,
                },
            }
            json.NewEncoder(w).Encode(response)
        } else if r.Method == "DELETE" {
            // Delete booking
            if err := db.Delete(&Booking{}, bookingID).Error; err != nil {
                http.Error(w, "Failed to delete booking: "+err.Error(), http.StatusInternalServerError)
                return
            }
            
            response := map[string]interface{}{
                "success": true,
                "message": "Booking deleted successfully",
            }
            json.NewEncoder(w).Encode(response)
        } else if r.Method == "GET" {
            // Get single booking
            var booking Booking
            if err := db.Preload("Customer").First(&booking, bookingID).Error; err != nil {
                if err == gorm.ErrRecordNotFound {
                    http.Error(w, "Booking not found", http.StatusNotFound)
                } else {
                    http.Error(w, "Failed to fetch booking: "+err.Error(), http.StatusInternalServerError)
                }
                return
            }
            
            response := map[string]interface{}{
                "success": true,
                "data": map[string]interface{}{
                    "booking": booking,
                },
            }
            json.NewEncoder(w).Encode(response)
        } else {
            http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        }
    }))

    // Customers endpoint
    http.HandleFunc("/api/customers", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        var customers []Customer
        if err := db.Find(&customers).Error; err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }

        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "customers": customers,
            },
        }
        json.NewEncoder(w).Encode(response)
    }))

    // Services endpoint
    http.HandleFunc("/api/services", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        var services []Service
        if err := db.Find(&services).Error; err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }

        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "services": services,
            },
        }
        json.NewEncoder(w).Encode(response)
    }))

    // Staff endpoint
    http.HandleFunc("/api/staff", corsHandler(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        
        var staff []Staff
        if err := db.Find(&staff).Error; err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }

        response := map[string]interface{}{
            "success": true,
            "data": map[string]interface{}{
                "staff": staff,
            },
        }
        json.NewEncoder(w).Encode(response)
    }))

    port := os.Getenv("PORT")
    if port == "" {
        port = "8080"
    }

    fmt.Println("\n🚀 DAS Booking PostgreSQL Server starting...")
    fmt.Printf("📍 Server: http://localhost:%s\n", port)
    fmt.Println("📍 Endpoints:")
    fmt.Println("   - GET  /api/health")
    fmt.Println("   - POST /api/auth/login")
    fmt.Println("   - GET  /api/organizations")
    fmt.Println("   - GET  /api/bookings         (list all)")
    fmt.Println("   - POST /api/bookings         (create new)")
    fmt.Println("   - GET  /api/bookings/{id}    (get single)")
    fmt.Println("   - PUT  /api/bookings/{id}    (update)")
    fmt.Println("   - DELETE /api/bookings/{id}  (delete)")
    fmt.Println("   - GET  /api/customers")
    fmt.Println("   - GET  /api/services")
    fmt.Println("   - GET  /api/staff")
    fmt.Println("\n✅ Server is ready to accept connections")
    fmt.Println("💾 Using PostgreSQL database: das_booking_db")
    
    log.Fatal(http.ListenAndServe(":"+port, nil))
}

func seedDatabase() {
    fmt.Println("🌱 Seeding database with initial data...")

    // Seed Organizations
    organizations := []Organization{
        {Name: "AutoCare Plus", Email: "admin@autocare.com", Status: "active"},
        {Name: "Beauty Haven Spa", Email: "contact@beautyhaven.com", Status: "active"},
        {Name: "Quick Fix Garage", Email: "info@quickfix.com", Status: "pending"},
    }
    
    for _, org := range organizations {
        var existingOrg Organization
        if err := db.Where("email = ?", org.Email).First(&existingOrg).Error; err != nil {
            if err == gorm.ErrRecordNotFound {
                db.Create(&org)
                fmt.Printf("   ✓ Created organization: %s\n", org.Name)
            }
        }
    }

    // Seed Customers
    customers := []Customer{
        {FirstName: "John", LastName: "Smith", Phone: "+61412345678", Email: "john.smith@email.com"},
        {FirstName: "Sarah", LastName: "Jones", Phone: "+61423456789", Email: "sarah.jones@email.com"},
        {FirstName: "Mike", LastName: "Brown", Phone: "+61434567890", Email: "mike.brown@email.com"},
    }
    
    for _, customer := range customers {
        var existingCustomer Customer
        if err := db.Where("email = ?", customer.Email).First(&existingCustomer).Error; err != nil {
            if err == gorm.ErrRecordNotFound {
                db.Create(&customer)
                fmt.Printf("   ✓ Created customer: %s %s\n", customer.FirstName, customer.LastName)
            }
        }
    }

    // Seed Services
    services := []Service{
        {Name: "Oil Change", Duration: 60, Price: 85.00},
        {Name: "Brake Service", Duration: 90, Price: 180.00},
        {Name: "Tire Rotation", Duration: 45, Price: 120.00},
        {Name: "Hair Cut & Style", Duration: 60, Price: 65.00},
        {Name: "Facial Treatment", Duration: 75, Price: 95.00},
    }
    
    for _, service := range services {
        var existingService Service
        if err := db.Where("name = ?", service.Name).First(&existingService).Error; err != nil {
            if err == gorm.ErrRecordNotFound {
                db.Create(&service)
                fmt.Printf("   ✓ Created service: %s\n", service.Name)
            }
        }
    }

    // Seed Staff
    staff := []Staff{
        {FirstName: "Mike", LastName: "Johnson"},
        {FirstName: "Tom", LastName: "Wilson"},
        {FirstName: "Lisa", LastName: "Davis"},
        {FirstName: "Emma", LastName: "Garcia"},
    }
    
    for _, member := range staff {
        var existingStaff Staff
        if err := db.Where("first_name = ? AND last_name = ?", member.FirstName, member.LastName).First(&existingStaff).Error; err != nil {
            if err == gorm.ErrRecordNotFound {
                db.Create(&member)
                fmt.Printf("   ✓ Created staff: %s %s\n", member.FirstName, member.LastName)
            }
        }
    }

    // Seed Users
    users := []User{
        {Email: "superadmin@dasyinbook.com", FirstName: "Super", LastName: "Admin", Role: "super_admin"},
        {Email: "admin@autocare.com", FirstName: "Garage", LastName: "Admin", Role: "admin"},
        {Email: "manager@beautyhaven.com", FirstName: "Salon", LastName: "Manager", Role: "manager"},
    }
    
    for _, user := range users {
        var existingUser User
        if err := db.Where("email = ?", user.Email).First(&existingUser).Error; err != nil {
            if err == gorm.ErrRecordNotFound {
                db.Create(&user)
                fmt.Printf("   ✓ Created user: %s\n", user.Email)
            }
        }
    }

    // Seed Bookings
    var firstCustomer Customer
    if err := db.First(&firstCustomer).Error; err == nil {
        bookings := []Booking{
            {
                StartTime:  time.Now().Add(2 * time.Hour),
                EndTime:    time.Now().Add(3 * time.Hour),
                Status:     "confirmed",
                TotalPrice: 85.00,
                CustomerID: firstCustomer.ID,
            },
            {
                StartTime:  time.Now().Add(24 * time.Hour),
                EndTime:    time.Now().Add(25 * time.Hour + 30*time.Minute),
                Status:     "scheduled",
                TotalPrice: 180.00,
                CustomerID: firstCustomer.ID,
            },
        }
        
        for _, booking := range bookings {
            var existingBooking Booking
            if err := db.Where("customer_id = ? AND start_time = ?", booking.CustomerID, booking.StartTime).First(&existingBooking).Error; err != nil {
                if err == gorm.ErrRecordNotFound {
                    db.Create(&booking)
                    fmt.Printf("   ✓ Created booking for: %s\n", booking.Status)
                }
            }
        }
    }

    fmt.Println("✅ Database seeding completed!")
}