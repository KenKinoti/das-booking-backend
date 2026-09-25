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
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/aitools"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/dbstatus"
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
		c.JSON(200, gin.H{"service": "DASYIN ERP Backend", "version": handlers.AppVersion(), "status": "running", "health": "/health", "api": "/api/v1"})
	})

	// Plan entitlements: blocks module APIs the organisation's plan does not
	// include (402 MODULE_NOT_IN_PLAN). Must precede every route registration.
	r.Use(plansEnforcer(db, cfg))

	// Core API: auth, users, organisations, customers, bookings, services,
	// inventory, POS, suppliers, reports, super-admin …
	internal := handlers.NewHandler(db, cfg)
	internal.SetupRoutes(r)
	for _, register := range extraRootRoutes {
		register(r, db, cfg)
	}

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
	// AI tools (MCP server + in-app assistant) call the API in-process.
	aitools.Engine = r
	aitools.JWTSecret = cfg.JWTSecret
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
	// Supabase's transaction pooler (port 6543) cannot keep our schema setting
	// between statements; its session pooler on port 5432 of the same host can.
	if strings.HasSuffix(strings.ToLower(connCfg.Host), "pooler.supabase.com") && connCfg.Port == 6543 &&
		!strings.EqualFold(os.Getenv("DB_ALLOW_TRANSACTION_POOLER"), "true") {
		connCfg.Port = 5432
		msg := "DATABASE_URL uses Supabase's transaction pooler (6543); connecting to its session pooler (5432) instead"
		log.Printf("ℹ️  %s", msg)
		dbstatus.SetRewrite(msg)
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
				dbstatus.SetPoolerWarning("DATABASE_URL looks like a transaction-mode pooler; use a direct or session-mode connection string")
				break
			}
		}
	}
	if strings.EqualFold(os.Getenv("SKIP_MIGRATIONS"), "true") {
		log.Println("SKIP_MIGRATIONS=true — not changing the schema")
	} else {
		started := time.Now()
		migrate(db)
		dbstatus.SetMigrated()
		log.Printf("✅ migrations finished in %s (%d problems)", time.Since(started).Round(time.Millisecond), dbstatus.Get(false).MigrationErrors)
	}
	bootstrap(db)
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
		for i, m := range extraMigrations {
			m := m
			if err := inSchema(db, func(tx *gorm.DB) error { m(tx); return nil }); err != nil {
				// A statement inside failed and aborted the transaction; run it
				// again outside one (migrations are idempotent).
				log.Printf("⚠️  module migration #%d needed a retry: %v", i, err)
				m(db)
			}
		}
	}()
	order := []string{"core", "booking", "care", "inventory", "pos", "events", "integrations", "messaging", "video", "finance", "crm", "invoicing"}
	for _, name := range order {
		for _, m := range groups[name] {
			m := m
			if err := inSchema(db, func(tx *gorm.DB) error { return tx.AutoMigrate(m) }); err != nil {
				log.Printf("⚠️  migration (%s) %T: %v", name, m, err)
				dbstatus.AddMigrationError(fmt.Sprintf("%s %T: %v", name, m, err))
			}
		}
		log.Printf("✅ migrated %s", name)
	}
}

// bootstrapLockKey is the Postgres advisory lock that serialises first-run
// bootstrap across instances (Cloud Run may start several at once).
const bootstrapLockKey = 727_001_2026

// bootstrap runs bootstrapAdmin and ensureAccounts in ONE transaction that is
// pinned to the ERP schema and holds an advisory lock, so two instances that
// start together can never both create the first organisation, and a query
// can never land on a legacy public table behind a transaction pooler.
func bootstrap(db *gorm.DB) {
	err := inSchema(db, func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", bootstrapLockKey).Error; err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}
		bootstrapAdmin(tx)
		ensureAccounts(tx)
		return nil
	})
	if err != nil {
		log.Printf("❌ bootstrap: %v", err)
	}
}

// qualified returns a table name qualified with the ERP schema.
func qualified(table string) string {
	if s := dbSchema(); s != "" {
		return pgx.Identifier{s, table}.Sanitize()
	}
	return pgx.Identifier{table}.Sanitize()
}

