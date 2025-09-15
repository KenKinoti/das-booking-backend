package inventory

import (
	"github.com/google/uuid"
	"gorm.io/gorm"
	"time"
)

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// InitializeSampleProducts creates sample products for testing
func (s *Service) InitializeSampleProducts(orgID string) error {
	products := []Product{
		{
			ID: uuid.New().String(), OrganizationID: orgID, SKU: "PART-001", Name: "Brake Pads",
			Description: "High-quality brake pads", Category: "Parts", Brand: "AutoPro",
			CostPrice: 45.00, SellingPrice: 75.00, QuantityOnHand: 25, ReorderLevel: 10,
		},
		{
			ID: uuid.New().String(), OrganizationID: orgID, SKU: "OIL-001", Name: "Engine Oil",
			Description: "5W-30 synthetic oil", Category: "Fluids", Brand: "Mobil",
			CostPrice: 25.00, SellingPrice: 45.00, QuantityOnHand: 50, ReorderLevel: 15,
		},
	}
	return s.db.Create(&products).Error
}

func (s *Service) GetProducts(orgID string) ([]Product, error) {
	var products []Product
	err := s.db.Where("organization_id = ?", orgID).Order("name").Find(&products).Error
	return products, err
}

func (s *Service) CreateProduct(product *Product) error {
	product.ID = uuid.New().String()
	product.CreatedAt = time.Now()
	product.UpdatedAt = time.Now()
	return s.db.Create(product).Error
}

func (s *Service) GetProduct(id string) (*Product, error) {
	var product Product
	err := s.db.First(&product, "id = ?", id).Error
	return &product, err
}

func (s *Service) UpdateProduct(product *Product) error {
	product.UpdatedAt = time.Now()
	return s.db.Save(product).Error
}

func (s *Service) DeleteProduct(id string) error {
	return s.db.Delete(&Product{}, "id = ?", id).Error
}

func (s *Service) GetDashboard(orgID string) (*Dashboard, error) {
	var dashboard Dashboard

	// Get products count
	s.db.Model(&Product{}).Where("organization_id = ? AND is_active = ?", orgID, true).Count(&dashboard.ProductsCount)

	// Get low stock count
	s.db.Model(&Product{}).Where("organization_id = ? AND is_active = ? AND quantity_on_hand <= reorder_level", orgID, true).Count(&dashboard.LowStockCount)

	// Calculate total inventory value
	var totalValue float64
	s.db.Model(&Product{}).Where("organization_id = ? AND is_active = ?", orgID, true).
		Select("COALESCE(SUM(quantity_on_hand * cost_price), 0)").Row().Scan(&totalValue)
	dashboard.TotalInventoryValue = totalValue

	return &dashboard, nil
}