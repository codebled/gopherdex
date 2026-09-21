package godoc

import (
	"context"
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
