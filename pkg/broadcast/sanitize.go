package broadcast

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Simple rich text: paragraphs, bold, italic, links, bullet and numbered
// lists. Whatever the composer (or an API caller) sends is parsed into that
// small tree and rendered again, so nothing else — scripts, styles, event
// handlers, images, unsafe links — survives. Plain text (no tags) becomes
// paragraphs on blank lines, line breaks inside them and "- " lines as a
// bullet list.
// ---------------------------------------------------------------------------

// MaxBodyInput is the most HTML accepted for a body.
const MaxBodyInput = 50000

type rnode struct {
	tag  string // "" text | root | p | ul | ol | li | strong | em | a | br
	text string
	href string
	kids []*rnode
}

func (n *rnode) add(k *rnode) *rnode {
	n.kids = append(n.kids, k)
	return k
}

// Tags whose content is dropped entirely.
var dropContent = map[string]bool{
	"script": true, "style": true, "iframe": true, "object": true, "embed": true, "template": true, "noscript": true,
	"svg": true, "math": true, "head": true, "title": true, "textarea": true, "select": true, "option": true, "button": true,
}

var blockAlias = map[string]bool{
	"p": true, "div": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "blockquote": true,
	"section": true, "article": true, "header": true, "footer": true, "pre": true, "address": true, "figure": true, "figcaption": true,
	"tr": true, "dt": true, "dd": true,
}

func mapTag(t string) string {
	switch t {
	case "b", "strong":
		return "strong"
	case "i", "em":
		return "em"
	case "a", "br", "ul", "ol", "li":
		return t
	}
	if blockAlias[t] {
		return "p"
	}
	return "" // transparent: keep the children only
}

func isInline(t string) bool { return t == "strong" || t == "em" || t == "a" }

// SafeLink cleans a link for the body: https/http URLs, mailto: and in-app
// paths ("/settings"). ok is false for anything else (javascript:, data: …).
func SafeLink(raw string) (string, bool) {
	u := strings.TrimSpace(html.UnescapeString(raw))
	if u == "" || len(u) > 1000 || strings.ContainsAny(u, "<>\"'\\\r\n\t ") {
		return "", false
	}
	if strings.HasPrefix(u, "/") {
		if strings.HasPrefix(u, "//") {
			return "", false
		}
		return u, true
	}
	p, err := url.Parse(u)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(p.Scheme) {
	case "https", "http":
		if p.Host == "" || p.User != nil {
			return "", false
		}
		return p.String(), true
	case "mailto":
		if !strings.Contains(p.Opaque, "@") {
			return "", false
		}
		return u, true
	}
	return "", false
}

// SafeCTA accepts an in-app path or an https URL for the button.
func SafeCTA(raw string) (string, bool) {
	u, ok := SafeLink(raw)
	if !ok || strings.HasPrefix(strings.ToLower(u), "mailto:") || strings.HasPrefix(strings.ToLower(u), "http:") {
		return "", false
	}
	return u, true
}

// SafeImage accepts https image URLs only.
func SafeImage(raw string) (string, bool) {
	u, ok := SafeLink(raw)
	if !ok || !strings.HasPrefix(strings.ToLower(u), "https://") {
		return "", false
	}
	return u, true
}

var attrRe = regexp.MustCompile(`(?is)([a-z_:][-a-z0-9_:.]*)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+)))?`)

func hrefOf(attrs string) string {
	for _, m := range attrRe.FindAllStringSubmatch(attrs, -1) {
		if strings.EqualFold(m[1], "href") {
			return m[2] + m[3] + m[4]
		}
	}
	return ""
}

type parser struct {
	root  *rnode
	stack []*rnode
}

func (p *parser) top() *rnode { return p.stack[len(p.stack)-1] }

