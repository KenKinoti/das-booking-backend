package main

import (
	"log"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/config"
	"github.com/kenkinoti/gofiber-das-crm-backend/internal/handlers"
)

// Extra supplier / inventory routes (core ones live in internal/handlers/handler.go).
func init() {
	extraRoutes = append(extraRoutes, func(auth, public *gin.RouterGroup, db *gorm.DB, cfg *config.Config) {
		h := handlers.NewHandler(db, cfg)
		po := auth.Group("/suppliers/purchase-orders")
		po.PUT("/:id", h.UpdatePurchaseOrder)
		po.DELETE("/:id", h.DeletePurchaseOrder)

		cats := auth.Group("/inventory/categories")
		cats.PUT("/:id", h.UpdateProductCategory)
		cats.DELETE("/:id", h.DeleteProductCategory)
	})

	// SKU and PO numbers are unique per organisation, not globally. The models
	// declare single-column unique indexes (idx_products_sku,
	// idx_purchase_orders_order_number); we replace them with composite partial
	// indexes under the SAME names, so gorm's AutoMigrate (which checks by
	// name) keeps them and doesn't try to recreate the global ones.
	extraMigrations = append(extraMigrations, func(db *gorm.DB) {
		scopeUnique(db, "products", "idx_products_sku", "organization_id, sku")
		scopeUnique(db, "purchase_orders", "idx_purchase_orders_order_number", "organization_id, order_number")
	})
}

func scopeUnique(db *gorm.DB, table, index, cols string) {
	var def string
	db.Raw("SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = ? AND indexname = ?", table, index).Scan(&def)
	if strings.Contains(def, "organization_id") {
		return
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DROP INDEX IF EXISTS " + index).Error; err != nil {
			return err
		}
		return tx.Exec("CREATE UNIQUE INDEX " + index + " ON " + table + " (" + cols + ") WHERE deleted_at IS NULL").Error
	})
	if err != nil {
		log.Printf("⚠️  suppliers: could not scope %s per organisation: %v", index, err)
		return
	}
	log.Printf("✅ %s is now unique per organisation", index)
}
