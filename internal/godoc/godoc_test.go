package godoc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

func TestExtract(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod": {Data: []byte("module example.com/hello\n")},
		"hello.go": {Data: []byte(`// Package hello returns friendly greetings.
package hello

// Greeting returns a greeting for name.
func Greeting(name string) string { return "Hello, " + name }

// Greeter builds greetings.
type Greeter struct {
	Exclaim bool
	secret  int
}

// NewGreeter returns a Greeter.
func NewGreeter() *Greeter { return &Greeter{} }

// Greet greets name.
func (g *Greeter) Greet(name string) string { return Greeting(name) }

func helper() {}
`)},
		"hello_test.go":        {Data: []byte("package hello\nfunc TestX() {}\n")},
		"wave/wave.go":         {Data: []byte("// Package wave waves.\npackage wave\n\n// Wave waves.\nfunc Wave() {}\n")},
		"testdata/skip.go":     {Data: []byte("package skip\n")},
		"tools/go.mod":         {Data: []byte("module example.com/hello/tools\n")},
		"tools/tools.go":       {Data: []byte("package tools\n")},
		"internal/bad/doc.txt": {Data: []byte("not go")},
	}

	pkgs, err := Extract(context.Background(), fsys, "example.com/hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2: %+v", len(pkgs), pkgs)
	}
	root, wave := pkgs[0], pkgs[1]
	if root.ImportPath != "example.com/hello" || root.Synopsis != "Package hello returns friendly greetings." {
		t.Errorf("root = %q / %q", root.ImportPath, root.Synopsis)
	}
	if len(root.Funcs) != 1 || root.Funcs[0].Decl != "func Greeting(name string) string" {
		t.Errorf("funcs = %+v", root.Funcs)
	}
	if len(root.Types) != 1 {
		t.Fatalf("types = %+v", root.Types)
	}
	g := root.Types[0]
	if g.Name != "Greeter" || len(g.Funcs) != 1 || len(g.Methods) != 1 || g.Methods[0].Decl != "func (g *Greeter) Greet(name string) string" {
		t.Errorf("Greeter = %+v", g)
	}
	if wave.ImportPath != "example.com/hello/wave" || wave.Name != "wave" {
		t.Errorf("wave = %+v", wave)
	}
}