// popTo pops until an element with tag is on top (and pops it) without
// crossing a boundary tag; it reports whether one was found.
func (p *parser) popTo(tag string, bounds ...string) bool {
	for i := len(p.stack) - 1; i > 0; i-- {
		t := p.stack[i].tag
		if t == tag {
			p.stack = p.stack[:i]
			return true
		}
		for _, b := range bounds {
			if t == b {
				return false
			}
		}
	}
	return false
}

// closeInline pops inline elements and an open paragraph.
func (p *parser) closeInline() {
	for len(p.stack) > 1 {
		t := p.top().tag
		if isInline(t) || t == "p" {
			p.stack = p.stack[:len(p.stack)-1]
			continue
		}
		break
	}
}

// inlineHost makes sure inline content has a paragraph or list item to go in.
func (p *parser) inlineHost() *rnode {
	switch p.top().tag {
	case "root":
		n := p.top().add(&rnode{tag: "p"})
		p.stack = append(p.stack, n)
	case "ul", "ol":
		n := p.top().add(&rnode{tag: "li"})
		p.stack = append(p.stack, n)
	}
	return p.top()
}

func (p *parser) inLi() bool {
	for i := len(p.stack) - 1; i > 0; i-- {
		switch p.stack[i].tag {
		case "li":
			return true
		case "ul", "ol":
			return false
		}
	}
	return false
}

func (p *parser) hasOpen(tag string) bool {
	for _, n := range p.stack {
		if n.tag == tag {
			return true
		}
	}
	return false
}

func (p *parser) start(name, attrs string) {
	switch t := mapTag(name); t {
	case "p":
		if p.inLi() {
			if top := p.top(); len(top.kids) > 0 {
				p.inlineHost().add(&rnode{tag: "br"})
			}
			return
		}
		p.closeInline()
		n := p.top().add(&rnode{tag: "p"})
		p.stack = append(p.stack, n)
	case "ul", "ol":
		if p.inLi() {
			// nested list inside the item: close inline elements of the item
			for len(p.stack) > 1 && isInline(p.top().tag) {
				p.stack = p.stack[:len(p.stack)-1]
			}
		} else {
			p.closeInline()
		}
		n := p.top().add(&rnode{tag: t})
		p.stack = append(p.stack, n)
	case "li":
		if !p.hasOpen("ul") && !p.hasOpen("ol") {
			// an item outside a list is a paragraph
			p.closeInline()
			n := p.top().add(&rnode{tag: "p"})
			p.stack = append(p.stack, n)
			return
		}
		// close up to the nearest list (an item left open ends here)
		for len(p.stack) > 1 && p.top().tag != "ul" && p.top().tag != "ol" {
			p.stack = p.stack[:len(p.stack)-1]
		}
		n := p.top().add(&rnode{tag: "li"})
		p.stack = append(p.stack, n)
	case "strong", "em":
		host := p.inlineHost()
		n := host.add(&rnode{tag: t})
		p.stack = append(p.stack, n)
	case "a":
		href, ok := SafeLink(hrefOf(attrs))
		if !ok || p.hasOpen("a") {
			return // transparent
		}
		host := p.inlineHost()
		n := host.add(&rnode{tag: "a", href: href})
		p.stack = append(p.stack, n)
	case "br":
		p.inlineHost().add(&rnode{tag: "br"})
	}
}

func (p *parser) end(name string) {
	switch t := mapTag(name); t {
	case "p":
		if p.inLi() {
			return
		}
		p.popTo("p", "li", "ul", "ol")
	case "ul", "ol":
		p.popTo(t)
	case "li":
		p.popTo("li", "ul", "ol")
	case "strong", "em", "a":
		p.popTo(t, "p", "li", "ul", "ol")
	}
}

var spaceRe = regexp.MustCompile(`[\s\x{00a0}]+`)

