package branding

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func pngBytes(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 40, 20)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A minimal lossless WebP header (VP8L) for a 300×100 image.
func webpBytes() []byte {
	bits := uint32(300-1) | uint32(100-1)<<14
	b := []byte("RIFF\x00\x00\x00\x00WEBPVP8L\x00\x00\x00\x00\x2f")
	b = append(b, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))
	return append(b, make([]byte, 16)...)
}

func TestValidateTypes(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		ct   string
		w, h int
	}{
		{"png", pngBytes(t, 120, 40, color.Black), TypePNG, 120, 40},
		{"jpeg", jpegBytes(t), TypeJPEG, 40, 20},
		{"webp", webpBytes(), TypeWebP, 300, 100},
		{"svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 50"><rect width="200" height="50" fill="#123456"/></svg>`), TypeSVG, 200, 50},
		{"svg-bom-size", []byte("\xef\xbb\xbf  <svg xmlns='http://www.w3.org/2000/svg' width='64px' height='32'><circle r='4'/></svg>"), TypeSVG, 64, 32},
	}
	for _, tc := range cases {
		got, err := Validate(tc.data)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
		if got.ContentType != tc.ct || got.Width != tc.w || got.Height != tc.h || len(got.ETag) != 20 {
			t.Fatalf("%s: got %+v", tc.name, got)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	big := append(pngBytes(t, 2, 2, color.White), make([]byte, MaxLogoBytes)...)
	fakePNG := append([]byte("\x89PNG\r\n\x1a\n"), []byte("not really a png")...)
	cases := map[string][]byte{
		"empty":    {},
		"too big":  big,
		"text":     []byte("hello world"),
		"gif":      []byte("GIF89a\x01\x00\x01\x00"),
		"html":     []byte("<html><body>hi</body></html>"),
		"fake png": fakePNG,
	}
	for name, data := range cases {
		if _, err := Validate(data); err == nil || !IsValidation(err) {
			t.Fatalf("%s: expected a validation error, got %v", name, err)
		}
	}
	if _, err := Validate(big); err == nil || !strings.Contains(err.Error(), "1 MB") {
		t.Fatalf("size error should mention 1 MB: %v", err)
	}
}

func TestSVGSanitising(t *testing.T) {
	const ns = `xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"`
	bad := map[string]string{
		"script":            `<svg ` + ns + `><script>alert(1)</script></svg>`,
		"script uppercase":  `<svg ` + ns + `><SCRIPT>alert(1)</SCRIPT></svg>`,
		"foreignObject":     `<svg ` + ns + `><foreignObject><div xmlns="http://www.w3.org/1999/xhtml">x</div></foreignObject></svg>`,
		"onload":            `<svg ` + ns + ` onload="alert(1)"><rect/></svg>`,
		"onclick nested":    `<svg ` + ns + `><g><rect onClick="x()"/></g></svg>`,
		"javascript href":   `<svg ` + ns + `><a href="javascript:alert(1)"><rect/></a></svg>`,
		"javascript spaced": `<svg ` + ns + `><a xlink:href="java&#x09;script:alert(1)"><rect/></a></svg>`,
		"external image":    `<svg ` + ns + `><image href="https://evil.example/x.png"/></svg>`,
		"external use":      `<svg ` + ns + `><use xlink:href="http://evil.example/s.svg#a"/></svg>`,
		"css import":        `<svg ` + ns + `><style>@import url(https://evil.example/a.css);</style></svg>`,
		"css url":           `<svg ` + ns + `><rect style="fill:url(https://evil.example/p)"/></svg>`,
		"entity":            `<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY x "y">]><svg ` + ns + `>&x;</svg>`,
		"stylesheet PI":     `<?xml-stylesheet href="https://evil.example/a.css"?><svg ` + ns + `/>`,
		"iframe":            `<svg ` + ns + `><iframe src="https://evil.example"/></svg>`,
		"not svg root":      `<html><svg ` + ns + `/></html>`,
		"broken xml":        `<svg ` + ns + `><rect></svg>`,
	}
	for name, svg := range bad {
		if _, err := Validate([]byte(svg)); err == nil {
			t.Fatalf("%s: unsafe SVG was accepted", name)
		}
	}
	good := []string{
		`<svg ` + ns + `><defs><linearGradient id="g"/></defs><rect fill="url(#g)" width="10" height="10"/><use href="#g"/></svg>`,
		`<svg ` + ns + `><image href="data:image/png;base64,iVBORw0KGgo="/><style>.a{fill:red}</style></svg>`,
		`<svg ` + ns + `><text>Only &amp; ascii entities</text></svg>`,
	}
	for i, svg := range good {
		if _, err := Validate([]byte(svg)); err != nil {
			t.Fatalf("good SVG #%d rejected: %v", i, err)
		}
	}
}

func TestDecodeDataURI(t *testing.T) {
	p := pngBytes(t, 3, 3, color.White)
	got, err := DecodeDataURI("data:image/png;base64," + base64.StdEncoding.EncodeToString(p))
	if err != nil || !bytes.Equal(got, p) {
		t.Fatalf("base64 data URI: %v", err)
	}
	// declared type is ignored — the bytes decide
	got, _ = DecodeDataURI("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(p))
	if Detect(got) != TypePNG {
		t.Fatal("declared type should not matter")
	}
	got, err = DecodeDataURI("data:image/svg+xml,%3Csvg%20xmlns%3D%22http%3A%2F%2Fwww.w3.org%2F2000%2Fsvg%22%2F%3E")
	if err != nil || Detect(got) != TypeSVG {
		t.Fatalf("url-encoded svg: %q %v", got, err)
	}
	if _, err := DecodeDataURI("data:image/png;base64,@@@"); err == nil {
		t.Fatal("bad base64 should fail")
	}
}

func TestETagMatches(t *testing.T) {
	if !etagMatches(`"abc"`, "abc") || !etagMatches(`W/"abc"`, "abc") || !etagMatches(`"x", "abc"`, "abc") || !etagMatches("*", "abc") {
		t.Fatal("expected matches")
	}
	if etagMatches(`"abd"`, "abc") {
		t.Fatal("unexpected match")
	}
}
