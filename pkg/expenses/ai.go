package expenses

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/assistant"
)

// AIEnabled reports whether AI extraction is configured (ANTHROPIC_API_KEY).
func AIEnabled() bool { return assistant.ConfigFromEnv().APIKey != "" }

const aiPrompt = `You read supplier invoices, receipts and billing emails for a small business and extract the expense.
Reply with ONLY a JSON object, no prose:
{"vendor": string, "invoice_number": string, "invoice_date": "YYYY-MM-DD" or "", "period_start": "YYYY-MM-DD" or "", "period_end": "YYYY-MM-DD" or "",
 "currency": ISO 4217 code or "", "total": number (amount charged including tax, 0 if unknown), "tax": number (GST/VAT included, 0 if none shown),
 "service": short description of what was bought, "category": one of "hosting","domains","email","software","other",
 "paid": true/false/null, "is_expense": true/false (false for statements, notices, marketing), "confidence": 0..1}
Use only values present in the document. Do not guess amounts.`

// AIExtract asks Claude to read the document. text may be empty when an image
// is given (mime image/png|jpeg|gif|webp) or a PDF (application/pdf).
func AIExtract(ctx context.Context, text string, file []byte, mime string) (*Extracted, string, error) {
	cfg := assistant.ConfigFromEnv()
	if cfg.APIKey == "" {
		return nil, "", errors.New("AI extraction is not configured")
	}
	content := []map[string]any{}
	switch {
	case len(file) > 0 && strings.HasPrefix(mime, "image/"):
		content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": base64.StdEncoding.EncodeToString(file)}})
	case len(file) > 0 && mime == "application/pdf" && strings.TrimSpace(text) == "":
		content = append(content, map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": base64.StdEncoding.EncodeToString(file)}})
	}
	if len(text) > 30000 {
		text = text[:30000]
	}
	if strings.TrimSpace(text) != "" {
		content = append(content, map[string]any{"type": "text", "text": "Document text:\n" + text})
	}
	if len(content) == 0 {
		return nil, "", errors.New("nothing to read")
	}
	content = append(content, map[string]any{"type": "text", "text": "Extract the expense as JSON."})
	body, _ := json.Marshal(map[string]any{
		"model": cfg.Model, "max_tokens": 800, "system": aiPrompt,
		"messages": []map[string]any{{"role": "user", "content": content}},
	})
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{Timeout: 70 * time.Second}).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("AI extraction failed (HTTP %d)", resp.StatusCode)
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, "", err
	}
	out := ""
	for _, c := range r.Content {
		if c.Type == "text" {
			out += c.Text
		}
	}
	js := regexp.MustCompile(`(?s)\{.*\}`).FindString(out)
	var a struct {
		Vendor        string   `json:"vendor"`
		InvoiceNumber string   `json:"invoice_number"`
		InvoiceDate   string   `json:"invoice_date"`
		PeriodStart   string   `json:"period_start"`
		PeriodEnd     string   `json:"period_end"`
		Currency      string   `json:"currency"`
		Total         float64  `json:"total"`
		Tax           float64  `json:"tax"`
		Service       string   `json:"service"`
		Category      string   `json:"category"`
		Paid          *bool    `json:"paid"`
		IsExpense     *bool    `json:"is_expense"`
		Confidence    *float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(js), &a); err != nil {
		return nil, "", errors.New("AI reply was not readable")
	}
	e := &Extracted{Kind: KindInvoice, Method: "ai", InvoiceNumber: strings.TrimSpace(a.InvoiceNumber), Currency: strings.ToUpper(strings.TrimSpace(a.Currency)),
		Service: strings.TrimSpace(a.Service), Paid: a.Paid}
	e.VendorKey = VendorFor(a.Vendor, "", a.Vendor)
	e.Date = ParseDate(a.InvoiceDate)
	e.PeriodStart, e.PeriodEnd = ParseDate(a.PeriodStart), ParseDate(a.PeriodEnd)
	if a.Total > 0 {
		e.Total, e.HasTotal = math.Round(a.Total*100)/100, true
	}
	if a.Tax > 0 && a.Tax < a.Total {
		e.Tax, e.HasTax, e.TaxNote = math.Round(a.Tax*100)/100, true, "Tax (AI-extracted)"
	}
	if a.IsExpense != nil && !*a.IsExpense {
		e.Ignore = "AI: not an expense document"
	}
	e.Confidence = 0.6
	if a.Confidence != nil {
		e.Confidence = math.Max(0, math.Min(0.85, *a.Confidence)) // never fully trusted
	}
	e.Warnings = append(e.Warnings, "AI-extracted, please confirm")
	cat := strings.ToLower(strings.TrimSpace(a.Category))
	if _, ok := CategoryLabels[cat]; !ok {
		cat = ""
	}
	return e, cat + "|" + strings.TrimSpace(a.Vendor), nil
}
