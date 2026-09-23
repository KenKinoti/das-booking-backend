// Production entrypoint for the DASYIN ERP backend (deployed to Cloud Run).
//
// It wires together the core booking/organisation API (internal/handlers), the
// finance, CRM and invoicing modules, messaging and video signalling, and
// migrates every table those handlers need.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/middleware"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/booking"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/crm"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/finance"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/invoicing"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/messaging"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/video"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Note: no .env file found, using environment variables")
	}

	cfg := config.Load()
	if os.Getenv("ENVIRONMENT") == "" {
		cfg.Environment = "production"
	}
	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
		if cfg.JWTSecret == "default-secret-change-me" || len(cfg.JWTSecret) < 16 {
			log.Println("⚠️  JWT_SECRET is missing or weak — set a long random JWT_SECRET in the service environment")
		}
	}

	// Open the pool without pinging so the HTTP server can start listening
	// immediately (Cloud Run kills containers that don't bind $PORT quickly).
	// The database connection, migrations and first-run bootstrap happen in
	// the background with retries.
	db, err := openDatabase()
	if err != nil {
		log.Fatal("Invalid database configuration: ", err)
	}
	go prepareDatabase(db)

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.Logger())
	r.Use(corsMiddleware())
	r.Use(middleware.SecurityHeaders())

	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{"service": "DASYIN ERP Backend", "version": "2.0.0", "status": "running", "health": "/health", "api": "/api/v1"})
	})

	// Core API: auth, users, organisations, customers, bookings, services,
	// inventory, POS, suppliers, reports, super-admin …
	internal := handlers.NewHandler(db, cfg)
	internal.SetupRoutes(r)

	// Authenticated module APIs
	v1 := r.Group("/api/v1")
	v1.GET("/health", internal.HealthCheck)
	auth := v1.Group("/")
	auth.Use(middleware.AuthRequired(cfg), orgScope())
	{
		finance.NewHandler(finance.NewService(db)).RegisterRoutes(auth.Group("/finance"))
		crm.NewHandler(crm.NewService(db)).RegisterRoutes(auth.Group("/crm"))
		booking.NewHandler(db).RegisterRoutes(auth.Group("/booking"))

		inv := invoicing.NewHandler(db)
		inv.Mailer = invoicing.SMTPMailerFromEnv()
		inv.PublicURL = os.Getenv("PUBLIC_APP_URL")
		inv.RegisterRoutes(auth.Group("/invoicing"))
		inv.RegisterPublicRoutes(v1.Group("/public"))

		prod := auth.Group("/production")
		prod.GET("/dashboard", getProductionDashboard)
		prod.GET("/work-orders", getWorkOrders)
		prod.POST("/work-orders", createWorkOrder)
		prod.GET("/boms", getBOMs)
		prod.POST("/boms", createBOM)

		proj := auth.Group("/projects")
		proj.GET("/dashboard", getProjectDashboard)
		proj.GET("/projects", getProjects)
		proj.POST("/projects", createProject)
		proj.GET("/time-entries", getTimeEntries)
		proj.POST("/time-entries", createTimeEntry)

		ecom := auth.Group("/ecommerce")
		ecom.GET("/dashboard", getEcommerceDashboard)
		ecom.GET("/platforms", getPlatforms)
		ecom.POST("/platforms", createPlatform)
		ecom.GET("/orders", getOnlineOrders)
		ecom.POST("/sync", triggerSync)

		hcmG := auth.Group("/hcm")
		hcmG.GET("/dashboard", getHCMDashboard)
		hcmG.GET("/employees", getEmployees)
		hcmG.POST("/employees", createEmployee)
		hcmG.GET("/time-entries", getHCMTimeEntries)
		hcmG.POST("/time-entries", createHCMTimeEntry)
		hcmG.GET("/payroll", getPayrollRecords)
		hcmG.POST("/payroll", createPayrollPeriod)

		// Integration settings (email, SMS, payments) — always scoped to the caller's organisation
		ih := handlers.NewSimpleIntegrationHandler(db)
		integ := auth.Group("/integrations/organizations/:organizationId", scopeOrgParam())
		integ.GET("/settings", ih.GetIntegrationSettings)
		integ.PUT("/settings", middleware.RequireRole("admin"), ih.UpdateIntegrationSettings)
		integ.POST("/test/stripe", ih.TestStripeConnection)
		integ.POST("/test/email", ih.TestEmailConnection)
		integ.POST("/test/twilio", ih.TestTwilioConnection)
		integ.POST("/test/whatsapp", ih.TestWhatsAppConnection)
		integ.POST("/send/test-email", ih.SendTestEmail)
		integ.POST("/send/test-sms", ih.SendTestSMS)
		integ.POST("/send/test-whatsapp", ih.SendTestWhatsApp)
		integ.GET("/logs/integrations", ih.GetIntegrationLogs)
		integ.GET("/logs/communications", ih.GetCommunicationLogs)
		integ.GET("/transactions/payments", ih.GetPaymentTransactions)

		auth.GET("/admin/overview", getCompleteAdminOverview)
		auth.GET("/admin/modules", getCompleteModuleStatus)
	}

	messaging.RegisterRoutes(r, messaging.NewHandler(db), cfg)
	video.RegisterRoutes(r, video.NewHandler(db))

	port := cfg.Port
	log.Printf("🚀 DASYIN ERP Backend listening on :%s (%s)", port, cfg.Environment)
	if err := r.Run(":" + port); err != nil {
		log.Fatal("Failed to start server: ", err)
	}
}

