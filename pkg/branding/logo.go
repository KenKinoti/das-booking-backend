// Package branding stores each organisation's company logo — the single
// source of truth for the logo shown on invoices, quotes, receipts, emails,
// public pages and the app sidebar — and serves it from a public, cacheable
// URL.
package branding

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"image"
	_ "image/jpeg" // register decoders for DecodeConfig
	_ "image/png"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxLogoBytes is the largest logo accepted (1 MB).
const MaxLogoBytes = 1 << 20

// Supported content types.
const (
	TypePNG  = "image/png"
	TypeJPEG = "image/jpeg"
	TypeWebP = "image/webp"
	TypeSVG  = "image/svg+xml"
)

// OrgLogo is one organisation's logo image.
type OrgLogo struct {
	OrganizationID string    `json:"organization_id" gorm:"type:varchar(255);primaryKey"`
	ContentType    string    `json:"content_type" gorm:"type:varchar(50);not null"`
	Data           []byte    `json:"-" gorm:"type:bytea;not null"`
	ETag           string    `json:"etag" gorm:"column:etag;type:varchar(64);not null"`
	Size           int       `json:"size"`
	Width          int       `json:"width"`
	Height         int       `json:"height"`
	UpdatedAt      time.Time `json:"updated_at"`
	UpdatedBy      string    `json:"updated_by" gorm:"type:varchar(255)"`
}

func (OrgLogo) TableName() string { return "org_logos" }

// ValidationError is a user-facing reason a logo was rejected.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(msg string) error { return &ValidationError{Msg: msg} }

// IsValidation reports whether err is a ValidationError.
func IsValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

// Checked is the result of validating an uploaded logo.
type Checked struct {
	ContentType   string
	Width, Height int
	ETag          string
}

// Validate checks size, detects the real type from the file's magic bytes
// (the declared type is never trusted), rejects unsafe SVGs and returns the
// content type, dimensions (when known) and a content hash for the ETag.
func Validate(data []byte) (Checked, error) {
	if len(data) == 0 {
		return Checked{}, invalid("The file is empty")
	}
	if len(data) > MaxLogoBytes {
		return Checked{}, invalid("Logo must be 1 MB or smaller")
	}
	ct := Detect(data)
	var w, h int
	switch ct {
	case TypePNG, TypeJPEG:
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return Checked{}, invalid("The image file is damaged or not a real " + strings.TrimPrefix(ct, "image/") + " image")
		}
		w, h = cfg.Width, cfg.Height
	case TypeWebP:
		var ok bool
		if w, h, ok = webpSize(data); !ok {
			return Checked{}, invalid("The WebP file is damaged")
		}
	case TypeSVG:
		if err := CheckSVG(data); err != nil {
			return Checked{}, err
		}
		w, h = svgSize(data)
	default:
		return Checked{}, invalid("Use a PNG, JPG, WebP or SVG image")
	}
	if (ct != TypeSVG) && (w < 1 || h < 1 || w > 10000 || h > 10000) {
		return Checked{}, invalid("Image dimensions are not supported")
	}
	sum := sha256.Sum256(data)
	return Checked{ContentType: ct, Width: w, Height: h, ETag: hex.EncodeToString(sum[:])[:20]}, nil
}

// Detect returns the image type from magic bytes, or "" when unsupported.
func Detect(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return TypePNG
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return TypeJPEG
	case len(b) > 16 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return TypeWebP
	}
	s := strings.TrimLeft(string(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))), " \t\r\n")
	if strings.HasPrefix(s, "<") && strings.Contains(strings.ToLower(s[:min(len(s), 4096)]), "<svg") {
		return TypeSVG
	}
	return ""
}

func webpSize(b []byte) (int, int, bool) {
	if len(b) < 30 {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8X":
		w := int(b[24]) | int(b[25])<<8 | int(b[26])<<16
		h := int(b[27]) | int(b[28])<<8 | int(b[29])<<16
		return w + 1, h + 1, true
	case "VP8L":
		if b[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		return int(bits&0x3FFF) + 1, int((bits>>14)&0x3FFF) + 1, true
	case "VP8 ":
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, false
		}
		return int(binary.LittleEndian.Uint16(b[26:28]) & 0x3FFF), int(binary.LittleEndian.Uint16(b[28:30]) & 0x3FFF), true
	}
	return 0, 0, false
}

var (
	dangerousSVGElems = map[string]bool{
		"script": true, "foreignobject": true, "iframe": true, "embed": true, "object": true,
		"audio": true, "video": true, "handler": true, "listener": true,
	}
	cssURL  = regexp.MustCompile(`(?i)url\s*\(\s*['"]?\s*([^'")\s]*)`)
	spaceRe = regexp.MustCompile(`[\s\x00-\x1f]+`)
)

