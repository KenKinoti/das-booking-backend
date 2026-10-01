package invoicing

import (
	"math"
	"strconv"
	"strings"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// currencyWords are the English names of a currency's major and minor units
// (singular, plural). A currency without minor units has no minor names.
var currencyWords = map[string][4]string{
	"USD": {"US dollar", "US dollars", "cent", "cents"},
	"EUR": {"euro", "euros", "cent", "cents"},
	"GBP": {"pound sterling", "pounds sterling", "penny", "pence"},
	"JPY": {"Japanese yen", "Japanese yen", "", ""},
	"CHF": {"Swiss franc", "Swiss francs", "centime", "centimes"},
	"CAD": {"Canadian dollar", "Canadian dollars", "cent", "cents"},
	"AUD": {"Australian dollar", "Australian dollars", "cent", "cents"},
	"NZD": {"New Zealand dollar", "New Zealand dollars", "cent", "cents"},
	"CNY": {"Chinese yuan", "Chinese yuan", "fen", "fen"},
	"INR": {"Indian rupee", "Indian rupees", "paisa", "paise"},
	"SGD": {"Singapore dollar", "Singapore dollars", "cent", "cents"},
	"HKD": {"Hong Kong dollar", "Hong Kong dollars", "cent", "cents"},
	"AED": {"UAE dirham", "UAE dirhams", "fils", "fils"},
	"SAR": {"Saudi riyal", "Saudi riyals", "halala", "halalas"},
	"KWD": {"Kuwaiti dinar", "Kuwaiti dinars", "fils", "fils"},
	"ZAR": {"South African rand", "South African rand", "cent", "cents"},
	"NGN": {"Nigerian naira", "Nigerian naira", "kobo", "kobo"},
	"EGP": {"Egyptian pound", "Egyptian pounds", "piastre", "piastres"},
	"KES": {"Kenyan shilling", "Kenyan shillings", "cent", "cents"},
	"GHS": {"Ghanaian cedi", "Ghanaian cedis", "pesewa", "pesewas"},
	"MAD": {"Moroccan dirham", "Moroccan dirhams", "centime", "centimes"},
	"TZS": {"Tanzanian shilling", "Tanzanian shillings", "cent", "cents"},
	"UGX": {"Ugandan shilling", "Ugandan shillings", "", ""},
}

var (
	ones = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
		"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}
	tens   = []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
	scales = []string{"", "thousand", "million", "billion", "trillion"}
)

// below1000 spells 0 < n < 1000 ("one hundred and five").
func below1000(n int) string {
	var parts []string
	if n >= 100 {
		parts = append(parts, ones[n/100]+" hundred")
		n %= 100
		if n > 0 {
			parts = append(parts, "and")
		}
	}
	switch {
	case n >= 20:
		w := tens[n/10]
		if n%10 > 0 {
			w += "-" + ones[n%10]
		}
		parts = append(parts, w)
	case n > 0:
		parts = append(parts, ones[n])
	}
	return strings.Join(parts, " ")
}

// NumberInWords spells a whole number in British English, e.g. 7500 →
// "seven thousand five hundred", 1105 → "one thousand one hundred and five".
func NumberInWords(n int64) string {
	if n == 0 {
		return "zero"
	}
	if n < 0 {
		return "minus " + NumberInWords(-n)
	}
	var groups []string
	for i := 0; n > 0 && i < len(scales); i++ {
		g := int(n % 1000)
		n /= 1000
		if g == 0 {
			continue
		}
		w := below1000(g)
		if scales[i] != "" {
			w += " " + scales[i]
		} else if g < 100 && n > 0 {
			w = "and " + w // "one thousand and five"
		}
		groups = append([]string{w}, groups...)
	}
	return strings.Join(groups, " ")
}

// AmountInWords spells an amount with its currency, e.g. KES 7,500.50 →
// "Seven thousand five hundred Kenyan shillings and fifty cents only".
// The amount is rounded to the currency's decimals (JPY 0, KWD 3).
func AmountInWords(v float64, cur string) string {
	cur = strings.ToUpper(strings.TrimSpace(cur))
	d := currency.Decimals(cur)
	scale := math.Pow10(d)
	minorTotal := int64(math.Round(math.Abs(v) * scale))
	major := minorTotal / int64(scale)
	minor := minorTotal % int64(scale)
	names, ok := currencyWords[cur]
	if !ok {
		names = [4]string{cur, cur, "", ""}
	}
	unit := names[1]
	if major == 1 {
		unit = names[0]
	}
	out := NumberInWords(major) + " " + unit
	if minor > 0 {
		mu := names[3]
		if minor == 1 {
			mu = names[2]
		}
		if mu == "" {
			out += " and " + strconv.FormatInt(minor, 10) + "/" + strconv.FormatInt(int64(scale), 10)
		} else {
			out += " and " + NumberInWords(minor) + " " + mu
		}
	}
	out += " only"
	if v < 0 {
		out = "minus " + out
	}
	return strings.ToUpper(out[:1]) + out[1:]
}
