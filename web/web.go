// Package web embeds the landing page template and its static assets, so
// the server ships as a single binary.
package web

import "embed"

// The icons in static/ are drawn by gen_icons.go from the same shapes as
// static/favicon.svg. After changing the logo, run: go generate ./web
//
//go:generate go run gen_icons.go

// FS holds templates/ and static/.
//
//go:embed templates static
var FS embed.FS
