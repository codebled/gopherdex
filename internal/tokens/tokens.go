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

// Semantic tokens are what CSS uses. Every token points at a Brand color, so
// the page cannot drift outside the palette.
//
// Gopher Blue and Light Blue are too light for text on white; they are used
// for fills and lines, with black text on top. Blue text uses Dark Blue.
var Semantic = []Token{
	{"bg", "white"},
	{"surface", "white"},
	{"text", "black"},
	{"text-muted", "slate"},
	{"line", "light-blue"},
	{"primary", "gopher-blue"},
	{"on-primary", "black"},
	{"link", "dark-blue"},
	{"focus", "dark-blue"},
	{"success", "aqua"},
	{"on-success", "black"},
	{"highlight", "yellow"},
	{"on-highlight", "black"},
	{"danger", "fuchsia"},
	{"on-danger", "white"},
	{"code-bg", "black"},
	{"code-text", "white"},
	{"code-accent", "light-blue"},
	{"code-prompt", "gopher-blue"},
	{"code-ok", "aqua"},
	{"code-string", "yellow"},
}

// TextPairs are foreground/background tokens used together for text. Tests
// require each to reach WCAG AA (4.5:1).
var TextPairs = [][2]string{
	{"text", "bg"}, {"text-muted", "bg"}, {"link", "bg"}, {"danger", "bg"},
	{"on-primary", "primary"}, {"on-success", "success"},
	{"on-highlight", "highlight"}, {"on-danger", "danger"},
	{"text", "line"},
	{"code-text", "code-bg"}, {"code-accent", "code-bg"}, {"code-prompt", "code-bg"},
	{"code-ok", "code-bg"}, {"code-string", "code-bg"},
}

// Hex returns the value of a brand color.
func Hex(brand string) (string, bool) {
	for _, c := range Brand {
		if c.Name == brand {
			return c.Hex, true
		}
	}
	return "", false
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
