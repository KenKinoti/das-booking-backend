// Package payments takes invoice payments online through Flutterwave
// (hosted checkout): per-organisation settings with encrypted keys, a fee
// table used to recommend the cheapest payment method, the public pay page
// API, verification of every transaction with Flutterwave before it is
// recorded (exactly once) and the webhook.
package payments

import (
	"math"
	"sort"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Method is a Flutterwave checkout payment option.
type Method struct {
	Key        string   `json:"key"`        // the payment_options value
	Label      string   `json:"label"`      // for people
	Short      string   `json:"short"`      // stored on the invoice payment: "Flutterwave · <Short>"
	Icon       string   `json:"icon"`       // Font Awesome class
	Currencies []string `json:"currencies"` // empty = every currency
	Order      int      `json:"-"`          // tie-break: mobile money / bank before cards
}

// Methods are the checkout options the app offers (Flutterwave Standard,
// developer.flutterwave.com/v3.0/docs/payment-methods).
var Methods = []Method{
	{Key: "mpesa", Label: "M-Pesa", Short: "M-Pesa", Icon: "fa-solid fa-mobile-screen-button", Currencies: []string{"KES"}, Order: 1},
	{Key: "mobilemoneyghana", Label: "Mobile money (Ghana)", Short: "Mobile money", Icon: "fa-solid fa-mobile-screen-button", Currencies: []string{"GHS"}, Order: 1},
	{Key: "mobilemoneyuganda", Label: "Mobile money (Uganda)", Short: "Mobile money", Icon: "fa-solid fa-mobile-screen-button", Currencies: []string{"UGX"}, Order: 1},
	{Key: "mobilemoneytanzania", Label: "Mobile money (Tanzania)", Short: "Mobile money", Icon: "fa-solid fa-mobile-screen-button", Currencies: []string{"TZS"}, Order: 1},
	{Key: "mobilemoneyrwanda", Label: "Mobile money (Rwanda)", Short: "Mobile money", Icon: "fa-solid fa-mobile-screen-button", Currencies: []string{"RWF"}, Order: 1},
	{Key: "mobilemoneyzambia", Label: "Mobile money (Zambia)", Short: "Mobile money", Icon: "fa-solid fa-mobile-screen-button", Currencies: []string{"ZMW"}, Order: 1},
	{Key: "banktransfer", Label: "Bank transfer", Short: "Bank transfer", Icon: "fa-solid fa-building-columns", Currencies: []string{"NGN", "GHS"}, Order: 2},
	{Key: "ussd", Label: "USSD", Short: "USSD", Icon: "fa-solid fa-hashtag", Currencies: []string{"NGN"}, Order: 3},
	{Key: "card", Label: "Card (Visa, Mastercard)", Short: "Card", Icon: "fa-regular fa-credit-card", Order: 5},
}

// MethodByKey returns the method with key.
func MethodByKey(key string) (Method, bool) {
	for _, m := range Methods {
		if m.Key == key {
			return m, true
		}
	}
	return Method{}, false
}

// Supports reports whether m can be used for cur.
func (m Method) Supports(cur string) bool {
	if len(m.Currencies) == 0 {
		return true
	}
	for _, c := range m.Currencies {
		if c == cur {
			return true
		}
	}
	return false
}

// Card fee scopes.
const (
	ScopeAny           = ""
	ScopeLocal         = "local"
	ScopeInternational = "international"
)

// FeeRule is one row of the fee table: what Flutterwave charges for a
// method in a currency. Percent is 0–100; Fixed and Cap are in the currency
// (Cap 0 = no cap). Currency "*" matches every currency.
type FeeRule struct {
	Method   string  `json:"method"`
	Currency string  `json:"currency"`
	Scope    string  `json:"scope"` // cards: local | international; "" for everything else
	Percent  float64 `json:"percent"`
	Fixed    float64 `json:"fixed"`
	Cap      float64 `json:"cap"`
	Note     string  `json:"note,omitempty"`
}

// PricingSource and PricingCheckedOn document where the default fees come
// from. Every value is an editable default: businesses must check their own
// Flutterwave dashboard (negotiated rates, VAT, new prices).
const (
	PricingSource    = "https://flutterwave.com/ke/pricing · https://flutterwave.com/ng/pricing · https://flutterwave.com/gh/pricing · https://flutterwave.com/ng/support/pricing/pricing-for-receiving-payment"
	PricingCheckedOn = "2026-09-30"
)

// DefaultFees is the published Flutterwave pricing for collections, read
// on PricingCheckedOn from PricingSource. Taxes on fees (e.g. Nigeria's 7.5%
// VAT) are not included — set "Tax on fees" in the settings.
func DefaultFees() []FeeRule {
	return []FeeRule{
		// Kenya
		{Method: "mpesa", Currency: "KES", Percent: 2.9, Note: "Kenya wallets & mobile money"},
		{Method: "card", Currency: "KES", Scope: ScopeLocal, Percent: 3.2, Note: "Kenya local cards"},
		// Nigeria: 2% on every local method (1.4% transaction + 0.6% platform), + 7.5% VAT
		{Method: "banktransfer", Currency: "NGN", Percent: 2.0, Note: "Nigeria local, excl. 7.5% VAT"},
		{Method: "ussd", Currency: "NGN", Percent: 2.0, Note: "Nigeria local, excl. 7.5% VAT"},
		{Method: "card", Currency: "NGN", Scope: ScopeLocal, Percent: 2.0, Note: "Nigeria local cards, excl. 7.5% VAT"},
		// Ghana
		{Method: "mobilemoneyghana", Currency: "GHS", Percent: 2.0, Note: "Ghana mobile money"},
		{Method: "banktransfer", Currency: "GHS", Percent: 2.0, Note: "Ghana bank transfer"},
		{Method: "card", Currency: "GHS", Scope: ScopeLocal, Percent: 2.6, Note: "Ghana local cards"},
		// East Africa
		{Method: "mobilemoneyuganda", Currency: "UGX", Percent: 3.0, Note: "Uganda mobile money"},
		{Method: "card", Currency: "UGX", Scope: ScopeLocal, Percent: 4.8, Note: "Uganda local cards"},
		{Method: "mobilemoneytanzania", Currency: "TZS", Percent: 2.5, Note: "Tanzania mobile money"},
		{Method: "card", Currency: "TZS", Scope: ScopeLocal, Percent: 4.8, Note: "Tanzania local cards"},
		{Method: "mobilemoneyrwanda", Currency: "RWF", Percent: 3.5, Note: "Rwanda mobile money"},
		{Method: "card", Currency: "RWF", Scope: ScopeLocal, Percent: 4.8, Note: "Rwanda local cards"},
		{Method: "mobilemoneyzambia", Currency: "ZMW", Percent: 3.0, Note: "Zambia mobile money"},
		// South Africa
		{Method: "card", Currency: "ZAR", Scope: ScopeLocal, Percent: 2.9, Fixed: 1, Note: "South Africa local cards (2.9% + R1)"},
		// International cards (and every other currency)
		{Method: "card", Currency: "*", Scope: ScopeInternational, Percent: 4.8, Note: "International cards"},
	}
}

// homeCountry is the country whose "local" card pricing a currency gets.
var homeCountry = map[string]string{
	"KES": "KE", "NGN": "NG", "GHS": "GH", "UGX": "UG", "TZS": "TZ", "RWF": "RW", "ZMW": "ZM", "ZAR": "ZA",
	"XOF": "SN", "XAF": "CM", "MWK": "MW", "EGP": "EG",
}

// CardScope decides local vs international card pricing: a card payment is
// local when the currency is the business's own and the customer (when
// known) is in the currency's country. Anything else is international
// (Flutterwave treats a currency other than the merchant's local currency as
// an international payment).
func CardScope(cur, customerCountry, orgCountry, orgCurrency string) string {
	cur = strings.ToUpper(cur)
	home, ok := homeCountry[cur]
	if !ok {
		return ScopeInternational
	}
	if orgCurrency != "" && !strings.EqualFold(orgCurrency, cur) {
		return ScopeInternational
	}
	if orgCountry != "" && !strings.EqualFold(orgCountry, home) {
		return ScopeInternational
	}
	if customerCountry != "" && !strings.EqualFold(customerCountry, home) {
		return ScopeInternational
	}
	return ScopeLocal
}

// FindRule returns the fee rule for a method, currency and card scope:
// an exact currency match beats "*"; for cards, a rule for the scope beats
// an unscoped one. A local card with no local rule uses the international
// one.
func FindRule(rules []FeeRule, method, cur, scope string) (FeeRule, bool) {
	best, bestScore := FeeRule{}, -1
	for _, r := range rules {
		if r.Method != method {
			continue
		}
		score := 0
		switch {
		case strings.EqualFold(r.Currency, cur):
			score += 4
		case r.Currency == "*":
		default:
			continue
		}
		switch {
		case r.Scope == scope && scope != "":
			score += 2
		case r.Scope == ScopeAny:
			score++
		case method == "card" && scope == ScopeLocal && r.Scope == ScopeInternational:
			// fallback only
		default:
			continue
		}
		if score > bestScore {
			best, bestScore = r, score
		}
	}
	return best, bestScore >= 0
}

func roundTo(v float64, cur string) float64 {
	p := math.Pow10(currency.Decimals(cur))
	return math.Round(v*p) / p
}

func ceilTo(v float64, cur string) float64 {
	p := math.Pow10(currency.Decimals(cur))
	return math.Ceil(v*p-1e-9) / p
}

// Fee is the processor fee for amount under rule, including tax on the fee
// (taxPct, e.g. 7.5 for Nigerian VAT), rounded to the currency.
func Fee(rule FeeRule, amount, taxPct float64, cur string) float64 {
	if amount <= 0 {
		return 0
	}
	f := amount*rule.Percent/100 + rule.Fixed
	if rule.Cap > 0 && f > rule.Cap {
		f = rule.Cap
	}
	return roundTo(f*(1+taxPct/100), cur)
}

// Surcharge is what to add to amount so that, after the fee on the total,
// the business still receives amount (the customer pays the fee).
func Surcharge(rule FeeRule, amount, taxPct float64, cur string) float64 {
	if amount <= 0 {
		return 0
	}
	t := 1 + taxPct/100
	p := rule.Percent / 100 * t
	if p >= 0.5 {
		return Fee(rule, amount, taxPct, cur)
	}
	gross := (amount + rule.Fixed*t) / (1 - p)
	s := ceilTo(gross-amount, cur)
	if rule.Cap > 0 && s > rule.Cap*t {
		s = ceilTo(rule.Cap*t, cur)
	}
	return s
}

// Option is one way to pay an amount, with its estimated fee.
type Option struct {
	Method      string  `json:"method"`
	Label       string  `json:"label"`
	Icon        string  `json:"icon"`
	Scope       string  `json:"scope,omitempty"`
	Percent     float64 `json:"percent"`
	Fixed       float64 `json:"fixed"`
	Cap         float64 `json:"cap"`
	Fee         float64 `json:"fee"`       // estimated processor fee
	Surcharge   float64 `json:"surcharge"` // added to the customer's total (fee passed on)
	Total       float64 `json:"total"`     // what the customer pays
	Known       bool    `json:"fee_known"` // false when no fee rule matched
	Recommended bool    `json:"recommended"`
	order       int
}

// Context is what least-cost routing needs to know.
type Context struct {
	Amount          float64
	Currency        string
	CustomerCountry string
	OrgCountry      string
	OrgCurrency     string
}

// Options lists the allowed methods that work for the currency, cheapest
// first (estimated fee), the cheapest marked Recommended.
func Options(s *Settings, ctx Context) []Option {
	cur := strings.ToUpper(ctx.Currency)
	rules := s.Fees
	if len(rules) == 0 {
		rules = DefaultFees()
	}
	allowed := map[string]bool{}
	for _, m := range s.Methods {
		allowed[m] = true
	}
	scope := CardScope(cur, ctx.CustomerCountry, ctx.OrgCountry, ctx.OrgCurrency)
	out := []Option{}
	for _, m := range Methods {
		if (len(allowed) > 0 && !allowed[m.Key]) || !m.Supports(cur) {
			continue
		}
		sc := ScopeAny
		if m.Key == "card" {
			sc = scope
		}
		o := Option{Method: m.Key, Label: m.Label, Icon: m.Icon, order: m.Order}
		if r, ok := FindRule(rules, m.Key, cur, sc); ok {
			o.Known = true
			o.Scope = r.Scope
			o.Percent, o.Fixed, o.Cap = r.Percent, r.Fixed, r.Cap
			o.Fee = Fee(r, ctx.Amount, s.FeeTaxPercent, cur)
			if s.FeeBearer == BearerCustomer {
				o.Surcharge = Surcharge(r, ctx.Amount, s.FeeTaxPercent, cur)
				o.Fee = Fee(r, ctx.Amount+o.Surcharge, s.FeeTaxPercent, cur)
			}
		}
		o.Total = roundTo(ctx.Amount+o.Surcharge, cur)
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Known != b.Known {
			return a.Known
		}
		if a.Fee != b.Fee {
			return a.Fee < b.Fee
		}
		return a.order < b.order
	})
	if len(out) > 0 && out[0].Known {
		out[0].Recommended = true
	}
	return out
}
