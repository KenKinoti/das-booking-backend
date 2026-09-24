// Production entrypoint for the DASYIN ERP backend (deployed to Cloud Run).
//
// It wires together the core booking/organisation API (internal/handlers), the
// finance, CRM and invoicing modules, messaging and video signalling, and
// migrates every table those handlers need.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
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

	// Plan entitlements: blocks module APIs the organisation's plan does not
	// include (402 MODULE_NOT_IN_PLAN). Must precede every route registration.
	r.Use(plansEnforcer(db, cfg))

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

		// Feature modules register themselves from cmd/production/routes_*.go
		for _, register := range extraRoutes {
			register(auth, v1.Group("/public"), db, cfg)
		}

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
	// All ERP tables live in their own Postgres schema (DB_SCHEMA, default
	// "erp") so they never clash with tables created by older SQL scripts in
	// the public schema.
	schema := dbSchema()
	connCfg, err := pgx.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if schema != "" {
		connCfg.RuntimeParams["search_path"] = schema + ",public"
	}
	// Never use server-side prepared statements. Connection poolers in
	// transaction mode (Supabase :6543, PgBouncer, Neon pooler) hand each
	// query to a different server connection, so cached statements vanish
	// and Postgres answers "prepared statement ... does not exist" (26000).
	connCfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	connCfg.StatementCacheCapacity = 0
	connCfg.DescriptionCacheCapacity = 0
	sqlDB := stdlib.OpenDB(*connCfg, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		if schema == "" {
			return nil
		}
		// Re-assert for poolers that ignore startup parameters.
		_, err := conn.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()+", public")
		return err
	}))
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
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
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetConnMaxLifetime(time.Hour)
	return db, nil
}

// dbSchema returns the Postgres schema for ERP tables ("" = use the default search_path).
func dbSchema() string {
	s := strings.TrimSpace(getEnv("DB_SCHEMA", "erp"))
	if strings.EqualFold(s, "public") || s == "-" {
		return ""
	}
	return s
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
	if schema := dbSchema(); schema != "" {
		if err := db.Exec("CREATE SCHEMA IF NOT EXISTS " + pgx.Identifier{schema}.Sanitize()).Error; err != nil {
			log.Printf("⚠️  could not create schema %q: %v", schema, err)
		} else {
			log.Printf("✅ using database schema %q", schema)
		}
		// Make the schema the role's default too, so server connections a
		// pooler opens without our startup parameters still find ERP tables.
		if err := db.Exec("ALTER ROLE CURRENT_USER SET search_path TO " + pgx.Identifier{schema}.Sanitize() + ", public").Error; err != nil {
			log.Printf("⚠️  could not set the role's default search_path: %v", err)
		}
		// Detect transaction-mode poolers that drop our search_path: sample a
		// few connections and warn loudly, because queries would then hit the
		// legacy public tables instead of the ERP ones.
		for i := 0; i < 5; i++ {
			var sp string
			if err := db.Raw("SHOW search_path").Scan(&sp).Error; err == nil && !strings.Contains(sp, schema) {
				log.Printf("🚨 DATABASE_URL looks like a transaction-mode pooler (search_path=%q). ERP data may be read from the wrong tables. Use a direct or SESSION-mode connection string (e.g. Supabase session pooler, port 5432).", sp)
				break
			}
		}
	}
	if strings.EqualFold(os.Getenv("SKIP_MIGRATIONS"), "true") {
		log.Println("SKIP_MIGRATIONS=true — not changing the schema")
	} else {
		started := time.Now()
		migrate(db)
		log.Printf("✅ migrations finished in %s", time.Since(started).Round(time.Millisecond))
	}
	bootstrapAdmin(db)
	ensureAccounts(db)
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
	defer func() {
		for _, m := range extraMigrations {
			m(db)
		}
	}()
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

// ensureAccounts is a recovery tool for lost passwords.
//
//   - RESET_ADMIN_EMAIL + RESET_ADMIN_PASSWORD: sets that user's password (and
//     reactivates them); creates them as a super admin if they don't exist.
//   - ENSURE_TEST_ACCOUNTS=true: makes sure the four test accounts shown on the
//     login page exist with password TEST_ACCOUNTS_PASSWORD (default Test123!@#).
//
// Remove the variables again once you can sign in.
func ensureAccounts(db *gorm.DB) {
	type acct struct{ email, first, last, role, password string }
	var list []acct
	if e, p := strings.TrimSpace(os.Getenv("RESET_ADMIN_EMAIL")), os.Getenv("RESET_ADMIN_PASSWORD"); e != "" && p != "" {
		list = append(list, acct{e, "System", "Admin", "super_admin", p})
	}
	if strings.EqualFold(os.Getenv("ENSURE_TEST_ACCOUNTS"), "true") {
		pw := getEnv("TEST_ACCOUNTS_PASSWORD", "Test123!@#")
		list = append(list,
			acct{"kennedy@dasyin.com.au", "Ken", "Kinoti", "super_admin", pw},
			acct{"Michael.Thomas75@test.com", "Michael", "Thomas", "admin", pw},
			acct{"David.Jones20@test.com", "David", "Jones", "support_coordinator", pw},
			acct{"Jane.Jones90@test.com", "Jane", "Jones", "care_worker", pw},
		)
	}
	if len(list) == 0 {
		return
	}
	var org models.Organization
	if err := db.Order("created_at ASC").First(&org).Error; err != nil {
		org = models.Organization{ID: uuid.NewString(), Name: getEnv("ADMIN_ORG_NAME", "DASYIN"), BusinessType: "general", ABN: fmt.Sprintf("%011d", time.Now().Unix()%100000000000)}
		if err := db.Create(&org).Error; err != nil {
			log.Printf("ensureAccounts: create organisation: %v", err)
			return
		}
	}
	for _, a := range list {
		hash, err := bcrypt.GenerateFromPassword([]byte(a.password), bcrypt.DefaultCost)
		if err != nil {
			continue
		}
		var u models.User
		err = db.Where("LOWER(email) = LOWER(?)", a.email).First(&u).Error
		if err == nil {
			if err := db.Model(&models.User{}).Where("id = ?", u.ID).Updates(map[string]interface{}{"password_hash": string(hash), "is_active": true}).Error; err != nil {
				log.Printf("ensureAccounts: update %s: %v", a.email, err)
				continue
			}
			log.Printf("🔑 Password reset for %s", a.email)
			continue
		}
		u = models.User{ID: uuid.NewString(), Email: a.email, PasswordHash: string(hash), FirstName: a.first, LastName: a.last, Role: a.role, OrganizationID: org.ID, IsActive: true}
		if err := db.Create(&u).Error; err != nil {
			log.Printf("ensureAccounts: create %s: %v", a.email, err)
			continue
		}
		log.Printf("🔑 Created %s (%s)", a.email, a.role)
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