func openDatabase() (*gorm.DB, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=%s TimeZone=UTC",
			getEnv("DB_HOST", "localhost"), getEnv("DB_USER", "postgres"), getEnv("DB_PASSWORD", ""),
			getEnv("DB_NAME", "das_erp_production"), getEnv("DB_PORT", "5432"), getEnv("DB_SSLMODE", "require"))
	}
	db, err := gorm.Open(postgres.Open(dbURL), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               logger.Default.LogMode(logger.Warn),
		NowFunc:              func() time.Time { return time.Now().UTC() },
		// Many legacy models reference each other loosely; creating FK
		// constraints during migration breaks on existing data.
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetConnMaxLifetime(time.Hour)
	return db, nil
}

// prepareDatabase waits for the database, then migrates and bootstraps.
// Set SKIP_MIGRATIONS=true to skip schema changes on startup.
func prepareDatabase(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err != nil {
		log.Printf("❌ database handle: %v", err)
		return
	}
	for attempt := 1; ; attempt++ {
		if err := sqlDB.Ping(); err == nil {
			log.Printf("✅ database connected")
			break
		} else {
			log.Printf("⏳ database not reachable yet (attempt %d): %v", attempt, err)
		}
		if attempt >= 60 {
			log.Printf("❌ giving up on database connection — check DATABASE_URL / Cloud SQL connection")
			return
		}
		time.Sleep(5 * time.Second)
	}
	if strings.EqualFold(os.Getenv("SKIP_MIGRATIONS"), "true") {
		log.Println("SKIP_MIGRATIONS=true — not changing the schema")
	} else {
		started := time.Now()
		migrate(db)
		log.Printf("✅ migrations finished in %s", time.Since(started).Round(time.Millisecond))
	}
	bootstrapAdmin(db)
}

// migrate creates every table used by the registered handlers. Each group is
// migrated independently so one legacy model can't block the rest.
func migrate(db *gorm.DB) {
	groups := map[string][]interface{}{
		"core": {
			&models.Organization{}, &models.User{}, &models.RefreshToken{}, &models.UserPermission{},
			&models.Role{}, &models.RolePermission{}, &models.OrganizationInvitation{}, &models.AuditLog{},
			&models.OrganizationBranding{}, &models.OrganizationSettings{}, &models.OrganizationSubscription{},
			&models.OrganizationModules{},
		},
		"booking": {
			&models.Customer{}, &models.Vehicle{}, &models.Service{}, &models.Booking{},
		},
		"care": {
			&models.Participant{}, &models.EmergencyContact{}, &models.Shift{}, &models.Document{}, &models.CarePlan{},
		},
		"inventory": {
			&models.ProductCategory{}, &models.Brand{}, &models.Product{}, &models.Supplier{}, &models.PurchaseOrder{},
			&models.PurchaseOrderItem{}, &models.InventoryItem{}, &models.InventoryLocation{}, &models.InventoryMovement{},
		},
		"pos": {
			&models.POSTransaction{}, &models.POSItem{}, &models.POSPayment{}, &models.CashDrawer{}, &models.Discount{},
			&models.TaxRate{}, &models.LaybyPayment{}, &models.LaybyItem{}, &models.LaybyPaymentEntry{},
		},
		"events": {
			&models.EventCategory{}, &models.Event{}, &models.TicketType{}, &models.EventRegistration{},
			&models.EventSession{}, &models.EventImage{}, &models.EventReview{},
		},
		"integrations": {
			&models.IntegrationSettings{}, &models.IntegrationLog{}, &models.PaymentTransaction{}, &models.CommunicationLog{},
		},
		"messaging": {
			&models.MessageThread{}, &models.MessageParticipant{}, &models.Message{}, &models.MessageAttachment{},
			&models.MessageReaction{}, &models.MessageReadStatus{}, &models.MessageIntegration{}, &models.MessageSettings{},
			&models.WebSocketConnection{}, &models.TypingIndicator{}, &models.MessageTemplate{},
		},
		"video": {
			&models.VideoCall{}, &models.CallParticipant{}, &models.CallRecording{}, &models.LiveStream{},
			&models.StreamViewer{}, &models.StreamRecording{}, &models.ScreenShare{}, &models.VideoSettings{}, &models.WebRTCSignal{},
		},
		"finance": {
			&finance.ChartOfAccount{}, &finance.JournalEntry{}, &finance.JournalEntryLine{}, &finance.Invoice{},
			&finance.InvoiceLineItem{}, &finance.Bill{}, &finance.BillLineItem{}, &finance.Vendor{}, &finance.Payment{},
			&finance.BankAccount{}, &finance.BankTransaction{}, &finance.GeneralLedger{}, &finance.AccountBalance{},
			&finance.AuditTrail{}, &finance.AccountingPeriod{},
		},
		"crm":       {&crm.Lead{}, &crm.CRMCustomer{}, &crm.Opportunity{}},
		"invoicing": invoicing.Models(),
	}
	order := []string{"core", "booking", "care", "inventory", "pos", "events", "integrations", "messaging", "video", "finance", "crm", "invoicing"}
	for _, name := range order {
		for _, m := range groups[name] {
			if err := db.AutoMigrate(m); err != nil {
				log.Printf("⚠️  migration (%s) %T: %v", name, m, err)
			}
		}
		log.Printf("✅ migrated %s", name)
	}
}

