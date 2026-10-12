package barstock

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Spec is what the unit maths needs to know about a stock item.
type Spec struct {
	BaseUnit  string  // ml | unit
	UnitSize  float64 // base units in one stock unit (750 for a 750 ml bottle)
	UnitLabel string  // bottle, keg, can, piece …
	PackSize  int     // stock units in a purchase pack (12)
	PackLabel string  // case, crate …
	TotML     float64 // effective tot size (item override or org setting)
}

const eps = 1e-6

func round3(v float64) float64 {
	r := math.Round(v*1000) / 1000
	if r == 0 {
		return 0 // no negative zero
	}
	return r
}

// RoundMoney rounds an amount to the currency's minor units.
func RoundMoney(v float64, cur string) float64 {
	p := math.Pow10(currency.Decimals(cur))
	r := math.Round(v*p) / p
	if r == 0 {
		return 0
	}
	return r
}

// num formats a quantity without trailing zeros ("12", "12.5", "0.333").
func num(v float64) string { return strconv.FormatFloat(round3(v), 'f', -1, 64) }

func (s Spec) size() float64 {
	if s.UnitSize <= 0 {
		return 1
	}
	return s.UnitSize
}

func (s Spec) pack() int {
	if s.PackSize < 1 {
		return 1
	}
	return s.PackSize
}

func (s Spec) label() string {
	if strings.TrimSpace(s.UnitLabel) != "" {
		return strings.TrimSpace(s.UnitLabel)
	}
	if s.BaseUnit == BaseML {
		return "bottle"
	}
	return "unit"
}

func (s Spec) packLabel() string {
	if strings.TrimSpace(s.PackLabel) != "" {
		return strings.TrimSpace(s.PackLabel)
	}
	return "pack"
}

// ToBase converts a quantity entered in unit to base units:
//
//	"", base       base units as stored (ml or units)
//	ml             millilitres (ml items only)
//	tot            tots of TotML ml (ml items only)
//	bottle, stock  stock units (× unit size)
//	unit           a base unit for counted items; a stock unit for ml items
//	pack, case     purchase packs (× pack size × unit size)
func (s Spec) ToBase(qty float64, unit string) (float64, error) {
	if math.IsNaN(qty) || math.IsInf(qty, 0) {
		return 0, fmt.Errorf("invalid quantity")
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "base":
		return round3(qty), nil
	case "ml":
		if s.BaseUnit != BaseML {
			return 0, fmt.Errorf("this item is counted in units, not ml")
		}
		return round3(qty), nil
	case "tot", "tots":
		if s.BaseUnit != BaseML {
			return 0, fmt.Errorf("tots only apply to items measured in ml")
		}
		if s.TotML <= 0 {
			return 0, fmt.Errorf("set a tot size first")
		}
		return round3(qty * s.TotML), nil
	case "bottle", "bottles", "stock", "stock_unit":
		return round3(qty * s.size()), nil
	case "unit", "units":
		if s.BaseUnit == BaseML {
			return round3(qty * s.size()), nil
		}
		return round3(qty), nil
	case "pack", "packs", "case", "cases":
		return round3(qty * float64(s.pack()) * s.size()), nil
	}
	return 0, fmt.Errorf("unknown unit %q (use ml, tot, bottle, unit or pack)", unit)
}

// FromBase converts base units to the given unit (inverse of ToBase).
func (s Spec) FromBase(base float64, unit string) float64 {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "tot", "tots":
		if s.TotML > 0 {
			return round3(base / s.TotML)
		}
	case "bottle", "bottles", "stock", "stock_unit":
		return round3(base / s.size())
	case "unit", "units":
		if s.BaseUnit == BaseML {
			return round3(base / s.size())
		}
	case "pack", "packs", "case", "cases":
		return round3(base / s.size() / float64(s.pack()))
	}
	return round3(base)
}

// StockUnits is base ÷ unit size.
func (s Spec) StockUnits(base float64) float64 { return base / s.size() }

// Plural is a small English pluraliser for unit labels.
func Plural(word string, n float64) string {
	w := strings.TrimSpace(word)
	if w == "" || math.Abs(n-1) < eps {
		return w
	}
	lw := strings.ToLower(w)
	switch {
	case lw == "ml" || lw == "g" || lw == "kg" || strings.HasSuffix(lw, "s") && !strings.HasSuffix(lw, "ss"):
		return w
	case strings.HasSuffix(lw, "ss") || strings.HasSuffix(lw, "x") || strings.HasSuffix(lw, "ch") || strings.HasSuffix(lw, "sh"):
		return w + "es"
	case strings.HasSuffix(lw, "y") && len(lw) > 1 && !strings.ContainsRune("aeiou", rune(lw[len(lw)-2])):
		return w[:len(w)-1] + "ies"
	}
	return w + "s"
}

func countText(n float64, label string) string { return num(n) + " " + Plural(label, n) }

