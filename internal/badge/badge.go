// Package badge draws README badges: small two-part SVG labels like
// "gopherdex | v1.2.0", in the style of shields.io, using only Go brand
// colors.
package badge

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/parthiban-sivakumar/gopherdex/internal/tokens"
)

// Style is a background and text color pair from the brand palette,
// chosen so the text has enough contrast.
type Style struct{ BG, FG string }

// The styles badges use. Each one's comment says what it marks.
var (
	Neutral = Style{"slate", "white"}       // labels, unknown values
	Blue    = Style{"gopher-blue", "black"} // versions
	Dark    = Style{"dark-blue", "white"}   // counts
	Good    = Style{"aqua", "black"}        // verified, no known issues
	Bad     = Style{"fuchsia", "white"}     // vulnerabilities
)

// Badge is a label and a value.
type Badge struct {
	Label, Value string
	Style        Style // of the value; the label is always Neutral
}

const (
	height   = 20
	padding  = 6
	fontSize = 11
)

// logoWidth is the space the Gopherdex box takes at the left of the label.
const logoWidth = 17

// logo is the Gopherdex box, scaled from its 32-unit drawing to 14px.
func logo() string {
	return fmt.Sprintf(`<g transform="translate(5 3) scale(0.4375)">`+
		`<path d="M16 2.5 28.5 9.5 16 16.5 3.5 9.5Z" fill="%s"/>`+
		`<path d="M3.5 9.5 16 16.5V30L3.5 23Z" fill="%s"/>`+
		`<path d="M28.5 9.5 16 16.5V30L28.5 23Z" fill="%s"/>`+
		`<path d="M9.75 6 22.25 13" stroke="%s" stroke-width="2" stroke-linecap="round"/></g>`,
		hex("light-blue"), hex("gopher-blue"), hex("dark-blue"), hex("white"))
}

// SVG renders the badge, with the Gopherdex box before the label. Text is
// measured with approximate Verdana widths, which is what shields.io and
// most READMEs expect.
func (b Badge) SVG() []byte {
	lw := textWidth(b.Label) + 2*padding + logoWidth
	vw := textWidth(b.Value) + 2*padding
	w := lw + vw
	title := html.EscapeString(b.Label + ": " + b.Value)
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" role="img" aria-label="%s">`, w, height, title)
	fmt.Fprintf(&sb, `<title>%s</title>`, title)
	fmt.Fprintf(&sb, `<clipPath id="r"><rect width="%d" height="%d" rx="3"/></clipPath><g clip-path="url(#r)">`, w, height)
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="%s"/>`, lw, height, hex(Neutral.BG))
	fmt.Fprintf(&sb, `<rect x="%d" width="%d" height="%d" fill="%s"/></g>`, lw, vw, height, hex(b.Style.BG))
	fmt.Fprintf(&sb, `<g font-family="Verdana,DejaVu Sans,Geneva,sans-serif" font-size="%d" text-anchor="middle">`, fontSize)
	fmt.Fprintf(&sb, `<text x="%d" y="14" fill="%s">%s</text>`, logoWidth+(lw-logoWidth)/2, hex(Neutral.FG), html.EscapeString(b.Label))
	fmt.Fprintf(&sb, `<text x="%d" y="14" fill="%s">%s</text>`, lw+vw/2, hex(b.Style.FG), html.EscapeString(b.Value))
	sb.WriteString(`</g>`)
	sb.WriteString(logo())
	sb.WriteString(`</svg>`)
	return []byte(sb.String())
}

// textWidth approximates the rendered width of s in 11px Verdana.
func textWidth(s string) int {
	w := 0.0
	for _, r := range s {
		switch {
		case strings.ContainsRune("il.,:;|!'", r):
			w += 3.4
		case strings.ContainsRune("fjrt()[]/ ", r):
			w += 4.6
		case r == 'm' || r == 'w' || r == 'M' || r == 'W':
			w += 10.2
		case r >= 'A' && r <= 'Z':
			w += 7.6
		case r >= '0' && r <= '9':
			w += 7
		case r < utf8.RuneSelf:
			w += 6.6
		default:
			w += 8 // wide characters
		}
	}
	return int(w + 0.5)
}

// Count formats a number compactly: 950, 1.2k, 34k, 5.6M.
func Count(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	}
}

// hex returns a brand color's value; the styles above only name colors
// that exist, which the tests check.
func hex(name string) string {
	h, _ := tokens.Hex(name)
	return h
}
