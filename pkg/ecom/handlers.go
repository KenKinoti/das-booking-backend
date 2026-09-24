// Package ecom connects Shopify and WooCommerce stores and imports their
// orders (read-only) into a local table.
package ecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
)

// Connection is a linked online store. Credentials are stored encrypted.
type Connection struct {
	ID             string     `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID          string     `json:"org_id" gorm:"type:varchar(255);not null;index"`
	Platform       string     `json:"platform" gorm:"type:varchar(20);not null"`
	Name           string     `json:"name" gorm:"type:varchar(160);not null"`
	StoreURL       string     `json:"store_url" gorm:"type:varchar(500);not null"`
	APIKeyEnc      string     `json:"-" gorm:"type:text"`
	APISecretEnc   string     `json:"-" gorm:"type:text"`
	KeyHint        string     `json:"key_hint" gorm:"type:varchar(40)"`
	Status         string     `json:"status" gorm:"type:varchar(20);not null;default:'untested'"` // connected | error | untested
	LastError      string     `json:"last_error" gorm:"type:text"`
	LastTestedAt   *time.Time `json:"last_tested_at"`
	LastSyncAt     *time.Time `json:"last_sync_at"`
	OrdersImported int        `json:"orders_imported" gorm:"not null;default:0"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	HasSecret bool `json:"has_secret" gorm:"-"`
}

func (Connection) TableName() string { return "ecom_connections" }

// Order is an order imported from a store.
type Order struct {
	ID             string    `json:"id" gorm:"type:varchar(64);primaryKey"`
	OrgID          string    `json:"org_id" gorm:"type:varchar(255);not null;index"`
	ConnectionID   string    `json:"connection_id" gorm:"type:varchar(64);not null;uniqueIndex:idx_ecom_order_ext"`
	ExternalID     string    `json:"external_id" gorm:"type:varchar(64);not null;uniqueIndex:idx_ecom_order_ext"`
	Platform       string    `json:"platform" gorm:"type:varchar(20);not null"`
	Number         string    `json:"number" gorm:"type:varchar(60)"`
	CustomerName   string    `json:"customer_name" gorm:"type:varchar(200)"`
	CustomerEmail  string    `json:"customer_email" gorm:"type:varchar(255)"`
	Status         string    `json:"status" gorm:"type:varchar(20);index"`
	PlatformStatus string    `json:"platform_status" gorm:"type:varchar(60)"`
	PaymentStatus  string    `json:"payment_status" gorm:"type:varchar(30)"`
	Currency       string    `json:"currency" gorm:"type:varchar(3)"`
	Subtotal       float64   `json:"subtotal" gorm:"type:decimal(14,2)"`
	Tax            float64   `json:"tax" gorm:"type:decimal(14,2)"`
	Shipping       float64   `json:"shipping" gorm:"type:decimal(14,2)"`
	Total          float64   `json:"total" gorm:"type:decimal(14,2)"`
	ItemsCount     int       `json:"items_count"`
	ItemsJSON      string    `json:"-" gorm:"type:text"`
	PlacedAt       time.Time `json:"placed_at" gorm:"index"`
	ImportedAt     time.Time `json:"imported_at"`

	Items     []OrderItem `json:"items" gorm:"-"`
	StoreName string      `json:"store_name" gorm:"-"`
}

func (Order) TableName() string { return "ecom_orders" }

// Migrate creates the tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&Connection{}, &Order{}) }

// Handler serves /api/v1/ecom.
type Handler struct {
	DB     *gorm.DB
	Client *Client
	Enc    *encryption.EncryptionService
	Now    func() time.Time
}

