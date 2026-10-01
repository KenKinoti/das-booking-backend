package expenses

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// PDFText extracts the readable text of a PDF without third-party libraries.
// It handles uncompressed and Flate-compressed content streams, object
// streams (PDF 1.5+), literal and hex strings, and ToUnicode CMaps (so
// subset/CID fonts used by Google and most invoicing systems decode). It is a
// best-effort reader for machine-generated invoices — scanned images return
// "" and the caller falls back to the email body or AI.
func PDFText(data []byte) string {
	if !bytes.HasPrefix(bytes.TrimLeft(data, "\x00\r\n\t "), []byte("%PDF")) {
		return ""
	}
	doc := parsePDF(data)
	var out strings.Builder
	for _, num := range doc.order {
		o := doc.objs[num]
		if o.stream == nil || !o.isContent() {
			continue
		}
		txt := doc.contentText(o.stream)
		if strings.TrimSpace(txt) != "" {
			out.WriteString(txt)
			out.WriteString("\n")
		}
	}
	return CleanText(out.String())
}

type pdfObj struct {
	dict   string
	stream []byte // decoded (nil when not a stream or undecodable)
}

func (o pdfObj) isContent() bool {
	d := o.dict
	for _, skip := range []string{"/Type /XRef", "/Type/XRef", "/Type /ObjStm", "/Type/ObjStm", "/Subtype /Image", "/Subtype/Image",
		"/FontFile", "/Length1", "/Type /Metadata", "/Type/Metadata", "/Subtype /Type1C", "/Subtype/Type1C", "/Subtype /CIDFontType0C", "/Subtype/CIDFontType0C", "/Subtype /OpenType", "/Subtype/OpenType"} {
		if strings.Contains(d, skip) {
			return false
		}
	}
	s := o.stream
	if bytes.Contains(s, []byte("begincmap")) {
		return false
	}
	return bytes.Contains(s, []byte("BT")) && (bytes.Contains(s, []byte("Tj")) || bytes.Contains(s, []byte("TJ")) || bytes.Contains(s, []byte("'")))
}

type pdfDoc struct {
	objs     map[int]pdfObj
	order    []int
	fontMaps map[string]map[string]string // font resource name → code (hex) → text
	merged   map[string]string
	codeLen  map[string]int // font resource name → bytes per code
}

var (
	reObj      = regexp.MustCompile(`(\d+)\s+(\d+)\s+obj\b`)
	reStream   = regexp.MustCompile(`stream\r?\n`)
	reFontRes  = regexp.MustCompile(`/([A-Za-z0-9_.+-]+)\s+(\d+)\s+\d+\s+R`)
	reToUni    = regexp.MustCompile(`/ToUnicode\s+(\d+)\s+\d+\s+R`)
	reFontDict = regexp.MustCompile(`/Font\s*<<([^<>]*)>>`)
	reFontRef  = regexp.MustCompile(`/Font\s+(\d+)\s+\d+\s+R`)
	reLength   = regexp.MustCompile(`/Length\s+(\d+)(\s+\d+\s+R)?`)
)

