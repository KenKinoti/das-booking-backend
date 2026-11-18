package scout

import (
	"fmt"
	"strings"
	"time"

	"github.com/kenkinoti/gofiber-das-crm-backend/pkg/currency"
)

// Fit score (0–100): how well an opportunity fits the profile and focus.
// It is computed here, from fixed rules, so it is the same for every model
// and can be explained point by point:
//
//	type wanted            18   (no type preference: 10)
//	country                20 target country · 12 a country your customers are in · 12 a region you named
//	matches                up to 22: focus keywords 6 each, sectors 5 each, what you sell 4 each
//	value                  10 above your minimum · 8 stated (no minimum) · 4 not stated
//	time to prepare        10 ≥ 14 days · 6 5–13 days · 2 closing within 4 days · 6 no fixed deadline
//	confidence             8 high · 5 medium · 2 low · 3 not given
//	effort                 8 low · 5 medium · 2 high · 4 not given
//	favoured source        4
//	not interested         −35 like one you dismissed · −15 you dismissed several of this type there

// scorer holds what scoring needs, prepared once per run.
type scorer struct {
	types       map[string]bool
	countries   map[string]bool
	custCountry map[string]bool
	regions     []string
	keywords    []string
	sectors     []string
	offerings   []string
	favoured    []string
	dismissed   []Opportunity
	today       time.Time
	minHome     float64 // 0 = no minimum (or no rate)
}

func newScorer(st Settings, dismissed []Opportunity, today time.Time, minHome float64) *scorer {
	sc := &scorer{types: map[string]bool{}, countries: map[string]bool{}, custCountry: map[string]bool{}, dismissed: dismissed,
		today: time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC), minHome: minHome}
	for _, t := range st.Focus.Types {
		sc.types[t] = true
	}
	for _, c := range st.Focus.Countries {
		sc.countries[c] = true
	}
	for _, c := range st.Profile.Countries {
		sc.custCountry[c.Key] = true
	}
	low := func(in []string) []string {
		out := []string{}
		for _, s := range in {
			if s = strings.ToLower(strings.TrimSpace(s)); len(s) >= 3 {
				out = append(out, s)
			}
		}
		return out
	}
	sc.regions = low(st.Focus.Regions)
	sc.keywords = low(st.Focus.Keywords)
	sc.sectors = sectorWords(st.Focus.Sectors)
	offs := []string{}
	seen := map[string]bool{}
	for _, o := range st.Profile.Offerings {
		for _, w := range []string{o.Name, o.Line} {
			w = strings.ToLower(strings.TrimSpace(w))
			if len(w) >= 4 && !seen[w] {
				seen[w] = true
				offs = append(offs, w)
			}
		}
	}
	sc.offerings = offs
	for _, s := range st.Focus.Sources {
		sc.favoured = append(sc.favoured, s.Domain)
	}
	return sc
}

// sectorWords turns "NGOs, CBOs & community groups" into matchable words
// ("ngo", "cbo", "community group").
func sectorWords(sectors []string) []string {
	out := []string{}
	for _, s := range sectors {
		for _, p := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == '&' || r == '/' || r == ';' }) {
			p = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), "and "))
			p = strings.TrimSuffix(p, "s")
			if len(p) >= 3 {
				out = append(out, p)
			}
		}
	}
	return out
}

func hits(text string, words []string) []string {
	out := []string{}
	for _, w := range words {
		if (len(w) <= 4 && containsWord(text, w)) || (len(w) > 4 && strings.Contains(text, w)) {
			out = append(out, w)
		}
	}
	return out
}

