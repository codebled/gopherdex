// Package web embeds the landing page template and its static assets, so
// the server ships as a single binary.
package web

import "embed"

// FS holds templates/ and static/.
//
//go:embed templates static
var FS embed.FS