func parsePDF(data []byte) *pdfDoc {
	d := &pdfDoc{objs: map[int]pdfObj{}, fontMaps: map[string]map[string]string{}, merged: map[string]string{}, codeLen: map[string]int{}}
	locs := reObj.FindAllSubmatchIndex(data, -1)
	for i, l := range locs {
		num, _ := strconv.Atoi(string(data[l[2]:l[3]]))
		end := len(data)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := data[l[1]:end]
		if e := bytes.Index(body, []byte("endobj")); e >= 0 {
			body = body[:e]
		}
		o := pdfObj{}
		if sl := reStream.FindIndex(body); sl != nil {
			o.dict = string(body[:sl[0]])
			raw := body[sl[1]:]
			if m := reLength.FindStringSubmatch(o.dict); m != nil && m[2] == "" {
				if n, err := strconv.Atoi(m[1]); err == nil && n <= len(raw) {
					raw = raw[:n]
				}
			}
			if e := bytes.LastIndex(raw, []byte("endstream")); e >= 0 {
				raw = raw[:e]
			}
			o.stream = decodeStream(o.dict, raw)
		} else {
			o.dict = string(body)
		}
		if _, seen := d.objs[num]; !seen {
			d.order = append(d.order, num)
		}
		d.objs[num] = o
	}
	// Objects inside object streams.
	for _, num := range append([]int{}, d.order...) {
		o := d.objs[num]
		if o.stream == nil || !(strings.Contains(o.dict, "/ObjStm")) {
			continue
		}
		first := 0
		if m := regexp.MustCompile(`/First\s+(\d+)`).FindStringSubmatch(o.dict); m != nil {
			first, _ = strconv.Atoi(m[1])
		}
		if first <= 0 || first > len(o.stream) {
			continue
		}
		hdr := strings.Fields(string(o.stream[:first]))
		type ent struct{ num, off int }
		var ents []ent
		for i := 0; i+1 < len(hdr); i += 2 {
			n, e1 := strconv.Atoi(hdr[i])
			off, e2 := strconv.Atoi(hdr[i+1])
			if e1 == nil && e2 == nil {
				ents = append(ents, ent{n, off})
			}
		}
		for i, e := range ents {
			s := first + e.off
			t := len(o.stream)
			if i+1 < len(ents) {
				t = first + ents[i+1].off
			}
			if s < 0 || s > t || t > len(o.stream) {
				continue
			}
			if _, seen := d.objs[e.num]; !seen {
				d.objs[e.num] = pdfObj{dict: string(o.stream[s:t])}
				d.order = append(d.order, e.num)
			}
		}
	}
	// Fonts: resource name → font object → ToUnicode CMap.
	cmapOf := func(fontObj int) (map[string]string, int) {
		fo, ok := d.objs[fontObj]
		if !ok {
			return nil, 0
		}
		m := reToUni.FindStringSubmatch(fo.dict)
		if m == nil {
			return nil, 0
		}
		n, _ := strconv.Atoi(m[1])
		cm, ok := d.objs[n]
		if !ok || cm.stream == nil {
			return nil, 0
		}
		return parseCMap(string(cm.stream))
	}
	addFonts := func(s string) {
		for _, fm := range reFontRes.FindAllStringSubmatch(s, -1) {
			n, _ := strconv.Atoi(fm[2])
			if mp, w := cmapOf(n); mp != nil {
				d.fontMaps[fm[1]] = mp
				d.codeLen[fm[1]] = w
				for k, v := range mp {
					if _, ok := d.merged[k]; !ok {
						d.merged[k] = v
					}
				}
			}
		}
	}
	for _, num := range d.order {
		dict := d.objs[num].dict
		for _, fd := range reFontDict.FindAllStringSubmatch(dict, -1) {
			addFonts(fd[1])
		}
		for _, fr := range reFontRef.FindAllStringSubmatch(dict, -1) {
			n, _ := strconv.Atoi(fr[1])
			if fo, ok := d.objs[n]; ok {
				addFonts(fo.dict)
			}
		}
	}
	return d
}

func decodeStream(dict string, raw []byte) []byte {
	if strings.Contains(dict, "/FlateDecode") || strings.Contains(dict, "/Fl ") || strings.Contains(dict, "/Fl]") {
		raw = bytes.TrimLeft(raw, "\r\n")
		r, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil
		}
		out, err := io.ReadAll(io.LimitReader(r, 20<<20))
		if len(out) == 0 && err != nil {
			return nil
		}
		return out
	}
	if strings.Contains(dict, "/Filter") {
		return nil // DCT, LZW, ASCII85 … not needed for text
	}
	return raw
}

var (
	reBfChar  = regexp.MustCompile(`(?s)beginbfchar(.*?)endbfchar`)
	reBfRange = regexp.MustCompile(`(?s)beginbfrange(.*?)endbfrange`)
	reHexTok  = regexp.MustCompile(`<([0-9A-Fa-f\s]*)>|\[([^\]]*)\]`)
)

