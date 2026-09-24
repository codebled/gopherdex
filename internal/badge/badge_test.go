package badge

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/codebled/gopherdex/internal/tokens"
)

func TestSVG(t *testing.T) {
	svg := string(Badge{Label: "gopherdex", Value: "v1.2.0 <&>", Style: Blue}.SVG())
	if err := xml.Unmarshal([]byte(svg), new(struct{})); err != nil {
		t.Fatalf("not well-formed XML: %v\n%s", err, svg)
	}
	for _, want := range []string{`aria-label="gopherdex: v1.2.0 &lt;&amp;&gt;"`, `fill="#555759"`, `fill="#00ADD8"`, `>v1.2.0 &lt;&amp;&gt;</text>`} {
		if !strings.Contains(svg, want) {
			t.Errorf("badge lacks %s:\n%s", want, svg)
		}
	}
	// Every style is in the brand palette.
	for _, s := range []Style{Neutral, Blue, Dark, Good, Bad} {
		if _, ok := tokens.Hex(s.BG); !ok {
			t.Errorf("style %+v isn't in the palette", s)
		}
		if _, ok := tokens.Hex(s.FG); !ok {
			t.Errorf("style %+v isn't in the palette", s)
		}
	}
	if textWidth("downloads") <= textWidth("v1") {
		t.Error("width estimate is off")
	}
}

func TestCount(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1k", 1234: "1.2k", 45_678: "45k", 2_500_000: "2.5M"} {
		if got := Count(n); got != want {
			t.Errorf("Count(%d) = %q, want %q", n, got, want)
		}
	}
}
