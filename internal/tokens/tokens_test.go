package tokens

import (
	"math"
	"strings"
	"testing"
)

func TestSemanticTokensUseBrandColors(t *testing.T) {
	seen := map[string]bool{}
	for _, tok := range Semantic {
		if seen[tok.Name] {
			t.Errorf("token --%s is defined twice", tok.Name)
		}
		seen[tok.Name] = true
		if _, ok := Hex(tok.Color); !ok {
			t.Errorf("token --%s uses %q, which is not a Go brand color", tok.Name, tok.Color)
		}
	}
}

func TestTextPairsMeetAA(t *testing.T) {
	for _, pair := range TextPairs {
		fg, ok1 := Lookup(pair[0])
		bg, ok2 := Lookup(pair[1])
		if !ok1 || !ok2 {
			t.Fatalf("pair %v references an unknown token", pair)
		}
		r, err := Ratio(fg, bg)
		if err != nil {
			t.Fatal(err)
		}
		if r < 4.5 {
			t.Errorf("--%s %s on --%s %s is %.2f:1, want at least 4.5:1", pair[0], fg, pair[1], bg, r)
		}
	}
}

func TestRatio(t *testing.T) {
	r, err := Ratio("#000000", "#FFFFFF")
	if err != nil || math.Abs(r-21) > 0.001 {
		t.Fatalf("Ratio(black, white) = %v, %v; want 21", r, err)
	}
	// Gopher Blue is a fill color: too light for text on white.
	if r, _ := Ratio("#00ADD8", "#FFFFFF"); r >= 4.5 {
		t.Errorf("Gopher Blue on white = %.2f, expected to fail AA", r)
	}
	if _, err := Ratio("blue", "#FFFFFF"); err == nil {
		t.Error("Ratio accepted a non-hex color")
	}
}

func TestCSS(t *testing.T) {
	css := CSS()
	for _, want := range []string{"--go-gopher-blue: #00ADD8;", "--go-white: #FFFFFF;", "--link: var(--go-dark-blue);", "color-scheme: light;"} {
		if !strings.Contains(css, want) {
			t.Errorf("CSS is missing %q\n%s", want, css)
		}
	}
	if strings.Contains(css, "prefers-color-scheme") || strings.Contains(css, "data-theme") {
		t.Error("CSS should define a single white theme")
	}
}