// NewHandler builds the handler. Set ECOM_DEV_ALLOW_LOCAL=1 only in local
// development to allow http:// and private-network stores (for fake servers).
func NewHandler(db *gorm.DB) *Handler {
	allow := os.Getenv("ECOM_DEV_ALLOW_LOCAL") == "1"
	return &Handler{DB: db, Client: NewClient(allow), Enc: encryption.NewEncryptionService()}
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// RegisterRoutes mounts the API on g.
func (h *Handler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("/summary", h.summary)
	g.GET("/connections", h.listConnections)
	g.POST("/connections", h.createConnection)
	g.PUT("/connections/:id", h.updateConnection)
	g.DELETE("/connections/:id", h.deleteConnection)
	g.POST("/connections/:id/test", h.testConnection)
	g.POST("/connections/:id/sync", h.syncConnection)
	g.GET("/orders", h.listOrders)
	g.GET("/orders/:id", h.getOrder)
}

func fail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"success": false, "error": msg, "message": msg})
}

func ok(c *gin.Context, status int, data interface{}) {
	c.JSON(status, gin.H{"success": true, "data": data})
}

func orgOf(c *gin.Context) (string, bool) {
	v, _ := c.Get("org_id")
	org := fmt.Sprint(v)
	if v == nil || org == "" {
		fail(c, http.StatusUnauthorized, "Organisation not found in token")
		return "", false
	}
	return org, true
}

func canManage(c *gin.Context) bool {
	v, _ := c.Get("user_role")
	switch strings.ToLower(fmt.Sprint(v)) {
	case "super_admin", "admin", "owner", "manager":
		return true
	}
	return false
}

// Mask shows only the last four characters of a secret.
func Mask(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= 4 {
		return "••••"
	}
	prefix := ""
	for _, p := range []string{"shpat_", "shpca_", "shppa_", "ck_", "cs_"} {
		if strings.HasPrefix(s, p) {
			prefix = p
		}
	}
	return prefix + "••••" + string(r[len(r)-4:])
}

func (h *Handler) creds(conn *Connection) (Creds, error) {
	key, err := h.Enc.Decrypt(conn.APIKeyEnc)
	if err != nil {
		return Creds{}, errors.New("The saved API key can't be read; please enter it again")
	}
	sec, err := h.Enc.Decrypt(conn.APISecretEnc)
	if err != nil {
		return Creds{}, errors.New("The saved API secret can't be read; please enter it again")
	}
	return Creds{Platform: conn.Platform, StoreURL: conn.StoreURL, APIKey: key, APISecret: sec}, nil
}

func (h *Handler) loadConn(c *gin.Context, org string) (*Connection, bool) {
	var conn Connection
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&conn).Error; err != nil {
		fail(c, http.StatusNotFound, "Store connection not found")
		return nil, false
	}
	conn.HasSecret = conn.APISecretEnc != ""
	return &conn, true
}

// ---------------------------------------------------------------------------

func (h *Handler) summary(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var conns, orders, open, last30 int64
	h.DB.Model(&Connection{}).Where("org_id = ?", org).Count(&conns)
	h.DB.Model(&Order{}).Where("org_id = ?", org).Count(&orders)
	h.DB.Model(&Order{}).Where("org_id = ? AND status IN ?", org, []string{"pending", "processing"}).Count(&open)
	since := h.now().AddDate(0, 0, -30)
	h.DB.Model(&Order{}).Where("org_id = ? AND placed_at >= ?", org, since).Count(&last30)
	type rev struct {
		Currency string  `json:"currency"`
		Total    float64 `json:"total"`
		N        int64   `json:"orders"`
	}
	revs := []rev{}
	h.DB.Model(&Order{}).Select("currency, SUM(total) AS total, COUNT(*) AS n").
		Where("org_id = ? AND placed_at >= ? AND status NOT IN ?", org, since, []string{"cancelled", "refunded"}).
		Group("currency").Order("n DESC").Scan(&revs)
	var lastSync *time.Time
	var c1 Connection
	if h.DB.Where("org_id = ? AND last_sync_at IS NOT NULL", org).Order("last_sync_at DESC").First(&c1).Error == nil {
		lastSync = c1.LastSyncAt
	}
	ok(c, http.StatusOK, gin.H{
		"connections": conns, "orders": orders, "open_orders": open, "orders_30d": last30,
		"revenue_30d": revs, "last_sync_at": lastSync,
	})
}