// safeRef allows in-document references (#id) and embedded raster images.
func safeRef(v string) bool {
	v = strings.ToLower(spaceRe.ReplaceAllString(v, ""))
	if v == "" || strings.HasPrefix(v, "#") {
		return true
	}
	for _, p := range []string{"data:image/png", "data:image/jpeg", "data:image/jpg", "data:image/gif", "data:image/webp"} {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

func checkCSS(s string) error {
	l := strings.ToLower(spaceRe.ReplaceAllString(s, ""))
	if strings.Contains(l, "@import") || strings.Contains(l, "javascript:") || strings.Contains(l, "expression(") {
		return invalid("This SVG contains styles that load external content, which isn't allowed")
	}
	for _, m := range cssURL.FindAllStringSubmatch(s, -1) {
		if !safeRef(m[1]) {
			return invalid("This SVG references external files, which isn't allowed")
		}
	}
	return nil
}

// CheckSVG rejects SVGs that could run script or load external content:
// <script>, <foreignObject> and other embedding elements, on* event
// attributes, javascript: URLs, external href/src references, entity
// declarations and external CSS.
func CheckSVG(data []byte) error {
	lower := strings.ToLower(string(data))
	if strings.Contains(lower, "<!entity") || strings.Contains(lower, "<!doctype") {
		return invalid("SVGs with DOCTYPE or entity declarations aren't allowed")
	}
	if strings.Contains(spaceRe.ReplaceAllString(lower, ""), "javascript:") {
		return invalid("This SVG contains a script link, which isn't allowed")
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	dec.Entity = map[string]string{}
	root := ""
	inStyle := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return invalid("The SVG file is not valid XML")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if root == "" {
				root = name
				if name != "svg" {
					return invalid("The file is not an SVG image")
				}
			}
			if dangerousSVGElems[name] {
				return invalid("This SVG contains <" + t.Name.Local + ">, which isn't allowed in a logo")
			}
			if name == "style" {
				inStyle++
			}
			for _, a := range t.Attr {
				an := strings.ToLower(a.Name.Local)
				if strings.HasPrefix(an, "on") {
					return invalid("This SVG contains an event handler (" + a.Name.Local + "), which isn't allowed")
				}
				if an == "href" || an == "src" || (an == "base" && a.Name.Space != "") {
					if !safeRef(a.Value) {
						return invalid("This SVG links to an external file, which isn't allowed")
					}
				}
				if an == "style" || strings.Contains(strings.ToLower(a.Value), "url(") {
					if err := checkCSS(a.Value); err != nil {
						return err
					}
				}
			}
		case xml.EndElement:
			if strings.ToLower(t.Name.Local) == "style" && inStyle > 0 {
				inStyle--
			}
		case xml.CharData:
			if inStyle > 0 {
				if err := checkCSS(string(t)); err != nil {
					return err
				}
			}
		case xml.Directive:
			return invalid("SVGs with DOCTYPE or entity declarations aren't allowed")
		case xml.ProcInst:
			if strings.EqualFold(t.Target, "xml-stylesheet") {
				return invalid("This SVG loads an external stylesheet, which isn't allowed")
			}
		}
	}
	if root != "svg" {
		return invalid("The file is not an SVG image")
	}
	return nil
}

var numRe = regexp.MustCompile(`^\s*([0-9.]+)`)

// svgSize reads width/height (or the viewBox) from the root element.
func svgSize(data []byte) (int, int) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return 0, 0
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		var w, h float64
		var vb string
		for _, a := range se.Attr {
			switch strings.ToLower(a.Name.Local) {
			case "width":
				w = parseNum(a.Value)
			case "height":
				h = parseNum(a.Value)
			case "viewbox":
				vb = a.Value
			}
		}
		if (w == 0 || h == 0) && vb != "" {
			f := strings.FieldsFunc(vb, func(r rune) bool { return r == ' ' || r == ',' })
			if len(f) == 4 {
				w, h = parseNum(f[2]), parseNum(f[3])
			}
		}
		return int(w + 0.5), int(h + 0.5)
	}
}

func parseNum(s string) float64 {
	m := numRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimRight(m[1], "."), 64)
	return f
}

// DecodeDataURI decodes "data:<type>;base64,<data>" (or a bare base64
// string). The declared type is ignored — Validate detects the real one.
func DecodeDataURI(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), "data:") {
		i := strings.Index(s, ",")
		if i < 0 {
			return nil, invalid("Invalid data URI")
		}
		meta := strings.ToLower(s[5:i])
		payload := s[i+1:]
		if !strings.HasSuffix(meta, ";base64") {
			// URL-encoded text (typically SVG)
			dec, err := urlUnescape(payload)
			if err != nil {
				return nil, invalid("Invalid data URI")
			}
			return []byte(dec), nil
		}
		s = payload
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if len(s) > MaxLogoBytes*4/3+8 {
		return nil, invalid("Logo must be 1 MB or smaller")
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		if b2, err2 := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err2 == nil {
			return b2, nil
		}
		return nil, invalid("The image data could not be read")
	}
	return b, nil
}

func urlUnescape(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' {
			if i+2 >= len(s) {
				return "", errors.New("bad escape")
			}
			b, err := hex.DecodeString(s[i+1 : i+3])
			if err != nil {
				return "", err
			}
			out.WriteByte(b[0])
			i += 2
			continue
		}
		out.WriteByte(c)
	}
	return out.String(), nil
}
