package expenses

import (
	"bytes"
	"encoding/csv"
	"math"
	"strings"
	"time"
)

// CSVRow is one expense line read from a CSV export / statement.
type CSVRow struct {
	Line          int
	Date          *time.Time
	Description   string
	InvoiceNumber string
	Currency      string
	Total         float64 // positive = money out
	Tax           float64
	HasTax        bool
	Skip          string // why the row is not an expense (credit, top-up, blank)
}

func normHeader(h string) string {
	h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
	return strings.NewReplacer("_", " ", "-", " ", ".", "", "(", "", ")", "").Replace(h)
}

func pickCol(hdr []string, names ...string) int {
	for _, n := range names {
		for i, h := range hdr {
			if h == n {
				return i
			}
		}
	}
	for _, n := range names {
		for i, h := range hdr {
			if strings.Contains(h, n) {
				return i
			}
		}
	}
	return -1
}

// ParseCSV reads a CSV of transactions / invoices. Column names are matched
// loosely (Date, Description/Details/Item, Amount/Total/Debit/Credit,
// Invoice/Reference, Currency, GST/Tax). defaultCur applies when no currency
// column or symbol is present.
func ParseCSV(data []byte, defaultCur string) ([]CSVRow, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	sep := ','
	if first, _, _ := bytes.Cut(data, []byte("\n")); bytes.Count(first, []byte(";")) > bytes.Count(first, []byte(",")) {
		sep = ';'
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = sep
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	// Header = first row with a date-ish and an amount-ish column.
	hi := -1
	var hdr []string
	for i, rec := range recs {
		if i > 15 {
			break
		}
		h := make([]string, len(rec))
		for j, c := range rec {
			h[j] = normHeader(c)
		}
		if pickCol(h, "date", "transaction date", "invoice date") >= 0 && pickCol(h, "amount", "total", "debit", "charge", "cost", "price") >= 0 {
			hi, hdr = i, h
			break
		}
	}
	if hi < 0 {
		return nil, errNoHeader
	}
	cDate := pickCol(hdr, "invoice date", "transaction date", "date")
	cDesc := pickCol(hdr, "description", "details", "item", "product", "service", "memo", "narrative", "type")
	cInv := pickCol(hdr, "invoice number", "invoice no", "invoice #", "invoice", "reference", "ref", "transaction id", "id")
	cCur := pickCol(hdr, "currency")
	cTax := pickCol(hdr, "gst", "tax", "vat")
	cDebit := pickCol(hdr, "debit", "charge", "withdrawal", "money out")
	cCredit := pickCol(hdr, "credit", "deposit", "money in", "payment")
	cAmt := pickCol(hdr, "total", "amount", "cost", "price")
	if cTax >= 0 && cTax == cAmt {
		cTax = -1
	}
	out := []CSVRow{}
	get := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	for i := hi + 1; i < len(recs); i++ {
		rec := recs[i]
		row := CSVRow{Line: i + 1, Description: get(rec, cDesc), InvoiceNumber: get(rec, cInv)}
		if strings.Join(rec, "") == "" {
			continue
		}
		row.Date = ParseDate(get(rec, cDate))
		cur := strings.ToUpper(get(rec, cCur))
		var amt float64
		found := false
		if cDebit >= 0 && cDebit != cAmt {
			if v, c, ok := ParseMoney(get(rec, cDebit)); ok && v != 0 {
				amt, found = math.Abs(v), true
				if cur == "" {
					cur = c
				}
			}
			if !found && cCredit >= 0 {
				if v, _, ok := ParseMoney(get(rec, cCredit)); ok && v != 0 {
					row.Skip = "Credit / top-up (money in)"
					amt, found = -math.Abs(v), true
				}
			}
		}
		if !found && cAmt >= 0 {
			if v, c, ok := ParseMoney(get(rec, cAmt)); ok {
				amt, found = v, true
				if cur == "" {
					cur = c
				}
			}
		}
		if !found {
			continue
		}
		if cur == "" {
			cur = defaultCur
		}
		row.Currency = cur
		desc := strings.ToLower(row.Description)
		switch {
		case row.Skip != "":
		case amt < 0 && cDebit < 0:
			// Statements that sign money out as negative.
			amt = -amt
		case amt == 0:
			row.Skip = "Zero amount"
		}
		if row.Skip == "" && (strings.Contains(desc, "top up") || strings.Contains(desc, "topup") || strings.Contains(desc, "add funds") ||
			strings.Contains(desc, "account credit") || strings.Contains(desc, "payment received") || strings.Contains(desc, "deposit")) {
			row.Skip = "Account top-up / payment (not an expense)"
		}
		row.Total = math.Round(math.Abs(amt)*100) / 100
		if cTax >= 0 {
			if v, _, ok := ParseMoney(get(rec, cTax)); ok && math.Abs(v) < row.Total {
				row.Tax, row.HasTax = math.Round(math.Abs(v)*100)/100, true
			}
		}
		out = append(out, row)
	}
	return out, nil
}

type parseError string

func (e parseError) Error() string { return string(e) }

const errNoHeader = parseError("could not find a header row with Date and Amount columns")