func (h *Handler) listConnections(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var rows []Connection
	if err := h.DB.Where("org_id = ?", org).Order("created_at").Find(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not load store connections")
		return
	}
	for i := range rows {
		rows[i].HasSecret = rows[i].APISecretEnc != ""
	}
	ok(c, http.StatusOK, gin.H{"connections": rows})
}

type connInput struct {
	Platform  string `json:"platform"`
	Name      string `json:"name"`
	StoreURL  string `json:"store_url"`
	APIKey    string `json:"api_key"`
	APISecret string `json:"api_secret"`
}

func (h *Handler) apply(c *gin.Context, org string, conn *Connection, creating bool) bool {
	var in connInput
	if err := c.ShouldBindJSON(&in); err != nil {
		fail(c, http.StatusBadRequest, "Invalid request body")
		return false
	}
	if creating {
		in.Platform = strings.ToLower(strings.TrimSpace(in.Platform))
		if in.Platform != Shopify && in.Platform != WooCommerce {
			fail(c, http.StatusBadRequest, "Choose Shopify or WooCommerce")
			return false
		}
		conn.Platform = in.Platform
	}
	base, err := NormalizeStoreURL(conn.Platform, in.StoreURL, h.Client.AllowPrivate)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return false
	}
	var dup int64
	h.DB.Model(&Connection{}).Where("org_id = ? AND store_url = ? AND id <> ?", org, base, conn.ID).Count(&dup)
	if dup > 0 {
		fail(c, http.StatusConflict, "That store is already connected")
		return false
	}
	urlChanged := conn.StoreURL != "" && conn.StoreURL != base
	conn.StoreURL = base
	conn.Name = strings.TrimSpace(in.Name)
	if len([]rune(conn.Name)) > 160 {
		conn.Name = string([]rune(conn.Name)[:160])
	}
	if conn.Name == "" {
		conn.Name = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	}
	key, sec := strings.TrimSpace(in.APIKey), strings.TrimSpace(in.APISecret)
	if creating || key != "" {
		if key == "" {
			fail(c, http.StatusBadRequest, map[bool]string{true: "Enter the Admin API access token", false: "Enter the consumer key"}[conn.Platform == Shopify])
			return false
		}
		if len(key) > 500 || len(sec) > 500 {
			fail(c, http.StatusBadRequest, "That key is too long")
			return false
		}
		enc, err := h.Enc.Encrypt(key)
		if err != nil {
			fail(c, http.StatusInternalServerError, "Could not secure the API key")
			return false
		}
		conn.APIKeyEnc, conn.KeyHint = enc, Mask(key)
	} else if urlChanged {
		fail(c, http.StatusBadRequest, "Re-enter the API key when changing the store address")
		return false
	}
	if conn.Platform == WooCommerce {
		if sec != "" {
			enc, err := h.Enc.Encrypt(sec)
			if err != nil {
				fail(c, http.StatusInternalServerError, "Could not secure the API secret")
				return false
			}
			conn.APISecretEnc = enc
		} else if creating || key != "" {
			fail(c, http.StatusBadRequest, "Enter the consumer secret")
			return false
		}
	}
	conn.Status = "untested"
	conn.LastError = ""
	return true
}

// runTest checks the connection and records the result.
func (h *Handler) runTest(conn *Connection) (string, error) {
	cr, err := h.creds(conn)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var name string
		name, err = h.Client.Test(ctx, cr)
		now := h.now()
		conn.LastTestedAt = &now
		if err == nil {
			conn.Status, conn.LastError = "connected", ""
			h.DB.Model(conn).Updates(map[string]interface{}{"status": conn.Status, "last_error": "", "last_tested_at": now})
			return name, nil
		}
	}
	conn.Status, conn.LastError = "error", err.Error()
	h.DB.Model(conn).Updates(map[string]interface{}{"status": conn.Status, "last_error": conn.LastError, "last_tested_at": conn.LastTestedAt})
	return "", err
}

