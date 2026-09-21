// Package godoc extracts package documentation from module source using
// go/parser, go/doc and go/doc/comment, the same packages pkg.go.dev is
// built on.
package godoc

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/doc"
	"go/doc/comment"
	"go/format"
	"go/parser"
	"go/token"
	"html/template"
	"io/fs"
	"maps"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
)

const maxFileSize = 1 << 20

// Package is the documentation of one package.
type Package struct {
	ImportPath string    `json:"importPath"`
	Name       string    `json:"name"`
	Synopsis   string    `json:"synopsis"`
	Doc        string    `json:"doc"`
	Deprecated bool      `json:"deprecated,omitempty"`
	Consts     []Value   `json:"consts"`
	Vars       []Value   `json:"vars"`
	Funcs      []Symbol  `json:"funcs"`
	Types      []Type    `json:"types"`
	Examples   []Example `json:"examples,omitempty"` // package-level examples
	Imports    []string  `json:"imports"`
	Files      []string  `json:"files"` // source files, relative to the module root

	Anchor  string        `json:"-"` // HTML id of the package's section
	DocHTML template.HTML `json:"-"`
}

// Pos is where a declaration starts, relative to the module root.
type Pos struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// Symbol is a documented function, method or type.
type Symbol struct {
	Name       string    `json:"name"`
	Decl       string    `json:"decl"`
	Doc        string    `json:"doc"`
	Pos        Pos       `json:"pos"`
	Deprecated bool      `json:"deprecated,omitempty"`
	Examples   []Example `json:"examples,omitempty"`

	Anchor  string        `json:"-"`
	DocHTML template.HTML `json:"-"`
}

// Value is a group of constants or variables declared together.
type Value struct {
	Names      []string `json:"names"`
	Decl       string   `json:"decl"`
	Doc        string   `json:"doc"`
	Pos        Pos      `json:"pos"`
	Deprecated bool     `json:"deprecated,omitempty"`

	Anchor  string        `json:"-"` // of the first name
	DocHTML template.HTML `json:"-"`
}

// Type is a documented type with its constants, variables, constructors
// and methods.
type Type struct {
	Symbol
	Consts  []Value  `json:"consts"`
	Vars    []Value  `json:"vars"`
	Funcs   []Symbol `json:"funcs"`
	Methods []Symbol `json:"methods"`
}

// Example is a testable example from a _test.go file.
type Example struct {
	Name   string `json:"name"`   // "", "Do", "Client.Get", with "_suffix" for variants
	Suffix string `json:"suffix"` // "second" in ExampleDo_second
	Doc    string `json:"doc"`
	Code   string `json:"code"`
	Output string `json:"output,omitempty"`
	// Play is a complete program for the Go Playground, when the example
	// can run on its own.
	Play string `json:"play,omitempty"`

	Anchor string `json:"-"`
}

// Options control how documentation links are rendered.
type Options struct {
	// ExternalURL returns the page for a symbol in a package outside the
	// module (symbol "" for the package). Default: pkg.go.dev.
	ExternalURL func(importPath, symbol string) string
}

// PackageAnchor is the HTML id of a package's section on the docs page.
func PackageAnchor(modulePath, importPath string) string {
	rel := strings.TrimPrefix(strings.TrimPrefix(importPath, modulePath), "/")
	if rel == "" {
		return "pkg"
	}
	return "pkg-" + strings.ReplaceAll(rel, "/", "-")
}

// SymbolAnchor is the HTML id of a symbol, e.g. "pkg-backoff.Policy.Next".
func SymbolAnchor(modulePath, importPath, symbol string) string {
	a := PackageAnchor(modulePath, importPath)
	if symbol != "" {
		a += "." + symbol
	}
	return a
}

// Extract documents every package in fsys, the source tree of modulePath,
// with default options.
func Extract(ctx context.Context, fsys fs.FS, modulePath string) ([]Package, error) {
	return ExtractWith(ctx, fsys, modulePath, Options{})
}

