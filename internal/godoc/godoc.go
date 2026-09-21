// Package godoc extracts package documentation from module source using
// go/parser and go/doc, the same packages pkg.go.dev is built on.
package godoc

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/doc"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
)

const maxFileSize = 1 << 20

// Package is the documentation of one package.
type Package struct {
	ImportPath string   `json:"importPath"`
	Name       string   `json:"name"`
	Synopsis   string   `json:"synopsis"`
	Doc        string   `json:"doc"`
	Funcs      []Symbol `json:"funcs"`
	Types      []Type   `json:"types"`
}

// Symbol is a documented declaration.
type Symbol struct {
	Name string `json:"name"`
	Decl string `json:"decl"`
	Doc  string `json:"doc"`
}

// Type is a documented type with its constructors and methods.
type Type struct {
	Symbol
	Funcs   []Symbol `json:"funcs"`
	Methods []Symbol `json:"methods"`
}

// Extract documents every package in fsys, the source tree of modulePath.
// Test files, testdata, vendor, hidden directories and nested modules are
// skipped. Only exported identifiers are included.
func Extract(ctx context.Context, fsys fs.FS, modulePath string) ([]Package, error) {
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
		if d.Type().IsRegular() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			dirs[path.Dir(p)] = append(dirs[path.Dir(p)], p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan source of %s: %w", modulePath, err)
	}

	pkgs := []Package{}
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		pkg, err := extractDir(fsys, modulePath, dir, dirs[dir])
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

func extractDir(fsys fs.FS, modulePath, dir string, names []string) (Package, error) {
	fset := token.NewFileSet()
	byName := map[string][]*ast.File{}
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
			return Package{}, fmt.Errorf("parse %s: %w", name, err)
		}
		byName[f.Name.Name] = append(byName[f.Name.Name], f)
	}

	// A directory can mix package names, e.g. a "package main" generator
	// behind a build tag. Document the name with the most files.
	var pkgName string
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		if len(byName[n]) > len(byName[pkgName]) {
			pkgName = n
		}
	}

	importPath := modulePath
	if dir != "." {
		importPath += "/" + dir
	}
	p, err := doc.NewFromFiles(fset, byName[pkgName], importPath)
	if err != nil {
		return Package{}, fmt.Errorf("document %s: %w", importPath, err)
	}

	out := Package{
		ImportPath: importPath,
		Name:       p.Name,
		Synopsis:   p.Synopsis(p.Doc),
		Doc:        p.Doc,
		Funcs:      funcs(fset, p.Funcs),
		Types:      []Type{},
	}
	for _, t := range p.Types {
		decl := *t.Decl
		decl.Doc = nil
		out.Types = append(out.Types, Type{
			Symbol:  Symbol{Name: t.Name, Decl: printNode(fset, &decl), Doc: t.Doc},
			Funcs:   funcs(fset, t.Funcs),
			Methods: funcs(fset, t.Methods),
		})
	}
	return out, nil
}

func funcs(fset *token.FileSet, fns []*doc.Func) []Symbol {
	out := make([]Symbol, 0, len(fns))
	for _, f := range fns {
		decl := *f.Decl
		decl.Body, decl.Doc = nil, nil
		out = append(out, Symbol{Name: f.Name, Decl: printNode(fset, &decl), Doc: f.Doc})
	}
	return out
}

func printNode(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}
