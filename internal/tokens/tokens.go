// Package tokens is the design system's single source of truth. The site
// uses the official Go brand colors and nothing else, on a white theme. The
// server renders the tokens into /tokens.css.
package tokens

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Color is a named brand color.
type Color struct {
	Name string
	Hex  string
}

// Token is a semantic name for one brand color.
type Token struct {
	Name  string
	Color string // name of a Brand color
}

// Brand is the Go brand palette: Gopher Blue, Light Blue, Aqua and Black as
// primaries, Fuchsia, Yellow and Slate as secondaries, and Dark Blue, the
// text-safe blue go.dev uses for links.
var Brand = []Color{
	{"gopher-blue", "#00ADD8"},
	{"light-blue", "#5DC9E2"},
	{"aqua", "#00A29C"},
	{"dark-blue", "#007D9C"},
	{"fuchsia", "#CE3262"},
	{"yellow", "#FDDD00"},
	{"slate", "#555759"},
	{"black", "#000000"},
	{"white", "#FFFFFF"},
}

// Tints are official colors mixed with white, for quiet surfaces and
// borders: a light slate reads as neutral grey, and a pale blue as a
// selected row. Each is exactly Percent of Brand over white, so the page
// still uses nothing but the Go palette.
type Tint struct {
	Name    string
	Brand   string
	Percent int
}

var Tints = []Tint{
	{"slate-50", "slate", 3},      // code blocks, sidebars
	{"slate-100", "slate", 10},    // hover
	{"slate-200", "slate", 18},    // borders
	{"slate-300", "slate", 34},    // input borders, strong lines
	{"blue-50", "gopher-blue", 5}, // selected, info
	{"blue-100", "gopher-blue", 18},
	{"aqua-50", "aqua", 10},      // success notices
	{"fuchsia-50", "fuchsia", 5}, // danger notices
	{"yellow-100", "yellow", 30}, // highlights
}

// Semantic tokens are what CSS uses. Every token points at a Brand color
// or a Tint, so the page cannot drift outside the palette.
//
// The theme is quiet: black text on white, slate for secondary text and
// lines, and blue only where it means something (links, the primary
// action, focus, the logo). Gopher Blue is too light for text on white,
// so text and buttons use Dark Blue.
var Semantic = []Token{
	{"bg", "white"},
	{"surface", "white"},
	{"surface-muted", "slate-50"},
	{"hover", "slate-100"},
	{"text", "black"},
	{"text-muted", "slate"},
	{"line", "slate-200"},
	{"line-strong", "slate-300"},
	{"primary", "dark-blue"},
	{"on-primary", "white"},
	{"primary-hover", "black"},
	{"accent", "gopher-blue"},
	{"accent-soft", "blue-50"},
	{"accent-line", "blue-100"},
	{"link", "dark-blue"},
	{"focus", "gopher-blue"},
	{"success", "aqua"},
	{"on-success", "black"},
	{"success-soft", "aqua-50"},
	{"highlight", "yellow-100"},
	{"on-highlight", "black"},
	{"danger", "fuchsia"},
	{"on-danger", "white"},
	{"danger-soft", "fuchsia-50"},
	{"code-bg", "slate-50"},
	{"code-text", "black"},
	{"code-accent", "slate"},
	{"code-prompt", "dark-blue"},
	{"code-ok", "dark-blue"},
	{"code-string", "fuchsia"},
}

// TextPairs are foreground/background tokens used together for text. Tests
// require each to reach WCAG AA (4.5:1).
var TextPairs = [][2]string{
	{"text", "bg"}, {"text-muted", "bg"}, {"link", "bg"}, {"danger", "bg"},
	{"text", "surface-muted"}, {"text-muted", "surface-muted"}, {"link", "surface-muted"},
	{"text", "hover"}, {"text", "accent-soft"}, {"link", "accent-soft"},
	{"on-primary", "primary"}, {"on-primary", "primary-hover"}, {"on-success", "success"},
	{"on-highlight", "highlight"}, {"on-danger", "danger"},
	{"text", "success-soft"}, {"text", "danger-soft"}, {"danger", "danger-soft"},
	{"text", "line"},
	{"code-text", "code-bg"}, {"code-accent", "code-bg"}, {"code-prompt", "code-bg"},
	{"code-ok", "code-bg"}, {"code-string", "code-bg"},
}

// Hex returns the value of a brand color or tint.
func Hex(name string) (string, bool) {
	for _, c := range Brand {
		if c.Name == name {
			return c.Hex, true
		}
	}
	for _, t := range Tints {
		if t.Name == name {
			base, ok := Hex(t.Brand)
			if !ok {
				return "", false
			}
			return mix(base, t.Percent), true
		}
	}
	return "", false
}

// mix returns percent of hex over white, as "#RRGGBB".
func mix(hex string, percent int) string {
	n, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	ch := func(shift uint) int {
		c := float64((n >> shift) & 0xFF)
		return int(math.Round(c*float64(percent)/100 + 255*float64(100-percent)/100))
	}
	return fmt.Sprintf("#%02X%02X%02X", ch(16), ch(8), ch(0))
}

// Lookup returns the value of a semantic token.
func Lookup(token string) (string, bool) {
	for _, t := range Semantic {
		if t.Name == token {
			return Hex(t.Color)
		}
	}
	return "", false
}

// CSS renders the palette and semantic tokens as custom properties.
func CSS() string {
	var b strings.Builder
	b.WriteString("/* Generated from internal/tokens. Do not edit. */\n:root {\n  color-scheme: light;\n")
	for _, c := range Brand {
		fmt.Fprintf(&b, "  --go-%s: %s;\n", c.Name, c.Hex)
	}
	for _, t := range Tints {
		h, _ := Hex(t.Name)
		fmt.Fprintf(&b, "  --go-%s: %s; /* %d%% %s */\n", t.Name, h, t.Percent, t.Brand)
	}
	for _, t := range Semantic {
		fmt.Fprintf(&b, "  --%s: var(--go-%s);\n", t.Name, t.Color)
	}
	b.WriteString("}\n")
	return b.String()
}

// Ratio returns the WCAG 2.x contrast ratio of two "#RRGGBB" colors.
func Ratio(a, b string) (float64, error) {
	la, err := luminance(a)
	if err != nil {
		return 0, err
	}
	lb, err := luminance(b)
	if err != nil {
		return 0, err
	}
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05), nil
}

func luminance(hex string) (float64, error) {
	h, ok := strings.CutPrefix(hex, "#")
	if !ok || len(h) != 6 {
		return 0, errors.New("color " + strconv.Quote(hex) + " is not #RRGGBB")
	}
	n, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("color %q: %w", hex, err)
	}
	channel := func(shift uint) float64 {
		c := float64((n>>shift)&0xFF) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(16) + 0.7152*channel(8) + 0.0722*channel(0), nil
}