// score fills Fit and FitReasons.
func (sc *scorer) score(o *Opportunity) {
	rs := []Reason{}
	add := func(p int, label string) {
		if p != 0 {
			rs = append(rs, Reason{Label: label, Points: p})
		}
	}
	if len(sc.types) == 0 {
		add(10, TypeLabel(o.Type))
	} else if sc.types[o.Type] {
		add(18, TypeLabel(o.Type)+" — a type you want")
	}
	text := " " + strings.ToLower(strings.Join([]string{o.Title, o.Summary, o.WhyItFits, o.Buyer, o.Location}, " ")) + " "
	switch {
	case o.Country != "" && sc.countries[o.Country]:
		add(20, currency.CountryName(o.Country)+" — a target country")
	case o.Country != "" && sc.custCountry[o.Country]:
		add(12, currency.CountryName(o.Country)+" — where your customers are")
	default:
		if h := hits(text, sc.regions); len(h) > 0 {
			add(12, "In a region you named: "+h[0])
		}
	}
	m := 0
	labels := []string{}
	for _, h := range hits(text, sc.keywords) {
		m += 6
		labels = append(labels, h)
	}
	for _, h := range hits(text, sc.sectors) {
		m += 5
		labels = append(labels, h)
	}
	for _, h := range hits(text, sc.offerings) {
		m += 4
		labels = append(labels, h)
	}
	if m > 22 {
		m = 22
	}
	if m > 0 {
		if len(labels) > 4 {
			labels = labels[:4]
		}
		add(m, "Matches "+strings.Join(labels, ", "))
	}
	switch {
	case o.ValueHome != nil && sc.minHome > 0 && *o.ValueHome >= sc.minHome:
		add(10, "Above your minimum value")
	case o.EstValue != nil && sc.minHome == 0:
		add(8, "Value stated")
	case o.EstValue == nil:
		add(4, "Value not stated")
	}
	if o.Deadline != nil {
		days := int(o.Deadline.Sub(sc.today).Hours() / 24)
		switch {
		case days >= 14:
			add(10, fmt.Sprintf("%d days to prepare", days))
		case days >= 5:
			add(6, fmt.Sprintf("%d days to prepare", days))
		default:
			add(2, "Closing soon")
		}
	} else {
		add(6, "No fixed deadline")
	}
	switch o.Confidence {
	case "high":
		add(8, "High confidence")
	case "medium":
		add(5, "Medium confidence")
	case "low":
		add(2, "Low confidence")
	default:
		add(3, "Confidence not given")
	}
	switch o.Effort {
	case "low":
		add(8, "Low effort")
	case "medium":
		add(5, "Medium effort")
	case "high":
		add(2, "High effort")
	default:
		add(4, "Effort not given")
	}
	for _, src := range o.Sources {
		if sc.isFavoured(src.URL) {
			add(4, "From a source you favour")
			break
		}
	}
	// Not interested: what the owner dismissed.
	key := o.TitleKey
	if key == "" {
		key = titleKey(o.Title)
	}
	penal := false
	for i := range sc.dismissed {
		d := &sc.dismissed[i]
		if similar(key, d.TitleKey) >= 0.6 {
			add(-35, "Like one you dismissed: "+clip(d.Title, 60))
			penal = true
			break
		}
	}
	if !penal && o.Country != "" {
		n := 0
		for i := range sc.dismissed {
			d := &sc.dismissed[i]
			if d.Type == o.Type && d.Country == o.Country && (d.DismissReason == "wrong_country" || d.DismissReason == "not_relevant" || d.DismissReason == "wrong_sector") {
				n++
			}
		}
		if n >= 2 {
			add(-15, fmt.Sprintf("You dismissed %d %ss in %s", n, strings.ToLower(TypeLabel(o.Type)), currency.CountryName(o.Country)))
		}
	}
	total := 0
	for _, r := range rs {
		total += r.Points
	}
	if total < 0 {
		total = 0
	}
	if total > 100 {
		total = 100
	}
	o.Fit, o.FitReasons = total, rs
}

func (sc *scorer) isFavoured(u string) bool {
	n := NormURL(u)
	for _, f := range sc.favoured {
		if n == f || strings.HasPrefix(n, f+"/") || strings.HasPrefix(n, f+"?") {
			return true
		}
		host := n
		if i := strings.IndexAny(n, "/?"); i >= 0 {
			host = n[:i]
		}
		fh := f
		if i := strings.IndexByte(f, '/'); i >= 0 {
			fh = f[:i]
		}
		if fh == f && strings.HasSuffix(host, "."+f) {
			return true
		}
	}
	return false
}