// bootstrapAdmin creates a first organisation and super admin when the users
// table is empty so a fresh deployment is usable. Credentials come from
// ADMIN_EMAIL / ADMIN_PASSWORD; if no password is supplied a random one is
// generated and printed once to the logs.
func bootstrapAdmin(db *gorm.DB) {
	var n int64
	if err := db.Model(&models.User{}).Count(&n).Error; err != nil || n > 0 {
		return
	}
	email := getEnv("ADMIN_EMAIL", "admin@dasyin.com")
	password := os.Getenv("ADMIN_PASSWORD")
	generated := false
	if password == "" {
		b := make([]byte, 9)
		_, _ = rand.Read(b)
		password = hex.EncodeToString(b)
		generated = true
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("bootstrap: %v", err)
		return
	}
	org := models.Organization{ID: uuid.NewString(), Name: getEnv("ADMIN_ORG_NAME", "DASYIN"), BusinessType: "general", ABN: fmt.Sprintf("%011d", time.Now().Unix()%100000000000)}
	if err := db.Create(&org).Error; err != nil {
		log.Printf("bootstrap organisation: %v", err)
		return
	}
	user := models.User{ID: uuid.NewString(), Email: email, PasswordHash: string(hash), FirstName: "System", LastName: "Admin", Role: "super_admin", OrganizationID: org.ID, IsActive: true}
	if err := db.Create(&user).Error; err != nil {
		log.Printf("bootstrap user: %v", err)
		return
	}
	if generated {
		log.Printf("🔐 Created super admin %s with generated password: %s  (change it after signing in)", email, password)
	} else {
		log.Printf("🔐 Created super admin %s", email)
	}
}

// orgScope exposes the organisation from the JWT under the key the finance
// and CRM modules read, so their data is isolated per organisation.
func orgScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		if v, ok := c.Get("org_id"); ok && v != nil {
			c.Set("organization_id", fmt.Sprint(v))
		}
		c.Next()
	}
}

// scopeOrgParam pins the :organizationId path parameter to the caller's own
// organisation. Super admins may address another organisation explicitly.
func scopeOrgParam() gin.HandlerFunc {
	return func(c *gin.Context) {
		org := fmt.Sprint(c.MustGet("org_id"))
		role, _ := c.Get("user_role")
		for i, p := range c.Params {
			if p.Key != "organizationId" {
				continue
			}
			if role != "super_admin" || p.Value == "" || p.Value == "current" {
				c.Params[i].Value = org
			}
		}
		c.Next()
	}
}

// corsMiddleware echoes the request origin when it is in CORS_ORIGINS
// (comma separated, "*" allows any origin).
func corsMiddleware() gin.HandlerFunc {
	allowed := map[string]bool{}
	any := false
	for _, o := range strings.Split(getEnv("CORS_ORIGINS", "*"), ",") {
		o = strings.TrimSpace(strings.TrimRight(o, "/"))
		if o == "*" {
			any = true
		} else if o != "" {
			allowed[o] = true
		}
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && (any || allowed[origin]) {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Requested-With,X-Organization-ID")
			c.Header("Access-Control-Expose-Headers", "Content-Disposition")
			c.Header("Access-Control-Max-Age", "600")
		}
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