// bootstrapOrganization returns the organisation start-up code should attach
// accounts to. It NEVER creates one when any organisation (even a deleted one)
// exists in the ERP schema: it prefers the oldest live organisation named
// ADMIN_ORG_NAME, then the oldest live organisation. Only a completely empty
// organisations table gets a new one. tx must hold the bootstrap lock.
func bootstrapOrganization(tx *gorm.DB, purpose string) (*models.Organization, error) {
	name := getEnv("ADMIN_ORG_NAME", "DASYIN")
	var total, live int64
	if err := tx.Raw("SELECT COUNT(*), COUNT(*) FILTER (WHERE deleted_at IS NULL) FROM "+qualified("organizations")).Row().Scan(&total, &live); err != nil {
		return nil, fmt.Errorf("count organisations: %w", err)
	}
	var org models.Organization
	if live > 0 {
		q := "SELECT id, name FROM " + qualified("organizations") + " WHERE deleted_at IS NULL"
		err := tx.Raw(q+" AND LOWER(TRIM(name)) = LOWER(TRIM(?)) ORDER BY created_at ASC, id ASC LIMIT 1", name).Scan(&org).Error
		if err != nil || org.ID == "" {
			err = tx.Raw(q + " ORDER BY created_at ASC, id ASC LIMIT 1").Scan(&org).Error
		}
		if err != nil || org.ID == "" {
			return nil, fmt.Errorf("find organisation: %v", err)
		}
		log.Printf("🏢 %s: using existing organisation %q (%s) — %d organisation(s) in schema %q, none created", purpose, org.Name, org.ID, live, dbSchema())
		return &org, nil
	}
	if total > 0 {
		return nil, fmt.Errorf("%s: every organisation in schema %q is deleted; refusing to create another — restore one first", purpose, dbSchema())
	}
	org = models.Organization{ID: uuid.NewString(), Name: name, BusinessType: "general", EnabledModules: []string{}, ModulesConfig: map[string]bool{}}
	if err := tx.Omit("Users", "Customers", "Vehicles", "Services", "Bookings", "ABN").Create(&org).Error; err != nil {
		return nil, fmt.Errorf("create organisation: %w", err)
	}
	log.Printf("🏢 %s: organisations table in schema %q was empty — created organisation %q (%s)", purpose, dbSchema(), org.Name, org.ID)
	return &org, nil
}

// bootstrapAdmin creates a first organisation and super admin when the users
// table is empty so a fresh deployment is usable. Credentials come from
// ADMIN_EMAIL / ADMIN_PASSWORD; if no password is supplied a random one is
// generated and printed once to the logs. tx holds the bootstrap lock.
func bootstrapAdmin(tx *gorm.DB) {
	var n int64
	if err := tx.Raw("SELECT COUNT(*) FROM " + qualified("users")).Row().Scan(&n); err != nil {
		log.Printf("bootstrap: count users: %v", err)
		return
	}
	if n > 0 {
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
	org, err := bootstrapOrganization(tx, "bootstrap")
	if err != nil {
		log.Printf("bootstrap: %v", err)
		return
	}
	user := models.User{ID: uuid.NewString(), Email: email, PasswordHash: string(hash), FirstName: "System", LastName: "Admin", Role: "super_admin", OrganizationID: org.ID, IsActive: true}
	if err := tx.Omit("Organization").Create(&user).Error; err != nil {
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
// Remove the variables again once you can sign in. tx holds the bootstrap lock.
func ensureAccounts(tx *gorm.DB) {
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
	var org *models.Organization
	for _, a := range list {
		hash, err := bcrypt.GenerateFromPassword([]byte(a.password), bcrypt.DefaultCost)
		if err != nil {
			continue
		}
		var existing struct {
			ID      string
			Deleted bool
		}
		// Soft-deleted users are found too: their email is still taken.
		tx.Raw("SELECT id, deleted_at IS NOT NULL AS deleted FROM "+qualified("users")+" WHERE LOWER(email) = LOWER(?) ORDER BY deleted_at DESC NULLS FIRST LIMIT 1", a.email).Scan(&existing)
		if existing.ID != "" && existing.Deleted {
			log.Printf("ensureAccounts: %s was deleted; not recreating it", a.email)
			continue
		}
		if id := existing.ID; id != "" {
			if err := tx.Exec("UPDATE "+qualified("users")+" SET password_hash = ?, is_active = true, updated_at = ? WHERE id = ?", string(hash), time.Now().UTC(), id).Error; err != nil {
				log.Printf("ensureAccounts: update %s: %v", a.email, err)
				continue
			}
			log.Printf("🔑 Password reset for %s", a.email)
			continue
		}
		if org == nil {
			if org, err = bootstrapOrganization(tx, "ensureAccounts"); err != nil {
				log.Printf("ensureAccounts: %v", err)
				return
			}
		}
		u := models.User{ID: uuid.NewString(), Email: a.email, PasswordHash: string(hash), FirstName: a.first, LastName: a.last, Role: a.role, OrganizationID: org.ID, IsActive: true}
		if err := tx.Omit("Organization").Create(&u).Error; err != nil {
			log.Printf("ensureAccounts: create %s: %v", a.email, err)
			return // the transaction is aborted now
		}
		log.Printf("🔑 Created %s (%s) in organisation %q", a.email, a.role, org.Name)
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
			c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Requested-With,X-Organization-ID,Mcp-Session-Id,MCP-Protocol-Version")
			c.Header("Access-Control-Expose-Headers", "Content-Disposition,Mcp-Session-Id,WWW-Authenticate")
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

// inSchema runs fn in a transaction whose search_path is pinned to the ERP
// schema. A transaction always stays on one server connection, even behind a
// transaction-mode pooler, so migrations can never land in the wrong schema.
func inSchema(db *gorm.DB, fn func(tx *gorm.DB) error) error {
	schema := dbSchema()
	return db.Transaction(func(tx *gorm.DB) error {
		if schema != "" {
			if err := tx.Exec("SET LOCAL search_path TO " + pgx.Identifier{schema}.Sanitize() + ", public").Error; err != nil {
				return err
			}
		}
		return fn(tx)
	})
}