// Text shows a base quantity the way staff think about it:
// "3 bottles + 420 ml", "2 cases + 5 cans", "1 keg + 12500 ml", "0 bottles".
func (s Spec) Text(base float64) string {
	base = round3(base)
	sign := ""
	if base < 0 {
		sign, base = "-", -base
	}
	size := s.size()
	if s.BaseUnit == BaseML {
		whole := math.Floor(base/size + eps)
		rem := round3(base - whole*size)
		if rem < 0 {
			rem = 0
		}
		parts := []string{}
		if whole > 0 || rem == 0 {
			parts = append(parts, countText(whole, s.label()))
		}
		if rem > 0 {
			parts = append(parts, num(rem)+" ml")
		}
		return sign + strings.Join(parts, " + ")
	}
	// Counted items: stock units (and packs when the pack holds several).
	units := base
	baseRem := 0.0
	if size != 1 {
		units = math.Floor(base/size + eps)
		baseRem = round3(base - units*size)
	}
	parts := []string{}
	if p := s.pack(); p > 1 && units >= float64(p) {
		packs := math.Floor(units/float64(p) + eps)
		units = round3(units - packs*float64(p))
		parts = append(parts, countText(packs, s.packLabel()))
	}
	if units > 0 || (len(parts) == 0 && baseRem == 0) {
		parts = append(parts, countText(units, s.label()))
	}
	if baseRem > 0 {
		parts = append(parts, countText(baseRem, "unit"))
	}
	return sign + strings.Join(parts, " + ")
}

// QtyText formats a base quantity in the unit it was entered in, e.g. "2 tots", "6 bottles".
func (s Spec) QtyText(base float64, unit string) string {
	u := strings.ToLower(strings.TrimSpace(unit))
	v := s.FromBase(base, u)
	switch u {
	case "tot", "tots":
		return countText(v, "tot")
	case "bottle", "bottles", "stock", "stock_unit":
		return countText(v, s.label())
	case "unit", "units":
		if s.BaseUnit == BaseML {
			return countText(v, s.label())
		}
		return countText(v, s.label())
	case "pack", "packs", "case", "cases":
		return countText(v, s.packLabel())
	case "ml":
		return num(v) + " ml"
	}
	return s.Text(base)
}

// Servings is how many drinks a quantity is (tots for ml items, units otherwise).
func (s Spec) Servings(base float64) float64 {
	if s.BaseUnit == BaseML {
		if s.TotML > 0 {
			return round3(base / s.TotML)
		}
		return 0
	}
	return round3(base)
}

// Partial count modes.
const (
	PartialNone   = ""
	PartialTenths = "tenths"
	PartialML     = "ml"
	PartialWeight = "weight"
)

// CountedBase turns a count entry into base units: full stock units plus a
// partial unit entered as tenths (0.1 steps), ml or a weight in grams
// (ml = (weight − empty) ÷ (full − empty) × unit size).
func (s Spec) CountedBase(fullUnits float64, mode string, value float64, fullW, emptyW *float64) (float64, error) {
	if fullUnits < 0 || math.IsNaN(fullUnits) {
		return 0, fmt.Errorf("full units cannot be negative")
	}
	size := s.size()
	partial := 0.0
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "none" {
		mode = PartialNone
	}
	if mode != PartialNone && s.BaseUnit != BaseML {
		if value != 0 {
			return 0, fmt.Errorf("partial amounts only apply to items measured in ml")
		}
		mode = PartialNone
	}
	switch mode {
	case PartialNone:
		if value != 0 {
			return 0, fmt.Errorf("choose how the partial amount is measured")
		}
	case PartialTenths:
		if value < 0 || value > 1+eps {
			return 0, fmt.Errorf("partial tenths must be between 0 and 1 (e.g. 0.4)")
		}
		t := value * 10
		if math.Abs(t-math.Round(t)) > 1e-6 {
			return 0, fmt.Errorf("partial tenths go in steps of 0.1")
		}
		partial = math.Round(t) / 10 * size
	case PartialML:
		if value < 0 || value > size+eps {
			return 0, fmt.Errorf("partial ml must be between 0 and %s", num(size))
		}
		partial = value
	case PartialWeight:
		if fullW == nil || emptyW == nil || *fullW <= *emptyW {
			return 0, fmt.Errorf("set the item's full and empty weights to count by weight")
		}
		if value < *emptyW-eps || value > *fullW+eps {
			return 0, fmt.Errorf("weight must be between %s g (empty) and %s g (full)", num(*emptyW), num(*fullW))
		}
		partial = (value - *emptyW) / (*fullW - *emptyW) * size
	default:
		return 0, fmt.Errorf("unknown partial mode %q (use tenths, ml or weight)", mode)
	}
	return round3(fullUnits*size + partial), nil
}

// ParseHM parses "HH:MM" (00:00–23:59).
func ParseHM(s string) (int, int, bool) {
	s = strings.TrimSpace(s)
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// BusinessDate is the trading day t belongs to: clubs trade past midnight,
// so anything before the cutoff (local time) belongs to the previous night.
// 02:00 on Saturday with cutoff 06:00 → Friday.
func BusinessDate(t time.Time, loc *time.Location, cutoff string) string {
	if loc == nil {
		loc = time.UTC
	}
	lt := t.In(loc)
	h, m, ok := ParseHM(cutoff)
	if !ok {
		h, m = 6, 0
	}
	if lt.Hour()*60+lt.Minute() < h*60+m {
		lt = time.Date(lt.Year(), lt.Month(), lt.Day()-1, 12, 0, 0, 0, loc)
	}
	return lt.Format("2006-01-02")
}

// AddDays shifts a YYYY-MM-DD date.
func AddDays(day string, n int) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// ValidDay reports whether s is YYYY-MM-DD.
func ValidDay(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}