func (h *Handler) createConnection(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !canManage(c) {
		fail(c, http.StatusForbidden, "Only admins can connect stores")
		return
	}
	conn := Connection{ID: uuid.New().String(), OrgID: org}
	if !h.apply(c, org, &conn, true) {
		return
	}
	if err := h.DB.Create(&conn).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save the store connection")
		return
	}
	name, terr := h.runTest(&conn)
	conn.HasSecret = conn.APISecretEnc != ""
	ok(c, http.StatusCreated, gin.H{"connection": conn, "test": testResult(name, terr)})
}

func testResult(name string, err error) gin.H {
	if err != nil {
		return gin.H{"ok": false, "message": err.Error()}
	}
	msg := "Connected"
	if name != "" {
		msg = "Connected to " + name
	}
	return gin.H{"ok": true, "message": msg, "store_name": name}
}

func (h *Handler) updateConnection(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !canManage(c) {
		fail(c, http.StatusForbidden, "Only admins can change store connections")
		return
	}
	conn, found := h.loadConn(c, org)
	if !found {
		return
	}
	if !h.apply(c, org, conn, false) {
		return
	}
	if err := h.DB.Save(conn).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not save the store connection")
		return
	}
	name, terr := h.runTest(conn)
	conn.HasSecret = conn.APISecretEnc != ""
	ok(c, http.StatusOK, gin.H{"connection": conn, "test": testResult(name, terr)})
}

func (h *Handler) deleteConnection(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	if !canManage(c) {
		fail(c, http.StatusForbidden, "Only admins can remove store connections")
		return
	}
	conn, found := h.loadConn(c, org)
	if !found {
		return
	}
	keep := c.Query("keep_orders") == "true"
	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if !keep {
			if err := tx.Where("org_id = ? AND connection_id = ?", org, conn.ID).Delete(&Order{}).Error; err != nil {
				return err
			}
		}
		return tx.Delete(conn).Error
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "Could not remove the store connection")
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) testConnection(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	conn, found := h.loadConn(c, org)
	if !found {
		return
	}
	name, err := h.runTest(conn)
	ok(c, http.StatusOK, gin.H{"connection": conn, "test": testResult(name, err)})
}