// parseCMap reads bfchar / bfrange entries: source code (upper hex) → text.
func parseCMap(s string) (map[string]string, int) {
	out := map[string]string{}
	width := 0
	for _, blk := range reBfChar.FindAllStringSubmatch(s, -1) {
		toks := reHexTok.FindAllStringSubmatch(blk[1], -1)
		for i := 0; i+1 < len(toks); i += 2 {
			src := strings.ToUpper(strings.Join(strings.Fields(toks[i][1]), ""))
			out[src] = utf16Hex(toks[i+1][1])
			width = len(src) / 2
		}
	}
	for _, blk := range reBfRange.FindAllStringSubmatch(s, -1) {
		toks := reHexTok.FindAllStringSubmatch(blk[1], -1)
		for i := 0; i+2 < len(toks); i += 3 {
			lo, hi := strings.ToUpper(strings.TrimSpace(toks[i][1])), strings.ToUpper(strings.TrimSpace(toks[i+1][1]))
			a, e1 := strconv.ParseUint(lo, 16, 32)
			b, e2 := strconv.ParseUint(hi, 16, 32)
			if e1 != nil || e2 != nil || b < a || b-a > 4096 {
				continue
			}
			width = len(lo) / 2
			if toks[i+2][2] != "" { // array of destinations
				dst := regexp.MustCompile(`<([0-9A-Fa-f]*)>`).FindAllStringSubmatch(toks[i+2][2], -1)
				for k := uint64(0); k <= b-a && int(k) < len(dst); k++ {
					out[strings.ToUpper(padHex(a+k, len(lo)))] = utf16Hex(dst[k][1])
				}
				continue
			}
			base := strings.TrimSpace(toks[i+2][1])
			bv, err := strconv.ParseUint(base, 16, 32)
			if err != nil {
				continue
			}
			for k := uint64(0); k <= b-a; k++ {
				out[strings.ToUpper(padHex(a+k, len(lo)))] = utf16Hex(padHex(bv+k, len(base)))
			}
		}
	}
	return out, width
}

func padHex(v uint64, n int) string {
	s := strconv.FormatUint(v, 16)
	for len(s) < n {
		s = "0" + s
	}
	return s
}

func utf16Hex(h string) string {
	h = strings.Join(strings.Fields(h), "")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b)%2 == 1 {
		return string(b)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return string(utf16.Decode(u))
}

// contentText interprets text operators in a content stream.
func (d *pdfDoc) contentText(s []byte) string {
	var out strings.Builder
	font := ""
	var operands []string // raw string operands since the last operator
	lastY := ""
	i := 0
	n := len(s)
	emit := func(t string) { out.WriteString(t) }
	for i < n {
		c := s[i]
		switch {
		case c == '(':
			str, j := readLiteral(s, i)
			operands = append(operands, "L"+str)
			i = j
		case c == '<' && i+1 < n && s[i+1] != '<':
			j := bytes.IndexByte(s[i:], '>')
			if j < 0 {
				return out.String()
			}
			operands = append(operands, "H"+string(s[i+1:i+j]))
			i += j + 1
		case c == '[':
			operands = append(operands, "[")
			i++
		case c == ']':
			operands = append(operands, "]")
			i++
		case c == '%':
			for i < n && s[i] != '\n' && s[i] != '\r' {
				i++
			}
		case c == '/':
			j := i + 1
			for j < n && !isDelim(s[j]) {
				j++
			}
			operands = append(operands, "N"+string(s[i+1:j]))
			i = j
		case isSpace(c):
			i++
		default:
			j := i
			for j < n && !isDelim(s[j]) && !isSpace(s[j]) {
				j++
			}
			if j == i {
				i++
				continue
			}
			tok := string(s[i:j])
			i = j
			if isNumber(tok) {
				operands = append(operands, "#"+tok)
				continue
			}
			switch tok {
			case "Tf":
				for _, o := range operands {
					if strings.HasPrefix(o, "N") {
						font = o[1:]
					}
				}
			case "Tj", "'", "\"":
				if tok != "Tj" {
					emit("\n")
				}
				for _, o := range operands {
					if o[0] == 'L' || o[0] == 'H' {
						emit(d.decode(font, o))
					}
				}
			case "TJ":
				for _, o := range operands {
					switch o[0] {
					case 'L', 'H':
						emit(d.decode(font, o))
					case '#':
						if v, err := strconv.ParseFloat(o[1:], 64); err == nil && v < -180 {
							emit(" ")
						}
					}
				}
			case "Td", "TD", "Tm":
				// New line when the vertical position changes, else a space.
				y := ""
				nums := []string{}
				for _, o := range operands {
					if o[0] == '#' {
						nums = append(nums, o[1:])
					}
				}
				if tok == "Tm" && len(nums) == 6 {
					y = nums[5]
					if y != lastY {
						emit("\n")
					} else {
						emit(" ")
					}
					lastY = y
				} else if len(nums) == 2 {
					if v, _ := strconv.ParseFloat(nums[1], 64); v != 0 {
						emit("\n")
					} else {
						emit(" ")
					}
				}
			case "T*", "ET":
				emit("\n")
			}
			operands = operands[:0]
		}
	}
	return out.String()
}