func (p *parser) text(s string) {
	s = spaceRe.ReplaceAllString(html.UnescapeString(s), " ")
	if s == "" {
		return
	}
	if strings.TrimSpace(s) == "" {
		switch p.top().tag {
		case "root", "ul", "ol":
			return
		}
	}
	host := p.inlineHost()
	if n := len(host.kids); n > 0 && host.kids[n-1].tag == "" {
		host.kids[n-1].text += s
		return
	}
	host.add(&rnode{text: s})
}

// parseHTML builds the tree from (untrusted) HTML.
func parseHTML(in string) *rnode {
	root := &rnode{tag: "root"}
	p := &parser{root: root, stack: []*rnode{root}}
	i := 0
	n := len(in)
	textStart := 0
	flush := func(end int) {
		if end > textStart {
			p.text(in[textStart:end])
		}
	}
	for i < n {
		if in[i] != '<' {
			i++
			continue
		}
		rest := in[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			flush(i)
			j := strings.Index(rest[4:], "-->")
			if j < 0 {
				i = n
			} else {
				i += 4 + j + 3
			}
			textStart = i
			continue
		case strings.HasPrefix(rest, "<!") || strings.HasPrefix(rest, "<?"):
			flush(i)
			j := strings.IndexByte(rest, '>')
			if j < 0 {
				i = n
			} else {
				i += j + 1
			}
			textStart = i
			continue
		}
		closing := len(rest) > 1 && rest[1] == '/'
		k := 1
		if closing {
			k = 2
		}
		if k >= len(rest) || !isLetter(rest[k]) {
			i++ // a lone "<" is text
			continue
		}
		flush(i)
		j := k
		for j < len(rest) && (isLetter(rest[j]) || (rest[j] >= '0' && rest[j] <= '9')) {
			j++
		}
		name := strings.ToLower(rest[k:j])
		// find the end of the tag, honouring quoted attribute values
		end, q := j, byte(0)
		for end < len(rest) {
			c := rest[end]
			if q != 0 {
				if c == q {
					q = 0
				}
			} else if c == '"' || c == '\'' {
				q = c
			} else if c == '>' {
				break
			}
			end++
		}
		attrs := ""
		if end > j {
			attrs = rest[j:end]
		}
		if end >= len(rest) {
			i = n
		} else {
			i += end + 1
		}
		textStart = i
		if closing {
			p.end(name)
			continue
		}
		if dropContent[name] && !strings.HasSuffix(strings.TrimSpace(attrs), "/") {
			// skip to the matching end tag
			low := strings.ToLower(in[i:])
			if e := strings.Index(low, "</"+name); e >= 0 {
				i += e
				if g := strings.IndexByte(in[i:], '>'); g >= 0 {
					i += g + 1
				} else {
					i = n
				}
			} else {
				i = n
			}
			textStart = i
			continue
		}
		p.start(name, attrs)
	}
	flush(n)
	return root
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

var bulletLine = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)])\s+`)

// parsePlain turns plain text into the tree.
func parsePlain(in string) *rnode {
	root := &rnode{tag: "root"}
	in = strings.ReplaceAll(strings.ReplaceAll(in, "\r\n", "\n"), "\r", "\n")
	for _, block := range regexp.MustCompile(`\n\s*\n`).Split(in, -1) {
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		var list *rnode
		var para *rnode
		for _, l := range lines {
			if strings.TrimSpace(l) == "" {
				continue
			}
			if m := bulletLine.FindString(l); m != "" {
				tag := "ul"
				if strings.ContainsAny(strings.TrimSpace(m), "0123456789") {
					tag = "ol"
				}
				if list == nil || list.tag != tag {
					list = root.add(&rnode{tag: tag})
				}
				para = nil
				li := list.add(&rnode{tag: "li"})
				addLinkified(li, strings.TrimSpace(l[len(m):]))
				continue
			}
			list = nil
			if para == nil {
				para = root.add(&rnode{tag: "p"})
			} else {
				para.add(&rnode{tag: "br"})
			}
			addLinkified(para, strings.TrimSpace(l))
		}
	}
	return root
}

var plainURL = regexp.MustCompile(`https?://[^\s<>"']+`)

