package integrations

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/internal/models"
	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/encryption"
	"github.com/stripe/stripe-go/v79"
	"github.com/stripe/stripe-go/v79/client"
	"github.com/stripe/stripe-go/v79/setupintent"
	"gorm.io/gorm"
)

// StripeService handles Stripe payment integration
type StripeService struct {
	db      *gorm.DB
	encrypt *encryption.EncryptionService
}

// NewStripeService creates a new Stripe service
func NewStripeService(db *gorm.DB) *StripeService {
	return &StripeService{
		db:      db,
		encrypt: encryption.GlobalEncryption,
	}
}

// TestConnection tests the Stripe API connection
func (s *StripeService) TestConnection(settings *models.StripeSettings) error {
	// Decrypt secret key
	secretKey, err := s.encrypt.Decrypt(settings.SecretKey)
	if err != nil {
		return fmt.Errorf("failed to decrypt secret key: %w", err)
	}

	// Create Stripe client
	sc := &client.API{}
	sc.Init(secretKey, nil)

	// Test by creating a simple test setup intent
	stripe.Key = secretKey
	params := &stripe.SetupIntentParams{
		Usage: stripe.String("off_session"),
	}
	_, err := setupintent.New(params)
	if err != nil {
		return fmt.Errorf("stripe connection test failed: %w", err)
	}

	// Update connection status
	settings.ConnectionVerified = true
	now := time.Now()
	settings.LastTestedAt = &now

	return nil
}

// CreatePaymentIntent creates a payment intent for a booking
func (s *StripeService) CreatePaymentIntent(settings *models.StripeSettings, amount int64, currency string, customerEmail string, description string) (*stripe.PaymentIntent, error) {
	// Decrypt secret key
	secretKey, err := s.encrypt.Decrypt(settings.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt secret key: %w", err)
	}

	// Set Stripe API key
	stripe.Key = secretKey

	// Parse payment methods
	var paymentMethods []string
	if settings.PaymentMethods != "" {
		if err := json.Unmarshal([]byte(settings.PaymentMethods), &paymentMethods); err != nil {
			paymentMethods = []string{"card"} // default
		}
	} else {
		paymentMethods = []string{"card"}
	}

	// Create payment intent parameters
	params := &stripe.PaymentIntentParams{
		Amount:                    stripe.Int64(amount),
		Currency:                  stripe.String(currency),
		PaymentMethodTypes:        stripe.StringSlice(paymentMethods),
		CaptureMethod:             stripe.String(settings.CaptureMethod),
		Description:               stripe.String(description),
		ReceiptEmail:              stripe.String(customerEmail),
		SetupFutureUsage:          stripe.String("off_session"), // For future payments
	}

	// Create the payment intent
	pi, err := stripe.PaymentIntents.New(params)
	if err != nil {
		return nil, fmt.Errorf("failed to create payment intent: %w", err)
	}

	return pi, nil
}

// CreateCustomer creates a Stripe customer
func (s *StripeService) CreateCustomer(settings *models.StripeSettings, customerData map[string]interface{}) (*stripe.Customer, error) {
	// Decrypt secret key
	secretKey, err := s.encrypt.Decrypt(settings.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt secret key: %w", err)
	}

	// Set Stripe API key
	stripe.Key = secretKey

	// Create customer parameters
	params := &stripe.CustomerParams{}

	if email, ok := customerData["email"].(string); ok {
		params.Email = stripe.String(email)
	}
	if name, ok := customerData["name"].(string); ok {
		params.Name = stripe.String(name)
	}
	if phone, ok := customerData["phone"].(string); ok {
		params.Phone = stripe.String(phone)
	}

	// Add address if provided
	if address, ok := customerData["address"].(map[string]interface{}); ok {
		stripeAddress := &stripe.AddressParams{}
		if line1, ok := address["line1"].(string); ok {
			stripeAddress.Line1 = stripe.String(line1)
		}
		if line2, ok := address["line2"].(string); ok {
			stripeAddress.Line2 = stripe.String(line2)
		}
		if city, ok := address["city"].(string); ok {
			stripeAddress.City = stripe.String(city)
		}
		if state, ok := address["state"].(string); ok {
			stripeAddress.State = stripe.String(state)
		}
		if postalCode, ok := address["postal_code"].(string); ok {
			stripeAddress.PostalCode = stripe.String(postalCode)
		}
		if country, ok := address["country"].(string); ok {
			stripeAddress.Country = stripe.String(country)
		}
		params.Address = stripeAddress
	}

	// Create the customer
	customer, err := stripe.Customers.New(params)
	if err != nil {
		return nil, fmt.Errorf("failed to create Stripe customer: %w", err)
	}

	return customer, nil
}