// ExtractWith documents every package in fsys, the source tree of
// modulePath. testdata, vendor, hidden directories and nested modules are
// skipped; _test.go files contribute examples only. Only exported
// identifiers are included.
func ExtractWith(ctx context.Context, fsys fs.FS, modulePath string, opts Options) ([]Package, error) {
	if opts.ExternalURL == nil {
		opts.ExternalURL = func(importPath, symbol string) string {
			u := "https://pkg.go.dev/" + importPath
			if symbol != "" {
				u += "#" + symbol
			}
			return u
		}
	}
	dirs := map[string][]string{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if p == "." {
				return nil
			}
			name := d.Name()
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return fs.SkipDir
			}
			if _, err := fs.Stat(fsys, path.Join(p, "go.mod")); err == nil {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && strings.HasSuffix(p, ".go") {
			dirs[path.Dir(p)] = append(dirs[path.Dir(p)], p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan source of %s: %w", modulePath, err)
	}

	// Which import paths are packages of this module, for links.
	local := map[string]bool{}
	for dir, names := range dirs {
		if slices.ContainsFunc(names, func(n string) bool { return !strings.HasSuffix(n, "_test.go") }) {
			local[joinPath(modulePath, dir)] = true
		}
	}

	pkgs := []Package{}
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		if !local[joinPath(modulePath, dir)] {
			continue // only tests here
		}
		pkg, err := extractDir(fsys, modulePath, dir, dirs[dir], local, opts)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

func joinPath(modulePath, dir string) string {
	if dir == "." {
		return modulePath
	}
	return modulePath + "/" + dir
}

func extractDir(fsys fs.FS, modulePath, dir string, names []string, local map[string]bool, opts Options) (Package, error) {
	fset := token.NewFileSet()
	byName := map[string][]*ast.File{}
	tests := map[string][]*ast.File{}
	for _, name := range names {
		st, err := fs.Stat(fsys, name)
		if err != nil {
			return Package{}, fmt.Errorf("stat %s: %w", name, err)
		}
		if st.Size() > maxFileSize {
			continue // generated tables and the like; not worth documenting
		}
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return Package{}, fmt.Errorf("read %s: %w", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			if strings.HasSuffix(name, "_test.go") {
				continue // a broken test file shouldn't hide the docs
			}
			return Package{}, fmt.Errorf("parse %s: %w", name, err)
		}
		if strings.HasSuffix(name, "_test.go") {
			tests[strings.TrimSuffix(f.Name.Name, "_test")] = append(tests[strings.TrimSuffix(f.Name.Name, "_test")], f)
		} else {
			byName[f.Name.Name] = append(byName[f.Name.Name], f)
		}
	}

	// A directory can mix package names, e.g. a "package main" generator
	// behind a build tag. Document the name with the most files.
	var pkgName string
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		if len(byName[n]) > len(byName[pkgName]) {
			pkgName = n
		}
	}

	importPath := joinPath(modulePath, dir)
	files := append(slices.Clone(byName[pkgName]), tests[pkgName]...)
	p, err := doc.NewFromFiles(fset, files, importPath)
	if err != nil {
		return Package{}, fmt.Errorf("document %s: %w", importPath, err)
	}

	r := &renderer{fset: fset, p: p, modulePath: modulePath, importPath: importPath, local: local, opts: opts}
	out := Package{
		ImportPath: importPath,
		Name:       p.Name,
		Synopsis:   p.Synopsis(p.Doc),
		Doc:        p.Doc,
		Deprecated: deprecated(p.Doc),
		Consts:     r.values(p.Consts),
		Vars:       r.values(p.Vars),
		Funcs:      r.funcs(p.Funcs, ""),
		Types:      []Type{},
		Examples:   r.examples(p.Examples),
		Imports:    []string{},
		Files:      []string{},
		Anchor:     PackageAnchor(modulePath, importPath),
		DocHTML:    r.html(p.Doc, PackageAnchor(modulePath, importPath)),
	}
	imports := map[string]bool{}
	for _, f := range byName[pkgName] {
		out.Files = append(out.Files, fset.File(f.Pos()).Name())
		for _, imp := range f.Imports {
			if s, err := strconv.Unquote(imp.Path.Value); err == nil {
				imports[s] = true
			}
		}
	}
	slices.Sort(out.Files)
	out.Imports = slices.Sorted(maps.Keys(imports))
	for _, t := range p.Types {
		decl := *t.Decl
		decl.Doc = nil
		anchor := SymbolAnchor(modulePath, importPath, t.Name)
		out.Types = append(out.Types, Type{
			Symbol: Symbol{Name: t.Name, Decl: r.print(&decl), Doc: t.Doc, Pos: r.pos(t.Decl), Deprecated: deprecated(t.Doc),
				Examples: r.examples(t.Examples), Anchor: anchor, DocHTML: r.html(t.Doc, anchor)},
			Consts:  r.values(t.Consts),
			Vars:    r.values(t.Vars),
			Funcs:   r.funcs(t.Funcs, ""),
			Methods: r.funcs(t.Methods, t.Name),
		})
	}
	return out, nil
}

// renderer turns one package's doc.Package into Symbols with rendered HTML.
type renderer struct {
	fset       *token.FileSet
	p          *doc.Package
	modulePath string
	importPath string
	local      map[string]bool
	opts       Options
}

func (r *renderer) pos(n ast.Node) Pos {
	pos := r.fset.Position(n.Pos())
	return Pos{File: pos.Filename, Line: pos.Line}
}

func (r *renderer) print(node any) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, r.fset, node); err != nil {
		return ""
	}
	return buf.String()
}

func (r *renderer) funcs(fns []*doc.Func, recv string) []Symbol {
	out := make([]Symbol, 0, len(fns))
	for _, f := range fns {
		decl := *f.Decl
		decl.Body, decl.Doc = nil, nil
		name := f.Name
		if recv != "" {
			name = recv + "." + f.Name
		}
		anchor := SymbolAnchor(r.modulePath, r.importPath, name)
		out = append(out, Symbol{Name: f.Name, Decl: r.print(&decl), Doc: f.Doc, Pos: r.pos(f.Decl), Deprecated: deprecated(f.Doc),
			Examples: r.examples(f.Examples), Anchor: anchor, DocHTML: r.html(f.Doc, anchor)})
	}
	return out
}