func addLinkified(n *rnode, s string) {
	last := 0
	for _, m := range plainURL.FindAllStringIndex(s, -1) {
		u := strings.TrimRight(s[m[0]:m[1]], ".,);:!?")
		if m[0] > last {
			n.add(&rnode{text: s[last:m[0]]})
		}
		if href, ok := SafeLink(u); ok {
			n.add(&rnode{tag: "a", href: href, kids: []*rnode{{text: u}}})
		} else {
			n.add(&rnode{text: u})
		}
		last = m[0] + len(u)
	}
	if last < len(s) {
		n.add(&rnode{text: s[last:]})
	}
}

// tidy drops empty elements and trims spaces and line breaks at the edges of
// paragraphs and items; it reports whether n has visible text.
func tidy(n *rnode) bool {
	if n.tag == "" {
		return strings.TrimSpace(n.text) != ""
	}
	if n.tag == "br" {
		return false
	}
	kept := n.kids[:0]
	any := false
	for _, k := range n.kids {
		vis := tidy(k)
		switch {
		case vis:
			any = true
			kept = append(kept, k)
		case k.tag == "br" || (k.tag == "" && k.text != ""):
			kept = append(kept, k) // spaces and breaks between words
		}
	}
	n.kids = kept
	if n.tag == "p" || n.tag == "li" {
		// trim breaks and spaces at both ends
		for len(n.kids) > 0 && (n.kids[0].tag == "br" || (n.kids[0].tag == "" && strings.TrimSpace(n.kids[0].text) == "")) {
			n.kids = n.kids[1:]
		}
		for len(n.kids) > 0 && (n.kids[len(n.kids)-1].tag == "br" || (n.kids[len(n.kids)-1].tag == "" && strings.TrimSpace(n.kids[len(n.kids)-1].text) == "")) {
			n.kids = n.kids[:len(n.kids)-1]
		}
		if len(n.kids) > 0 {
			if f := n.kids[0]; f.tag == "" {
				f.text = strings.TrimLeft(f.text, " ")
			}
			if l := n.kids[len(n.kids)-1]; l.tag == "" {
				l.text = strings.TrimRight(l.text, " ")
			}
		}
	}
	if n.tag == "ul" || n.tag == "ol" {
		// only items (a stray text run was wrapped in an item already)
		items := n.kids[:0]
		for _, k := range n.kids {
			if k.tag == "li" || k.tag == "ul" || k.tag == "ol" {
				items = append(items, k)
			}
		}
		n.kids = items
	}
	return any
}

// style decides how a tree is rendered (in the app or in an email).
type style struct {
	email  bool
	accent string
	appURL string // in-app links become absolute in emails
}

func (s style) open(n *rnode) string {
	if !s.email {
		switch n.tag {
		case "a":
			if strings.HasPrefix(n.href, "/") {
				return `<a href="` + html.EscapeString(n.href) + `">`
			}
			return `<a href="` + html.EscapeString(n.href) + `" target="_blank" rel="noopener noreferrer">`
		case "br":
			return "<br>"
		}
		return "<" + n.tag + ">"
	}
	const font = `-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif`
	switch n.tag {
	case "p":
		return `<p class="tx" style="margin:0 0 14px;font:15px/1.6 ` + font + `;color:#1a1d2e">`
	case "ul", "ol":
		return `<` + n.tag + ` class="tx" style="margin:0 0 14px;padding:0 0 0 22px;font:15px/1.6 ` + font + `;color:#1a1d2e">`
	case "li":
		return `<li style="margin:0 0 6px">`
	case "a":
		href := n.href
		if strings.HasPrefix(href, "/") && s.appURL != "" {
			href = strings.TrimRight(s.appURL, "/") + href
		}
		return `<a href="` + html.EscapeString(href) + `" style="color:` + s.accent + `;text-decoration:underline">`
	case "br":
		return "<br>"
	}
	return "<" + n.tag + ">"
}

