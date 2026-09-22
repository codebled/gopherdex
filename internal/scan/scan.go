// Package scan checks module zips before they are published, for the
// things a malicious or careless upload tends to contain: compiled
// programs, code that acts as soon as the package is imported, and large
// encoded payloads. It reports findings; the registry decides what to
// block and what to send for review.
//
// Go has no install scripts, so a module can only do harm once a program
// imports it. That makes init() functions and package-level variable
// initializers the place to look: they run on import, before the
// importer calls anything.
package scan

import (
	"archive/zip"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Severity says what the registry does about a finding.
type Severity string

// The severities, from most to least serious.
const (
	Block Severity = "block" // the upload is refused
	Warn  Severity = "warn"  // published, and sent to administrators for review
)

// Finding is one thing a check noticed.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
}

func (f Finding) String() string {
	loc := f.File
	if f.Line > 0 {
		loc += ":" + strconv.Itoa(f.Line)
	}
	if loc != "" {
		return f.Message + " (" + loc + ")"
	}
	return f.Message
}

// Blocked reports whether any finding blocks the upload.
func Blocked(fs []Finding) bool {
	return slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == Block })
}

const (
	maxGoFile      = 1 << 20  // bigger Go files are generated tables; skip them
	minEncodedBlob = 16 << 10 // string literals this long that look encoded are flagged
)

// Zip checks the files of a module zip. prefix is the "path@version/"
// every file in the zip starts with.
func Zip(zipFile, prefix string) ([]Finding, error) {
	zr, err := zip.OpenReader(zipFile)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	defer zr.Close()
	var out []Finding
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, prefix)
		if f.FileInfo().IsDir() {
			continue
		}
		inTestdata := strings.HasPrefix(name, "testdata/") || strings.Contains(name, "/testdata/")
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", name, err)
		}
		head := make([]byte, 4)
		n, _ := io.ReadFull(rc, head)
		head = head[:n]
		if kind := executableKind(head); kind != "" {
			rc.Close()
			sev, why := Block, "Compiled programs don't belong in a Go module's source; the go command builds from source."
			if inTestdata {
				sev, why = Warn, "It's under testdata, where test fixtures live, so it's allowed but reviewed."
			}
			if kind == "WebAssembly" {
				sev, why = Warn, "WebAssembly modules can be legitimate embedded assets, so it's allowed but reviewed."
			}
			out = append(out, Finding{Rule: "executable", Severity: sev, File: name,
				Message: fmt.Sprintf("%s binary in the module. %s", kind, why)})
			continue
		}
		if ext := path.Ext(name); (ext == ".syso" || ext == ".o" || ext == ".a") && !inTestdata {
			// The go command links .syso files into every build of the
			// package, so compiled code can hide in one without an
			// executable header. Legitimate uses exist (Windows icons and
			// manifests), so they're reviewed rather than refused.
			rc.Close()
			out = append(out, Finding{Rule: "object-file", Severity: Warn, File: name,
				Message: "Precompiled object file. The go command links .syso files into builds, so it could carry compiled code; it's allowed but reviewed."})
			continue
		}
		isGo := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && !inTestdata
		if !isGo {
			rc.Close()
			continue
		}
		if f.UncompressedSize64 > maxGoFile {
			// Too big to check, which is exactly how code would hide.
			rc.Close()
			out = append(out, Finding{Rule: "unscanned", Severity: Warn, File: name,
				Message: fmt.Sprintf("Go file of %d MB is too large to scan for code that runs on import; it's allowed but reviewed.", f.UncompressedSize64>>20)})
			continue
		}
		rest, err := io.ReadAll(io.LimitReader(rc, maxGoFile))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", name, err)
		}
		out = append(out, goFile(name, append(head, rest...))...)
	}
	return out, nil
}

// executableKind recognizes compiled programs by their magic numbers.
func executableKind(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x7fELF")):
		return "Linux (ELF)"
	case bytes.HasPrefix(head, []byte("MZ")):
		return "Windows (PE)"
	case bytes.HasPrefix(head, []byte{0xcf, 0xfa, 0xed, 0xfe}), bytes.HasPrefix(head, []byte{0xce, 0xfa, 0xed, 0xfe}),
		bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xcf}), bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xce}),
		bytes.HasPrefix(head, []byte{0xca, 0xfe, 0xba, 0xbe}):
		return "macOS (Mach-O)"
	case bytes.HasPrefix(head, []byte("\x00asm")):
		return "WebAssembly"
	}
	return ""
}

// risky are calls that have no business running the moment a package is
// imported: starting processes, network connections, loading code.
var risky = map[string]map[string]string{
	"os/exec":  {"Command": "starts a process", "CommandContext": "starts a process"},
	"os":       {"StartProcess": "starts a process", "RemoveAll": "deletes files"},
	"syscall":  {"Exec": "replaces the process", "ForkExec": "starts a process", "StartProcess": "starts a process"},
	"net/http": {"Get": "makes a network request", "Post": "makes a network request", "PostForm": "makes a network request", "Head": "makes a network request", "NewRequest": "makes a network request", "NewRequestWithContext": "makes a network request"},
	"net":      {"Dial": "opens a network connection", "DialTimeout": "opens a network connection", "Listen": "opens a network listener"},
	"plugin":   {"Open": "loads compiled code"},
}

var encodedPattern = regexp.MustCompile(`^[A-Za-z0-9+/=_\-\s]+$`)

// goFile checks one Go source file.
func goFile(name string, src []byte) []Finding {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil // the go command will refuse to build it anyway
	}
	// Which local names refer to the watched packages.
	local := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || risky[p] == nil {
			continue
		}
		n := path.Base(p)
		if imp.Name != nil {
			n = imp.Name.Name
		}
		if n != "_" && n != "." {
			local[n] = p
		}
	}

	var out []Finding
	seen := map[string]bool{}
	checkCalls := func(n ast.Node, where string) {
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || local[pkg.Name] == "" {
				return true
			}
			what, ok := risky[local[pkg.Name]][sel.Sel.Name]
			if !ok {
				return true
			}
			line := fset.Position(call.Pos()).Line
			key := fmt.Sprint(line, sel.Sel.Name)
			if seen[key] {
				return true
			}
			seen[key] = true
			out = append(out, Finding{Rule: "runs-on-import", Severity: Warn, File: name, Line: line,
				Message: fmt.Sprintf("%s calls %s.%s, which %s as soon as a program imports the package.", where, local[pkg.Name], sel.Sel.Name, what)})
			return true
		})
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == "init" && d.Recv == nil && d.Body != nil {
				checkCalls(d.Body, "init()")
			}
		case *ast.GenDecl:
			if d.Tok == token.VAR {
				for _, spec := range d.Specs {
					for _, v := range spec.(*ast.ValueSpec).Values {
						checkCalls(v, "A package-level variable")
					}
				}
			}
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || len(lit.Value) < minEncodedBlob {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil || len(s) < minEncodedBlob || !encodedPattern.MatchString(s) {
			return true
		}
		out = append(out, Finding{Rule: "encoded-blob", Severity: Warn, File: name, Line: fset.Position(lit.Pos()).Line,
			Message: fmt.Sprintf("A %d KB base64 or hex string literal: encoded payloads are a common way to hide code.", len(s)>>10)})
		return true
	})
	return out
}