func (r *renderer) values(vals []*doc.Value) []Value {
	out := make([]Value, 0, len(vals))
	for _, v := range vals {
		decl := *v.Decl
		decl.Doc = nil
		anchor := ""
		if len(v.Names) > 0 {
			anchor = SymbolAnchor(r.modulePath, r.importPath, v.Names[0])
		}
		out = append(out, Value{Names: v.Names, Decl: r.print(&decl), Doc: v.Doc, Pos: r.pos(v.Decl), Deprecated: deprecated(v.Doc),
			Anchor: anchor, DocHTML: r.html(v.Doc, anchor)})
	}
	return out
}

func (r *renderer) examples(exs []*doc.Example) []Example {
	var out []Example
	for _, ex := range exs {
		// go/doc names ExamplePolicy_Next_second "Policy_Next_second" with
		// suffix "second"; show it as Policy.Next, as pkg.go.dev does.
		base := ex.Name
		if ex.Suffix != "" {
			base = strings.TrimSuffix(base, "_"+ex.Suffix)
		}
		base = strings.Replace(base, "_", ".", 1)
		e := Example{Name: base, Suffix: ex.Suffix, Doc: ex.Doc, Output: ex.Output}
		if b, ok := ex.Code.(*ast.BlockStmt); ok {
			e.Code = r.blockBody(b)
		} else {
			e.Code = r.print(ex.Code)
		}
		if ex.Play != nil {
			e.Play = r.print(ex.Play)
		}
		id := "example-" + base
		if ex.Suffix != "" {
			id += "-" + ex.Suffix
		}
		e.Anchor = SymbolAnchor(r.modulePath, r.importPath, id)
		out = append(out, e)
	}
	return out
}

// blockBody prints the statements of an example function without the
// braces, unindented, as pkg.go.dev shows them.
func (r *renderer) blockBody(b *ast.BlockStmt) string {
	src := r.print(b)
	src = strings.TrimSpace(src)
	src = strings.TrimPrefix(src, "{")
	src = strings.TrimSuffix(src, "}")
	lines := strings.Split(strings.Trim(src, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "\t")
	}
	return strings.Join(lines, "\n")
}

// html renders a doc comment: headings, lists, code blocks, URLs and
// [Name] links to other declarations.
func (r *renderer) html(text, anchor string) template.HTML {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	d := r.p.Parser().Parse(text)
	safeLinks(d)
	pr := r.p.Printer()
	pr.HeadingLevel = 5
	pr.HeadingID = func(h *comment.Heading) string { return anchor + "-hdr-" + strings.TrimPrefix(h.DefaultID(), "hdr-") }
	pr.DocLinkURL = func(link *comment.DocLink) string {
		importPath := link.ImportPath
		if importPath == "" {
			importPath = r.importPath
		}
		symbol := link.Name
		if link.Recv != "" {
			symbol = link.Recv + "." + link.Name
		}
		if r.local[importPath] {
			return "#" + SymbolAnchor(r.modulePath, importPath, symbol)
		}
		return r.opts.ExternalURL(importPath, symbol)
	}
	out := string(pr.HTML(d))
	// Links in comments are the author's, not ours.
	out = strings.ReplaceAll(out, `<a href="http`, `<a rel="nofollow ugc noopener" href="http`)
	return template.HTML(out)
}

// safeLinks replaces link targets that aren't http(s) (javascript:,
// data:…) so a doc comment can't smuggle script into the page.
func safeLinks(d *comment.Doc) {
	ok := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && (u.Scheme == "http" || u.Scheme == "https")
	}
	for _, def := range d.Links {
		if !ok(def.URL) {
			def.URL = "#"
		}
	}
	var fixText func([]comment.Text)
	fixText = func(ts []comment.Text) {
		for _, t := range ts {
			switch t := t.(type) {
			case *comment.Link:
				if !ok(t.URL) {
					t.URL = "#"
				}
				fixText(t.Text)
			case *comment.DocLink:
				fixText(t.Text)
			}
		}
	}
	var fixBlocks func([]comment.Block)
	fixBlocks = func(bs []comment.Block) {
		for _, b := range bs {
			switch b := b.(type) {
			case *comment.Paragraph:
				fixText(b.Text)
			case *comment.Heading:
				fixText(b.Text)
			case *comment.List:
				for _, item := range b.Items {
					fixBlocks(item.Content)
				}
			}
		}
	}
	fixBlocks(d.Content)
}

// deprecated reports whether a doc comment has a "Deprecated:" paragraph,
// the Go convention that gopls and pkg.go.dev recognize.
func deprecated(text string) bool {
	for _, para := range strings.Split(text, "\n\n") {
		if strings.HasPrefix(strings.TrimSpace(para), "Deprecated: ") {
			return true
		}
	}
	return false
}