func (s style) render(b *strings.Builder, n *rnode) {
	if n.tag == "" {
		b.WriteString(html.EscapeString(n.text))
		return
	}
	if n.tag != "root" {
		b.WriteString(s.open(n))
	}
	for _, k := range n.kids {
		s.render(b, k)
	}
	if n.tag != "root" && n.tag != "br" {
		b.WriteString("</" + n.tag + ">")
	}
}

// renderText writes the plain-text form. Links show their address when it
// differs from the text (links=true) — the email's text part.
func renderText(b *strings.Builder, n *rnode, links bool, appURL string, depth int, number *int) {
	switch n.tag {
	case "":
		b.WriteString(n.text)
		return
	case "br":
		b.WriteString("\n")
		return
	case "a":
		var inner strings.Builder
		for _, k := range n.kids {
			renderText(&inner, k, false, appURL, depth, nil)
		}
		t := inner.String()
		b.WriteString(t)
		href := n.href
		if strings.HasPrefix(href, "/") && appURL != "" {
			href = strings.TrimRight(appURL, "/") + href
		}
		if links && strings.TrimSpace(t) != href && !strings.HasPrefix(href, "mailto:") {
			b.WriteString(" (" + href + ")")
		}
		return
	case "p":
		for _, k := range n.kids {
			renderText(b, k, links, appURL, depth, nil)
		}
		b.WriteString("\n\n")
		return
	case "ul", "ol":
		num := 0
		for _, k := range n.kids {
			if n.tag == "ol" {
				renderText(b, k, links, appURL, depth+1, &num)
			} else {
				renderText(b, k, links, appURL, depth+1, nil)
			}
		}
		if depth == 0 {
			b.WriteString("\n")
		}
		return
	case "li":
		b.WriteString(strings.Repeat("  ", max(depth-1, 0)))
		if number != nil {
			*number++
			b.WriteString(itoa(*number) + ". ")
		} else {
			b.WriteString("• ")
		}
		for _, k := range n.kids {
			if k.tag == "ul" || k.tag == "ol" {
				b.WriteString("\n")
				renderText(b, k, links, appURL, depth, nil)
				continue
			}
			renderText(b, k, links, appURL, depth, nil)
		}
		if !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		return
	}
	for _, k := range n.kids {
		renderText(b, k, links, appURL, depth, nil)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

var blankLines = regexp.MustCompile(`\n{3,}`)

func parseBody(in string) *rnode {
	if len(in) > MaxBodyInput {
		in = in[:MaxBodyInput]
		for !utf8.ValidString(in) {
			in = in[:len(in)-1]
		}
	}
	var root *rnode
	if strings.Contains(in, "<") && regexp.MustCompile(`</?[a-zA-Z]`).MatchString(in) {
		root = parseHTML(in)
	} else {
		root = parsePlain(in)
	}
	tidy(root)
	return root
}

// Sanitize returns the safe HTML of a body and its plain text.
func Sanitize(in string) (safeHTML, text string) {
	root := parseBody(in)
	var b strings.Builder
	style{}.render(&b, root)
	return b.String(), PlainText(b.String(), false, "")
}

// PlainText renders (already sanitised) HTML as plain text.
func PlainText(safeHTML string, links bool, appURL string) string {
	root := parseBody(safeHTML)
	var t strings.Builder
	renderText(&t, root, links, appURL, 0, nil)
	return strings.TrimSpace(blankLines.ReplaceAllString(t.String(), "\n\n"))
}

// EmailHTML renders (already sanitised) HTML with the inline styles email
// clients need; in-app links become absolute.
func EmailHTML(safeHTML, accent, appURL string) string {
	root := parseBody(safeHTML)
	var b strings.Builder
	style{email: true, accent: accent, appURL: appURL}.render(&b, root)
	return b.String()
}
