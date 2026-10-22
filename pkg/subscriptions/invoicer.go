package subscriptions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Invoices are created, emailed and voided through the invoicing API itself
// (in-process, as an administrator of the organisation), so numbering,
// currency and exchange-rate locking, the pre-send checks, the branded email
// with the PDF and the Flutterwave pay link, the email log and the activity
// history are exactly what the web app does. pkg/invoicing is not changed.

// Invoicer is the invoicing API.
type Invoicer interface {
	Create(ctx context.Context, who Actor, body map[string]any) (*InvoiceRef, error)
	Send(ctx context.Context, who Actor, invoiceID string, body map[string]any) error
	Void(ctx context.Context, who Actor, invoiceID string) error
	// Pay records a payment (POST /invoices/:id/payments).
	Pay(ctx context.Context, who Actor, invoiceID string, body map[string]any) error
}

// Actor is who an API call runs as.
type Actor struct {
	Org, User, Role, Email string
}

// InvoiceRef is what the subscription keeps of an invoice.
type InvoiceRef struct {
	ID          string  `json:"id"`
	Number      string  `json:"number"`
	Status      string  `json:"status"`
	PublicToken string  `json:"public_token"`
	Currency    string  `json:"currency"`
	Total       float64 `json:"total"`
	BalanceDue  float64 `json:"balance_due"`
}

// APIError is a non-2xx answer from the invoicing API.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

// APIInvoicer calls the API in-process (Engine is the gin engine, set once
// every route is registered).
type APIInvoicer struct {
	Engine func() http.Handler
	Secret []byte
	// Prefix is the invoicing API's path (default /api/v1/invoicing).
	Prefix string
}

func (a *APIInvoicer) token(who Actor) (string, error) {
	if len(a.Secret) == 0 {
		return "", errors.New("subscriptions: no JWT secret")
	}
	now := time.Now()
	c := jwt.MapClaims{"user_id": who.User, "org_id": who.Org, "role": who.Role, "email": who.Email, "iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(), "src": "subscriptions"}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(a.Secret)
}

func (a *APIInvoicer) call(ctx context.Context, who Actor, method, path string, body any) (map[string]any, error) {
	if a.Engine == nil || a.Engine() == nil {
		return nil, errors.New("subscriptions: the API is not ready yet")
	}
	tok, err := a.token(who)
	if err != nil {
		return nil, err
	}
	var buf []byte
	if body != nil {
		if buf, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	prefix := a.Prefix
	if prefix == "" {
		prefix = "/api/v1/invoicing"
	}
	req := httptest.NewRequest(method, prefix+path, bytes.NewReader(buf))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-By", "dasyin-subscriptions")
	rec := httptest.NewRecorder()
	a.Engine().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code >= 300 {
		e := &APIError{Status: rec.Code, Message: http.StatusText(rec.Code)}
		if m, ok := out["error"].(map[string]any); ok {
			e.Code, _ = m["code"].(string)
			if s, ok := m["message"].(string); ok && s != "" {
				e.Message = s
			}
		} else if s, ok := out["error"].(string); ok && s != "" {
			e.Message = s
		}
		if s, ok := out["message"].(string); ok && s != "" && e.Message == http.StatusText(rec.Code) {
			e.Message = s
		}
		return nil, e
	}
	if d, ok := out["data"].(map[string]any); ok {
		return d, nil
	}
	return out, nil
}

func refOf(m map[string]any) *InvoiceRef {
	if m == nil {
		return nil
	}
	if inv, ok := m["invoice"].(map[string]any); ok {
		m = inv
	}
	r := &InvoiceRef{}
	r.ID, _ = m["id"].(string)
	r.Number, _ = m["number"].(string)
	r.Status, _ = m["status"].(string)
	r.PublicToken, _ = m["public_token"].(string)
	r.Currency, _ = m["currency"].(string)
	r.Total, _ = m["total"].(float64)
	r.BalanceDue, _ = m["balance_due"].(float64)
	return r
}

// Create POSTs /invoices.
func (a *APIInvoicer) Create(ctx context.Context, who Actor, body map[string]any) (*InvoiceRef, error) {
	m, err := a.call(ctx, who, http.MethodPost, "/invoices", body)
	if err != nil {
		return nil, err
	}
	r := refOf(m)
	if r == nil || r.ID == "" {
		return nil, fmt.Errorf("the invoicing API returned no invoice")
	}
	return r, nil
}

// Send POSTs /invoices/:id/send (mode email: fails clearly without outgoing email).
func (a *APIInvoicer) Send(ctx context.Context, who Actor, id string, body map[string]any) error {
	if body == nil {
		body = map[string]any{}
	}
	if _, ok := body["mode"]; !ok {
		body["mode"] = "email"
	}
	_, err := a.call(ctx, who, http.MethodPost, "/invoices/"+id+"/send", body)
	return err
}

// Void POSTs /invoices/:id/void.
func (a *APIInvoicer) Void(ctx context.Context, who Actor, id string) error {
	_, err := a.call(ctx, who, http.MethodPost, "/invoices/"+id+"/void", nil)
	return err
}

// Pay POSTs /invoices/:id/payments.
func (a *APIInvoicer) Pay(ctx context.Context, who Actor, id string, body map[string]any) error {
	_, err := a.call(ctx, who, http.MethodPost, "/invoices/"+id+"/payments", body)
	return err
}

// ---------------------------------------------------------------- actor

// adminRoles are the roles whose users manage subscriptions and get alerts.
var adminRoles = []string{"owner", "admin", "org_admin", "super_admin"}

func isAdminRole(r string) bool {
	for _, x := range adminRoles {
		if strings.EqualFold(x, r) {
			return true
		}
	}
	return false
}

// actor is the organisation administrator the engine acts as: the person
// who created the subscription while they are an active administrator,
// else the organisation's first administrator.
func (s *Service) actor(org, prefer string) (Actor, error) {
	type row struct{ ID, Role, Email string }
	var rows []row
	s.DB.Raw(`SELECT id, role, COALESCE(email,'') AS email FROM users WHERE organization_id = ? AND deleted_at IS NULL AND (is_active IS NULL OR is_active)
		AND role IN ? ORDER BY CASE WHEN id = ? THEN 0 ELSE 1 END, CASE role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 WHEN 'org_admin' THEN 2 ELSE 3 END, created_at`,
		org, adminRoles, prefer).Scan(&rows)
	if len(rows) == 0 {
		return Actor{}, errors.New("the organisation has no active administrator to send invoices as")
	}
	return Actor{Org: org, User: rows[0].ID, Role: rows[0].Role, Email: rows[0].Email}, nil
}

// userActor is the signed-in user (with their role) for actions they take;
// the organisation's administrator when the user can't be found.
func (s *Service) userActor(org, user, role string) Actor {
	var rows []struct{ ID, Role, Email string }
	if user != "" {
		s.DB.Raw(`SELECT id, role, COALESCE(email,'') AS email FROM users WHERE organization_id = ? AND id = ? AND deleted_at IS NULL`, org, user).Scan(&rows)
	}
	if len(rows) == 1 {
		r := rows[0]
		return Actor{Org: org, User: r.ID, Role: firstNonEmpty(role, r.Role), Email: r.Email}
	}
	a, err := s.actor(org, user)
	if err != nil {
		return Actor{Org: org, User: user, Role: firstNonEmpty(role, "admin")}
	}
	return a
}
