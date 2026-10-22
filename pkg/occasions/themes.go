package occasions

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ThemeInfo describes a seasonal theme the web app knows how to draw
// (src/styles/seasons.css). Custom occasions pick one of these.
type ThemeInfo struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Icon        string `json:"icon"` // Font Awesome class of the greeting chip
	Description string `json:"description"`
	// Default accents (light / dark) — what the theme looks like, for the
	// admin list. Kept in sync with seasons.css.
	Accent     string `json:"accent"`
	AccentDark string `json:"accent_dark"`
}

// Themes lists every seasonal theme.
var Themes = []ThemeInfo{
	{"halloween", "Halloween", "fa-solid fa-ghost", "Pumpkin orange and witching purple, a wobbly glow and a dripping edge on main buttons, a cobweb in the corner.", "#b34700", "#ff9a3d"},
	{"christmas", "Christmas", "fa-solid fa-tree", "Pine green and berry red, a tiny snowflake sparkle on main buttons, holly in the corner.", "#1f7a3d", "#5fd38a"},
	{"new_year", "New Year", "fa-solid fa-champagne-glasses", "Midnight blue and gold, a gold shimmer across main buttons, a star burst in the corner.", "#8a5a00", "#f2c14e"},
	{"valentines", "Valentine's Day", "fa-solid fa-heart", "Rose red, hearts that float up from main buttons, a ribbon of hearts in the corner.", "#c2185b", "#ff7aa8"},
	{"kenya_national", "Kenyan national day", "fa-solid fa-flag", "Black, red, green and white stripes (no emblems), a tricolour edge on main buttons.", "#a0161c", "#ff6b6b"},
	{"mazingira", "Mazingira Day", "fa-solid fa-seedling", "Forest and leaf greens, a leaf that sways on main buttons, leaves in the corner.", "#2e7d32", "#7ed68a"},
	{"labour_day", "Labour Day", "fa-solid fa-helmet-safety", "Workshop amber and steel, a turning cog on main buttons.", "#9a4a00", "#ffb547"},
	{"australia_day", "Australia Day", "fa-solid fa-sun", "Green and gold, a golden glow on main buttons, a wattle sprig in the corner.", "#00704a", "#ffcd3c"},
	{"remembrance", "Remembrance (Anzac Day)", "fa-solid fa-dove", "Quiet and still: stone and rosemary with a red flower — no movement.", "#474f3e", "#cfc9bd"},
	{"easter", "Easter", "fa-solid fa-egg", "Spring pastels, a little hop on main buttons, painted eggs in the corner.", "#7b3fb0", "#c9a7ff"},
	{"diwali", "Diwali", "fa-solid fa-fire-flame-curved", "Marigold and lamp-light, a warm flicker on main buttons, diyas in the corner.", "#b04400", "#ffae42"},
	{"eid", "Eid", "fa-solid fa-moon", "Emerald and gold, twinkling stars on main buttons, a crescent and lantern in the corner.", "#00695c", "#4fd1b8"},
	{"black_friday", "Black Friday", "fa-solid fa-tags", "Black and gold, a gold “deal” tag on main buttons.", "#14110a", "#f5c518"},
	{"womens_day", "International Women's Day", "fa-solid fa-venus", "Purple, green and white, a soft glow on main buttons, a ribbon in the corner.", "#6a1b9a", "#ce93d8"},
	{"mothers_day", "Mother's Day", "fa-solid fa-spa", "Blush pink and rose, a petal on main buttons, tulips in the corner.", "#ad1457", "#f48fb1"},
	{"fathers_day", "Father's Day", "fa-solid fa-mug-hot", "Navy and teal, a lift on main buttons, a bow tie in the corner.", "#1f4e8c", "#7fb2ff"},
}

// ThemeByKey returns the theme (ok=false when unknown).
func ThemeByKey(key string) (ThemeInfo, bool) {
	for _, t := range Themes {
		if t.Key == key {
			return t, true
		}
	}
	return ThemeInfo{}, false
}

// The app's surfaces and primary-button text colours (src/styles/app.css).
const (
	lightSurface = "#ffffff"
	darkSurface  = "#121521"
	darkBtnText  = "#0d0b1f"
)

// ParseHex parses #rgb / #rrggbb.
func ParseHex(s string) (r, g, b float64, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff), true
}

func luminance(hex string) float64 {
	r, g, b, _ := ParseHex(hex)
	ch := func(c float64) float64 {
		c /= 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(r) + 0.7152*ch(g) + 0.0722*ch(b)
}

// Contrast is the WCAG 2 contrast ratio of two colours.
func Contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func toHex(r, g, b float64) string {
	c := func(v float64) int { return int(math.Max(0, math.Min(255, math.Round(v)))) }
	return fmt.Sprintf("#%02x%02x%02x", c(r), c(g), c(b))
}

func mix(hex, with string, t float64) string {
	r, g, b, _ := ParseHex(hex)
	r2, g2, b2, _ := ParseHex(with)
	return toHex(r+(r2-r)*t, g+(g2-g)*t, b+(b2-b)*t)
}

// NormalisePalette checks a custom palette: the light accent must reach
// WCAG AA (4.5:1) as white button text and as link text on white; the dark
// accent (derived from the light one when empty) must reach 4.5:1 with the
// dark theme's button text and on dark surfaces. Returns a cleaned copy.
func NormalisePalette(p *Palette) (*Palette, error) {
	if p == nil || (strings.TrimSpace(p.Accent) == "" && strings.TrimSpace(p.AccentDark) == "") {
		return nil, nil
	}
	out := &Palette{}
	if a := strings.TrimSpace(p.Accent); a != "" {
		r, g, b, ok := ParseHex(a)
		if !ok {
			return nil, fmt.Errorf("accent colour %q is not a #rrggbb colour", a)
		}
		out.Accent = toHex(r, g, b)
		if c := Contrast(out.Accent, lightSurface); c < 4.5 {
			return nil, fmt.Errorf("the light-mode accent %s has %.1f:1 contrast with white button text — pick a darker colour (needs 4.5:1)", out.Accent, c)
		}
	}
	if a := strings.TrimSpace(p.AccentDark); a != "" {
		r, g, b, ok := ParseHex(a)
		if !ok {
			return nil, fmt.Errorf("dark-mode accent %q is not a #rrggbb colour", a)
		}
		out.AccentDark = toHex(r, g, b)
	} else if out.Accent != "" {
		// Lighten the light accent until it reads on the dark theme.
		d := out.Accent
		for t := 0.0; t <= 1.0 && (Contrast(d, darkSurface) < 4.5 || Contrast(d, darkBtnText) < 4.5); t += 0.05 {
			d = mix(out.Accent, "#ffffff", t)
		}
		out.AccentDark = d
	}
	if out.AccentDark != "" {
		if c := math.Min(Contrast(out.AccentDark, darkSurface), Contrast(out.AccentDark, darkBtnText)); c < 4.5 {
			return nil, fmt.Errorf("the dark-mode accent %s has %.1f:1 contrast on the dark theme — pick a lighter colour (needs 4.5:1)", out.AccentDark, c)
		}
	}
	return out, nil
}