// Sync imports orders for one connection and returns (new, updated).
func (h *Handler) Sync(ctx context.Context, conn *Connection) (int, int, error) {
	cr, err := h.creds(conn)
	if err != nil {
		return 0, 0, err
	}
	var since *time.Time
	if conn.LastSyncAt != nil {
		s := conn.LastSyncAt.Add(-24 * time.Hour) // overlap to catch late updates
		since = &s
	}
	started := h.now()
	orders, err := h.Client.FetchOrders(ctx, cr, since)
	if err != nil {
		h.DB.Model(conn).Updates(map[string]interface{}{"status": "error", "last_error": err.Error()})
		conn.Status, conn.LastError = "error", err.Error()
		return 0, 0, err
	}
	var existing []string
	h.DB.Model(&Order{}).Where("connection_id = ?", conn.ID).Pluck("external_id", &existing)
	have := map[string]bool{}
	for _, id := range existing {
		have[id] = true
	}
	created, updated := 0, 0
	rows := make([]Order, 0, len(orders))
	for _, o := range orders {
		items, _ := json.Marshal(o.Items)
		placed := o.PlacedAt
		if placed.IsZero() {
			placed = started
		}
		rows = append(rows, Order{
			ID: uuid.New().String(), OrgID: conn.OrgID, ConnectionID: conn.ID, ExternalID: o.ExternalID, Platform: conn.Platform,
			Number: trunc(o.Number, 60), CustomerName: trunc(o.CustomerName, 200), CustomerEmail: trunc(o.CustomerEmail, 255),
			Status: o.Status, PlatformStatus: trunc(o.PlatformStatus, 60), PaymentStatus: trunc(o.PaymentStatus, 30), Currency: trunc(o.Currency, 3),
			Subtotal: o.Subtotal, Tax: o.Tax, Shipping: o.Shipping, Total: o.Total, ItemsCount: o.ItemsCount,
			ItemsJSON: string(items), PlacedAt: placed, ImportedAt: started,
		})
		if have[o.ExternalID] {
			updated++
		} else {
			created++
			have[o.ExternalID] = true
		}
	}
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		for i := 0; i < len(rows); i += 200 {
			end := i + 200
			if end > len(rows) {
				end = len(rows)
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "connection_id"}, {Name: "external_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"number", "customer_name", "customer_email", "status", "platform_status",
					"payment_status", "currency", "subtotal", "tax", "shipping", "total", "items_count", "items_json", "placed_at", "imported_at"}),
			}).Create(rows[i:end]).Error; err != nil {
				return err
			}
		}
		var total int64
		tx.Model(&Order{}).Where("connection_id = ?", conn.ID).Count(&total)
		conn.OrdersImported = int(total)
		conn.LastSyncAt = &started
		conn.Status, conn.LastError = "connected", ""
		return tx.Model(conn).Updates(map[string]interface{}{"orders_imported": total, "last_sync_at": started, "status": "connected", "last_error": ""}).Error
	})
	return created, updated, err
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (h *Handler) syncConnection(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	conn, found := h.loadConn(c, org)
	if !found {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	created, updated, err := h.Sync(ctx, conn)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			fail(c, http.StatusBadGateway, err.Error())
			return
		}
		fail(c, http.StatusBadGateway, "Import failed: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"created": created, "updated": updated, "connection": conn})
}

func (h *Handler) listOrders(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	q := h.DB.Model(&Order{}).Where("org_id = ?", org)
	if s := c.Query("status"); s != "" && s != "all" {
		if s == "open" {
			q = q.Where("status IN ?", []string{"pending", "processing"})
		} else {
			q = q.Where("status = ?", s)
		}
	}
	if id := c.Query("connection_id"); id != "" {
		q = q.Where("connection_id = ?", id)
	}
	if s := strings.ToLower(strings.TrimSpace(c.Query("q"))); s != "" {
		like := "%" + s + "%"
		q = q.Where("(LOWER(number) LIKE ? OR LOWER(customer_name) LIKE ? OR LOWER(customer_email) LIKE ?)", like, like, like)
	}
	var total int64
	q.Count(&total)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	per, _ := strconv.Atoi(c.DefaultQuery("per_page", "25"))
	if page < 1 {
		page = 1
	}
	if per < 1 || per > 100 {
		per = 25
	}
	var rows []Order
	if err := q.Order("placed_at DESC").Offset((page - 1) * per).Limit(per).Find(&rows).Error; err != nil {
		fail(c, http.StatusInternalServerError, "Could not load orders")
		return
	}
	names := h.storeNames(org)
	for i := range rows {
		rows[i].StoreName = names[rows[i].ConnectionID]
	}
	ok(c, http.StatusOK, gin.H{"orders": rows, "total": total, "page": page, "per_page": per})
}

func (h *Handler) storeNames(org string) map[string]string {
	var conns []Connection
	h.DB.Select("id, name").Where("org_id = ?", org).Find(&conns)
	m := map[string]string{}
	for _, c := range conns {
		m[c.ID] = c.Name
	}
	return m
}

func (h *Handler) getOrder(c *gin.Context) {
	org, okk := orgOf(c)
	if !okk {
		return
	}
	var o Order
	if err := h.DB.Where("id = ? AND org_id = ?", c.Param("id"), org).First(&o).Error; err != nil {
		fail(c, http.StatusNotFound, "Order not found")
		return
	}
	_ = json.Unmarshal([]byte(o.ItemsJSON), &o.Items)
	o.StoreName = h.storeNames(org)[o.ConnectionID]
	ok(c, http.StatusOK, o)
}