func TestRichDocs(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod": {Data: []byte("module example.com/retry\n")},
		"retry.go": {Data: []byte(`// Package retry retries operations.
//
// # Policies
//
// Use a [Policy] with [Do], or see [Policy.Next] and [example.com/retry/backoff.Exp].
// The standard [net/http.Client] works too. [Evil] and [Good] links:
//
// [Evil]: javascript:alert(1)
//
// [Good]: https://example.com/guide
package retry

import (
	"errors"
	"time"
)

// Limits.
const (
	// MaxAttempts caps retries.
	MaxAttempts = 10
	minDelay    = time.Millisecond
)

// ErrGaveUp is returned when attempts run out.
var ErrGaveUp = errors.New("gave up")

// Policy decides when to retry.
type Policy struct{}

// DefaultPolicy is used by [Do].
var DefaultPolicy Policy

// Next returns the next delay.
func (Policy) Next(n int) time.Duration { return time.Duration(n) }

// Do runs fn.
func Do(fn func() error) error { return fn() }

// Forever retries without end.
//
// Deprecated: Use [Do] with a [Policy].
func Forever(fn func() error) {}
`)},
		"example_test.go": {Data: []byte(`package retry_test

import (
	"fmt"

	"example.com/retry"
)

func Example() {
	fmt.Println("package example")
	// Output: package example
}

func ExampleDo() {
	err := retry.Do(func() error { return nil })
	fmt.Println(err)
	// Output: <nil>
}

func ExamplePolicy_Next_second() {
	fmt.Println(retry.Policy{}.Next(2))
}
`)},
		"backoff/backoff.go": {Data: []byte("// Package backoff computes delays.\npackage backoff\n\n// Exp grows exponentially.\nfunc Exp() {}\n")},
	}
	pkgs, err := ExtractWith(context.Background(), fsys, "example.com/retry", Options{
		ExternalURL: func(importPath, symbol string) string { return "/ext/" + importPath + "#" + symbol },
	})
	if err != nil {
		t.Fatal(err)
	}
	p := pkgs[0]
	if p.Anchor != "pkg" || pkgs[1].Anchor != "pkg-backoff" {
		t.Errorf("anchors = %q, %q", p.Anchor, pkgs[1].Anchor)
	}
	html := string(p.DocHTML)
	for _, want := range []string{
		`<h5 id="pkg-hdr-Policies">Policies</h5>`,
		`<a href="#pkg.Policy">Policy</a>`,
		`<a href="#pkg.Do">Do</a>`,
		`<a href="#pkg.Policy.Next">Policy.Next</a>`,
		`<a href="#pkg-backoff.Exp">example.com/retry/backoff.Exp</a>`,
		`<a href="/ext/net/http#Client">net/http.Client</a>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("package doc HTML lacks %s:\n%s", want, html)
		}
	}
	if !strings.Contains(html, `<a rel="nofollow ugc noopener" href="https://example.com/guide">Good</a>`) {
		t.Errorf("link definition not rendered:\n%s", html)
	}
	if strings.Contains(html, `href="javascript`) {
		t.Errorf("javascript: link survived:\n%s", html)
	}
	if len(p.Consts) != 1 || !strings.Contains(p.Consts[0].Decl, "MaxAttempts = 10") || strings.Contains(p.Consts[0].Decl, "minDelay") {
		t.Errorf("consts = %+v", p.Consts)
	}
	if len(p.Vars) != 1 || p.Vars[0].Names[0] != "ErrGaveUp" || p.Vars[0].Anchor != "pkg.ErrGaveUp" {
		t.Errorf("vars = %+v", p.Vars)
	}
	if len(p.Types) != 1 || len(p.Types[0].Vars) != 1 || p.Types[0].Vars[0].Names[0] != "DefaultPolicy" {
		t.Fatalf("types = %+v", p.Types)
	}
	next := p.Types[0].Methods[0]
	if next.Anchor != "pkg.Policy.Next" || next.Pos != (Pos{"retry.go", 35}) {
		t.Errorf("Next: anchor %q pos %+v", next.Anchor, next.Pos)
	}
	if len(next.Examples) != 1 || next.Examples[0].Suffix != "second" || next.Examples[0].Anchor != "pkg.example-Policy.Next-second" || next.Examples[0].Name != "Policy.Next" {
		t.Errorf("method examples = %+v", next.Examples)
	}
	var do, forever Symbol
	for _, f := range p.Funcs {
		switch f.Name {
		case "Do":
			do = f
		case "Forever":
			forever = f
		}
	}
	if !forever.Deprecated || do.Deprecated {
		t.Errorf("deprecated: Forever %v, Do %v", forever.Deprecated, do.Deprecated)
	}
	if len(do.Examples) != 1 || do.Examples[0].Output != "<nil>\n" || !strings.HasPrefix(do.Examples[0].Code, "err := retry.Do(") {
		t.Errorf("Do examples = %+v", do.Examples)
	}
	if !strings.Contains(do.Examples[0].Play, "package main") || !strings.Contains(do.Examples[0].Play, `"example.com/retry"`) {
		t.Errorf("playground program:\n%s", do.Examples[0].Play)
	}
	if len(p.Examples) != 1 || p.Examples[0].Output != "package example\n" {
		t.Errorf("package examples = %+v", p.Examples)
	}
	if strings.Join(p.Imports, ",") != "errors,time" || strings.Join(p.Files, ",") != "retry.go" {
		t.Errorf("imports %v files %v", p.Imports, p.Files)
	}
}

func TestExtractBudget(t *testing.T) {
	// One directory with far more source than the budget: documented
	// partly, without parsing it all.
	fsys := fstest.MapFS{"go.mod": {Data: []byte("module example.com/big\n")}}
	line := "var _ = 1\n"
	big := "// Package big is large.\npackage big\n\n" + strings.Repeat(line, (900<<10)/len(line))
	for i := range 20 {
		fsys[fmt.Sprintf("f%02d.go", i)] = &fstest.MapFile{Data: []byte(big)}
	}
	fsys["api.go"] = &fstest.MapFile{Data: []byte("package big\n\n// Hello says hi.\nfunc Hello() {}\n")}
	pkgs, err := Extract(context.Background(), fsys, "example.com/big")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || !pkgs[0].Partial || len(pkgs[0].Files) > 3 {
		t.Fatalf("got %d packages, partial %v, %d files", len(pkgs), len(pkgs) > 0 && pkgs[0].Partial, len(pkgs[0].Files))
	}
	if len(pkgs[0].Funcs) != 1 || pkgs[0].Funcs[0].Name != "Hello" {
		t.Errorf("small files within the budget should still be documented: %+v", pkgs[0].Funcs)
	}
}