func (d *pdfDoc) decode(font, op string) string {
	kind, v := op[0], op[1:]
	var raw []byte
	if kind == 'H' {
		h := strings.Join(strings.Fields(v), "")
		if len(h)%2 == 1 {
			h += "0"
		}
		b, err := hex.DecodeString(h)
		if err != nil {
			return ""
		}
		raw = b
	} else {
		raw = []byte(v)
	}
	mp := d.fontMaps[font]
	w := d.codeLen[font]
	if mp == nil && len(d.merged) > 0 && kind == 'H' {
		mp = d.merged
	}
	if mp != nil {
		if w <= 0 {
			w = 1
			if kind == 'H' && len(raw)%2 == 0 {
				w = 2
			}
		}
		var b strings.Builder
		for i := 0; i+w <= len(raw); i += w {
			key := strings.ToUpper(hex.EncodeToString(raw[i : i+w]))
			if t, ok := mp[key]; ok {
				b.WriteString(t)
			} else if w == 1 && raw[i] >= 32 && raw[i] < 127 {
				b.WriteByte(raw[i])
			}
		}
		return b.String()
	}
	if kind == 'H' && len(raw) >= 2 && raw[0] == 0 { // 2-byte codes without a map: assume Unicode-ish
		var b strings.Builder
		for i := 0; i+1 < len(raw); i += 2 {
			r := rune(raw[i])<<8 | rune(raw[i+1])
			if r >= 32 {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	// WinAnsi-ish single bytes.
	var b strings.Builder
	for _, c := range raw {
		switch {
		case c >= 32 && c < 127:
			b.WriteByte(c)
		case c == 0x80:
			b.WriteRune('€')
		case c == 0xA3:
			b.WriteRune('£')
		case c == 0xA0:
			b.WriteByte(' ')
		case c >= 0xA0:
			b.WriteRune(rune(c))
		}
	}
	return b.String()
}

func readLiteral(s []byte, i int) (string, int) {
	depth := 0
	var b bytes.Buffer
	j := i
	for j < len(s) {
		c := s[j]
		switch c {
		case '\\':
			if j+1 >= len(s) {
				return b.String(), j + 1
			}
			nx := s[j+1]
			switch nx {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b', 'f':
			case '(', ')', '\\':
				b.WriteByte(nx)
			case '\r', '\n':
			default:
				if nx >= '0' && nx <= '7' {
					k := j + 1
					v := 0
					for k < len(s) && k < j+4 && s[k] >= '0' && s[k] <= '7' {
						v = v*8 + int(s[k]-'0')
						k++
					}
					b.WriteByte(byte(v))
					j = k
					continue
				}
				b.WriteByte(nx)
			}
			j += 2
			continue
		case '(':
			depth++
			if depth > 1 {
				b.WriteByte(c)
			}
		case ')':
			depth--
			if depth == 0 {
				return b.String(), j + 1
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
		j++
	}
	return b.String(), j
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}
func isDelim(c byte) bool {
	return c == '(' || c == ')' || c == '<' || c == '>' || c == '[' || c == ']' || c == '{' || c == '}' || c == '/' || c == '%'
}
func isNumber(t string) bool {
	if t == "" {
		return false
	}
	_, err := strconv.ParseFloat(t, 64)
	return err == nil
}
