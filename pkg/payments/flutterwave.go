package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is Flutterwave's API. FLUTTERWAVE_BASE_URL overrides it
// (tests point it at a fake server).
const DefaultBaseURL = "https://api.flutterwave.com"

// BaseURL returns the Flutterwave API origin in use.
func BaseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("FLUTTERWAVE_BASE_URL")), "/"); v != "" {
		return v
	}
	return DefaultBaseURL
}

// Client calls the Flutterwave v3 API with a secret key.
type Client struct {
	BaseURL   string
	SecretKey string
	HTTP      *http.Client
}

// NewClient builds a client for the configured base URL.
func NewClient(secret string) *Client {
	return &Client{BaseURL: BaseURL(), SecretKey: secret, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// APIError is an error response from Flutterwave.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Flutterwave returned HTTP %d", e.Status)
	}
	return "Flutterwave: " + e.Message
}

type envelope struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	if c.SecretKey == "" {
		return errors.New("the Flutterwave secret key is not set")
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	base := c.BaseURL
	if base == "" {
		base = BaseURL()
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.SecretKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach Flutterwave: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	var env envelope
	_ = json.Unmarshal(raw, &env)
	if res.StatusCode >= 300 || (env.Status != "" && env.Status != "success") {
		msg := env.Message
		if res.StatusCode == http.StatusUnauthorized && msg == "" {
			msg = "invalid secret key"
		}
		return &APIError{Status: res.StatusCode, Message: msg}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("unexpected Flutterwave response: %w", err)
		}
	}
	return nil
}

// Customer is the payer.
type Customer struct {
	Email       string `json:"email"`
	Name        string `json:"name,omitempty"`
	PhoneNumber string `json:"phonenumber,omitempty"`
}

// Customizations brand the hosted checkout.
type Customizations struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Logo        string `json:"logo,omitempty"`
}

// PaymentRequest is POST /v3/payments (Flutterwave Standard).
type PaymentRequest struct {
	TxRef          string            `json:"tx_ref"`
	Amount         float64           `json:"amount"`
	Currency       string            `json:"currency"`
	RedirectURL    string            `json:"redirect_url"`
	PaymentOptions string            `json:"payment_options,omitempty"`
	Customer       Customer          `json:"customer"`
	Customizations Customizations    `json:"customizations"`
	Meta           map[string]string `json:"meta,omitempty"`
}

// CreatePayment starts a hosted checkout and returns its link.
func (c *Client) CreatePayment(ctx context.Context, r PaymentRequest) (string, error) {
	var d struct {
		Link string `json:"link"`
	}
	if err := c.do(ctx, http.MethodPost, "/v3/payments", r, &d); err != nil {
		return "", err
	}
	if d.Link == "" {
		return "", errors.New("Flutterwave did not return a checkout link")
	}
	return d.Link, nil
}

// flexString accepts a JSON string or number.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexString(v)
		return nil
	}
	*f = flexString(s)
	return nil
}

// Transaction is a verified Flutterwave transaction.
type FlwTransaction struct {
	ID            flexString `json:"id"`
	TxRef         string     `json:"tx_ref"`
	FlwRef        string     `json:"flw_ref"`
	Amount        float64    `json:"amount"`
	ChargedAmount float64    `json:"charged_amount"`
	AppFee        float64    `json:"app_fee"`
	MerchantFee   float64    `json:"merchant_fee"`
	Currency      string     `json:"currency"`
	Status        string     `json:"status"`
	PaymentType   string     `json:"payment_type"`
	Customer      struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"customer"`
}

// Verify is GET /v3/transactions/:id/verify.
func (c *Client) Verify(ctx context.Context, id string) (*FlwTransaction, error) {
	var t FlwTransaction
	if err := c.do(ctx, http.MethodGet, "/v3/transactions/"+url.PathEscape(id)+"/verify", nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// VerifyByRef is GET /v3/transactions/verify_by_reference?tx_ref=.
func (c *Client) VerifyByRef(ctx context.Context, txRef string) (*FlwTransaction, error) {
	var t FlwTransaction
	if err := c.do(ctx, http.MethodGet, "/v3/transactions/verify_by_reference?tx_ref="+url.QueryEscape(txRef), nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Balance is one wallet balance.
type Balance struct {
	Currency         string  `json:"currency"`
	AvailableBalance float64 `json:"available_balance"`
}

// Balances is GET /v3/balances — a harmless authenticated call used by
// "Test connection".
func (c *Client) Balances(ctx context.Context) ([]Balance, error) {
	var b []Balance
	if err := c.do(ctx, http.MethodGet, "/v3/balances", nil, &b); err != nil {
		return nil, err
	}
	return b, nil
}
