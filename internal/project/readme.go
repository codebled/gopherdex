package project

import (
	"bytes"
	"html/template"
	"net/url"
	"path"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

const maxReadmeSize = 512 << 10

// renderReadme turns a README into HTML that is safe to embed.
//
// goldmark is used without WithUnsafe, so raw HTML in the README is dropped
// and dangerous link schemes (javascript:, vbscript:, file:, non-image data:)
// are removed. Relative links and images point into the source repository
// when it is on a known forge; otherwise they are unlinked, because they
// would resolve against this site.
func renderReadme(name string, src []byte, repo *forge) template.HTML {
	if len(src) > maxReadmeSize {
		src = src[:maxReadmeSize]
	}
	ext := strings.ToLower(path.Ext(name))
	if ext != ".md" && ext != ".markdown" {
		return template.HTML("<pre class=\"readme-text\">" + template.HTMLEscapeString(string(src)) + "</pre>") //nolint:gosec // the README text is HTML-escaped
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(&linkRewriter{repo: repo}, 100))),
	)
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return template.HTML("<pre class=\"readme-text\">" + template.HTMLEscapeString(string(src)) + "</pre>") //nolint:gosec // the README text is HTML-escaped
	}
	// READMEs are user content: don't pass search ranking to their links.
	out := strings.ReplaceAll(buf.String(), `<a href="http`, `<a rel="nofollow ugc noopener" href="http`)
	return template.HTML(out) //nolint:gosec // goldmark without WithUnsafe drops raw HTML and unsafe links
}

// forge knows how to link to files in a hosted git repository at a tag.
type forge struct {
	blob, raw string // prefixes ending in "/"
}

// newForge supports the common public forges. dir is the module's
// subdirectory inside the repository ("" at the root).
func newForge(repository, tag, dir string) *forge {
	u, err := url.Parse(repository)
	if err != nil || u.Scheme != "https" || tag == "" {
		return nil
	}
	repo := strings.TrimSuffix(repository, "/")
	ref := url.PathEscape(tag)
	suffix := ""
	if dir != "" {
		suffix = dir + "/"
	}
	switch u.Host {
	case "github.com":
		return &forge{blob: repo + "/blob/" + ref + "/" + suffix, raw: repo + "/raw/" + ref + "/" + suffix}
	case "gitlab.com":
		return &forge{blob: repo + "/-/blob/" + ref + "/" + suffix, raw: repo + "/-/raw/" + ref + "/" + suffix}
	case "codeberg.org":
		return &forge{blob: repo + "/src/tag/" + ref + "/" + suffix, raw: repo + "/raw/tag/" + ref + "/" + suffix}
	}
	return nil
}

type linkRewriter struct {
	repo *forge
}

// Transform implements parser.ASTTransformer. It points relative links and
// images into the source repository and replaces the ones it cannot resolve
// with their text.
func (l *linkRewriter) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	var unlink []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link:
			if dest, ok := l.resolve(n.Destination, false); ok {
				n.Destination = dest
			} else {
				unlink = append(unlink, n)
			}
		case *ast.Image:
			if dest, ok := l.resolve(n.Destination, true); ok {
				n.Destination = dest
			} else {
				unlink = append(unlink, n)
			}
		}
		return ast.WalkContinue, nil
	}) // the walker never returns an error
	// Replace unresolvable links and images with their text.
	for _, n := range unlink {
		parent := n.Parent()
		for c := n.FirstChild(); c != nil; {
			next := c.NextSibling()
			parent.InsertBefore(parent, n, c)
			c = next
		}
		parent.RemoveChild(parent, n)
	}
}

// resolve returns the destination to use, or false to drop the link.
func (l *linkRewriter) resolve(dest []byte, image bool) ([]byte, bool) {
	d := string(dest)
	if d == "" || strings.HasPrefix(d, "#") {
		return dest, true
	}
	u, err := url.Parse(d)
	if err != nil {
		return nil, false
	}
	if u.Scheme != "" || u.Host != "" {
		return dest, true // absolute; goldmark filters dangerous schemes
	}
	if l.repo == nil {
		return nil, false
	}
	p := strings.TrimPrefix(path.Clean("/"+u.Path), "/")
	base := l.repo.blob
	if image {
		base = l.repo.raw
	}
	out := base + p
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return []byte(out), true
}
