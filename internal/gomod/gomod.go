// Package gomod reads the parts of a go.mod file that the discovery pages
// show: module path, deprecation notice, go version, requirements and
// retractions. It is intentionally small; use golang.org/x/mod/modfile when
// full fidelity matters.
package gomod

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/parthiban-sivakumar/gopherdex/internal/module"
)

// File is a parsed go.mod.
type File struct {
	Module     string    `json:"module"`
	Deprecated string    `json:"deprecated,omitempty"`
	Go         string    `json:"go,omitempty"`
	Require    []Require `json:"require"`
	Retract    []Retract `json:"retract"`
}

// Require is one requirement line.
type Require struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Indirect bool   `json:"indirect"`
}

// Retract is a retracted version or closed version range.
type Retract struct {
	Low       string `json:"low"`
	High      string `json:"high"`
	Rationale string `json:"rationale,omitempty"`
}

// Parse reads a go.mod file.
func Parse(data []byte) (*File, error) {
	f := &File{Require: []Require{}, Retract: []Retract{}}
	var (
		block    string   // verb of the open "( … )" block
		comments []string // comment lines directly above the current line
	)
	for i, raw := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line, comment := splitComment(raw)
		if line == "" {
			if comment != "" {
				comments = append(comments, comment)
			} else {
				comments = nil
			}
			continue
		}
		fields := strings.Fields(line)
		verb, args := fields[0], fields[1:]
		switch {
		case block != "" && line == ")":
			block, comments = "", nil
			continue
		case block != "":
			verb, args = block, fields
		case len(args) == 1 && args[0] == "(":
			block, comments = verb, nil
			continue
		}

		switch verb {
		case "module":
			if len(args) != 1 {
				return nil, fmt.Errorf("go.mod:%d: usage: module example.com/path", lineNo)
			}
			path, err := unquote(args[0])
			if err != nil {
				return nil, fmt.Errorf("go.mod:%d: %w", lineNo, err)
			}
			f.Module = path
			f.Deprecated = deprecation(append(comments, comment))
		case "go":
			if len(args) != 1 {
				return nil, fmt.Errorf("go.mod:%d: usage: go 1.23", lineNo)
			}
			f.Go = args[0]
		case "require":
			if len(args) != 2 {
				return nil, fmt.Errorf("go.mod:%d: usage: require example.com/path v1.2.3", lineNo)
			}
			path, err := unquote(args[0])
			if err != nil {
				return nil, fmt.Errorf("go.mod:%d: %w", lineNo, err)
			}
			version, err := unquote(args[1])
			if err != nil {
				return nil, fmt.Errorf("go.mod:%d: %w", lineNo, err)
			}
			indirect := comment == "indirect" || strings.HasPrefix(comment, "indirect;")
			f.Require = append(f.Require, Require{Path: path, Version: version, Indirect: indirect})
		case "retract":
			r, err := parseRetract(strings.Join(args, " "))
			if err != nil {
				return nil, fmt.Errorf("go.mod:%d: %w", lineNo, err)
			}
			r.Rationale = comment
			if r.Rationale == "" {
				r.Rationale = strings.Join(comments, " ")
			}
			f.Retract = append(f.Retract, r)
		}
		comments = nil
	}
	if block != "" {
		return nil, fmt.Errorf("go.mod: %s block is missing its closing parenthesis", block)
	}
	if f.Module == "" {
		return nil, errors.New("go.mod has no module directive")
	}
	return f, nil
}

// Retracted reports whether version falls in any retraction.
func (f *File) Retracted(version string) (Retract, bool) {
	for _, r := range f.Retract {
		if module.Compare(r.Low, version) <= 0 && module.Compare(version, r.High) <= 0 {
			return r, true
		}
	}
	return Retract{}, false
}

func splitComment(raw string) (line, comment string) {
	line = raw
	if i := strings.Index(raw, "//"); i >= 0 {
		line, comment = raw[:i], strings.TrimSpace(raw[i+2:])
	}
	return strings.TrimSpace(line), comment
}

func deprecation(lines []string) string {
	for i, l := range lines {
		if rest, ok := strings.CutPrefix(l, "Deprecated:"); ok {
			parts := append([]string{strings.TrimSpace(rest)}, lines[i+1:]...)
			return strings.TrimSpace(strings.Join(parts, " "))
		}
	}
	return ""
}

func parseRetract(s string) (Retract, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		v, err := unquote(s)
		return Retract{Low: v, High: v}, err
	}
	inner, ok := strings.CutSuffix(s[1:], "]")
	if !ok {
		return Retract{}, fmt.Errorf("retract %q is missing its closing bracket", s)
	}
	low, high, ok := strings.Cut(inner, ",")
	if !ok {
		return Retract{}, fmt.Errorf("retract range %q needs two versions", s)
	}
	return Retract{Low: strings.TrimSpace(low), High: strings.TrimSpace(high)}, nil
}

func unquote(s string) (string, error) {
	if strings.HasPrefix(s, `"`) || strings.HasPrefix(s, "`") {
		u, err := strconv.Unquote(s)
		if err != nil {
			return "", fmt.Errorf("bad quoted string %s: %w", s, err)
		}
		return u, nil
	}
	return s, nil
}