// ProcessPayment processes a payment and logs the transaction
func (s *StripeService) ProcessPayment(organizationID, customerID, bookingID string, paymentIntentID string) (*models.PaymentTransaction, error) {
	// Get integration settings
	var settings models.IntegrationSettings
	if err := s.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		return nil, fmt.Errorf("failed to get integration settings: %w", err)
	}

	// Decrypt secret key
	secretKey, err := s.encrypt.Decrypt(settings.StripeSettings.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt secret key: %w", err)
	}

	// Set Stripe API key
	stripe.Key = secretKey

	// Retrieve the payment intent
	pi, err := stripe.PaymentIntents.Get(paymentIntentID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve payment intent: %w", err)
	}

	// Create payment transaction record
	transaction := &models.PaymentTransaction{
		OrganizationID:        organizationID,
		CustomerID:            customerID,
		BookingID:             bookingID,
		Provider:              "stripe",
		ProviderTransactionID: pi.ID,
		Amount:                pi.Amount,
		Currency:              string(pi.Currency),
		Status:                string(pi.Status),
		Description:           pi.Description,
		PaymentMethod:         string(pi.PaymentMethod.Type),
	}

	// Add metadata
	if len(pi.Metadata) > 0 {
		metadata, _ := json.Marshal(pi.Metadata)
		transaction.Metadata = string(metadata)
	}

	// Set processed timestamp if successful
	if pi.Status == stripe.PaymentIntentStatusSucceeded {
		transaction.Status = "completed"
		now := time.Now()
		transaction.ProcessedAt = &now
	} else if pi.Status == stripe.PaymentIntentStatusCanceled ||
			  pi.Status == stripe.PaymentIntentStatusPaymentFailed {
		transaction.Status = "failed"
		if pi.LastPaymentError != nil {
			transaction.FailureReason = pi.LastPaymentError.Message
		}
	}

	// Save transaction to database
	if err := s.db.Create(transaction).Error; err != nil {
		return nil, fmt.Errorf("failed to save payment transaction: %w", err)
	}

	// Log the integration event
	s.LogIntegrationEvent(organizationID, "payment_processed", "success",
		fmt.Sprintf("Payment of %d %s processed successfully", pi.Amount, pi.Currency), "", "")

	return transaction, nil
}

// RefundPayment processes a refund
func (s *StripeService) RefundPayment(organizationID, transactionID string, amount int64, reason string) error {
	// Get the transaction
	var transaction models.PaymentTransaction
	if err := s.db.Where("id = ? AND organization_id = ?", transactionID, organizationID).First(&transaction).Error; err != nil {
		return fmt.Errorf("transaction not found: %w", err)
	}

	// Get integration settings
	var settings models.IntegrationSettings
	if err := s.db.Where("organization_id = ?", organizationID).First(&settings).Error; err != nil {
		return fmt.Errorf("failed to get integration settings: %w", err)
	}

	// Decrypt secret key
	secretKey, err := s.encrypt.Decrypt(settings.StripeSettings.SecretKey)
	if err != nil {
		return fmt.Errorf("failed to decrypt secret key: %w", err)
	}

	// Set Stripe API key
	stripe.Key = secretKey

	// Create refund parameters
	params := &stripe.RefundParams{
		PaymentIntent: stripe.String(transaction.ProviderTransactionID),
	}

	if amount > 0 {
		params.Amount = stripe.Int64(amount)
	}
	if reason != "" {
		params.Reason = stripe.String(reason)
	}

	// Process the refund
	refund, err := stripe.Refunds.New(params)
	if err != nil {
		s.LogIntegrationEvent(organizationID, "refund_failed", "error",
			fmt.Sprintf("Refund failed: %s", err.Error()), "", "")
		return fmt.Errorf("failed to process refund: %w", err)
	}

	// Update transaction record
	transaction.RefundAmount = refund.Amount
	transaction.RefundReason = reason
	now := time.Now()
	transaction.RefundedAt = &now

	if refund.Amount == transaction.Amount {
		transaction.Status = "refunded"
	} else {
		transaction.Status = "partially_refunded"
	}

	// Save updated transaction
	if err := s.db.Save(&transaction).Error; err != nil {
		return fmt.Errorf("failed to update transaction: %w", err)
	}

	// Log the integration event
	s.LogIntegrationEvent(organizationID, "refund_processed", "success",
		fmt.Sprintf("Refund of %d %s processed successfully", refund.Amount, refund.Currency), "", "")

	return nil
}

// LogIntegrationEvent logs integration events
func (s *StripeService) LogIntegrationEvent(organizationID, event, status, message, errorCode, userID string) {
	logEntry := &models.IntegrationLog{
		OrganizationID: organizationID,
		Integration:    "stripe",
		Event:          event,
		Status:         status,
		Message:        message,
		ErrorCode:      errorCode,
		UserID:         userID,
	}

	if err := s.db.Create(logEntry).Error; err != nil {
		log.Printf("Failed to log integration event: %v", err)
	}
}

// GetWebhookEvent processes Stripe webhook events
func (s *StripeService) GetWebhookEvent(payload []byte, sigHeader string, webhookSecret string) (*stripe.Event, error) {
	// Decrypt webhook secret
	secret, err := s.encrypt.Decrypt(webhookSecret)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt webhook secret: %w", err)
	}

	// Verify webhook signature and get event
	event, err := stripe.Webhooks.ConstructEvent(payload, sigHeader, secret)
	if err != nil {
		return nil, fmt.Errorf("webhook signature verification failed: %w", err)
	}

	return &event, nil
}

// HandleWebhookEvent handles Stripe webhook events
func (s *StripeService) HandleWebhookEvent(event *stripe.Event, organizationID string) error {
	switch event.Type {
	case "payment_intent.succeeded":
		return s.handlePaymentSucceeded(event, organizationID)
	case "payment_intent.payment_failed":
		return s.handlePaymentFailed(event, organizationID)
	case "charge.dispute.created":
		return s.handleChargeDispute(event, organizationID)
	default:
		log.Printf("Unhandled webhook event type: %s", event.Type)
	}

	return nil
}

func (s *StripeService) handlePaymentSucceeded(event *stripe.Event, organizationID string) error {
	var paymentIntent stripe.PaymentIntent
	if err := json.Unmarshal(event.Data.Raw, &paymentIntent); err != nil {
		return err
	}

	// Update transaction status
	if err := s.db.Model(&models.PaymentTransaction{}).
		Where("provider_transaction_id = ? AND organization_id = ?", paymentIntent.ID, organizationID).
		Updates(map[string]interface{}{
			"status":       "completed",
			"processed_at": time.Now(),
		}).Error; err != nil {
		return err
	}

	s.LogIntegrationEvent(organizationID, "webhook_payment_succeeded", "success",
		fmt.Sprintf("Payment succeeded webhook processed for %s", paymentIntent.ID), "", "")

	return nil
}

func (s *StripeService) handlePaymentFailed(event *stripe.Event, organizationID string) error {
	var paymentIntent stripe.PaymentIntent
	if err := json.Unmarshal(event.Data.Raw, &paymentIntent); err != nil {
		return err
	}

	failureReason := ""
	if paymentIntent.LastPaymentError != nil {
		failureReason = paymentIntent.LastPaymentError.Message
	}

	// Update transaction status
	if err := s.db.Model(&models.PaymentTransaction{}).
		Where("provider_transaction_id = ? AND organization_id = ?", paymentIntent.ID, organizationID).
		Updates(map[string]interface{}{
			"status":         "failed",
			"failure_reason": failureReason,
		}).Error; err != nil {
		return err
	}

	s.LogIntegrationEvent(organizationID, "webhook_payment_failed", "error",
		fmt.Sprintf("Payment failed webhook processed for %s: %s", paymentIntent.ID, failureReason), "", "")

	return nil
}

func (s *StripeService) handleChargeDispute(event *stripe.Event, organizationID string) error {
	var dispute stripe.Dispute
	if err := json.Unmarshal(event.Data.Raw, &dispute); err != nil {
		return err
	}

	s.LogIntegrationEvent(organizationID, "webhook_dispute_created", "warning",
		fmt.Sprintf("Dispute created for charge %s: %s", dispute.Charge.ID, dispute.Reason), "", "")

	return nil
}