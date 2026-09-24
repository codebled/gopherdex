// Package version reports which build of Gopherdex is running.
package version

import "runtime/debug"

// Module is Gopherdex's module path; the CLI installs from Module + "/cmd/gopherdex".
const Module = "github.com/codebled/gopherdex"

// CLIInstall is the command that installs the gopherdex CLI.
const CLIInstall = "go install " + Module + "/cmd/gopherdex@latest"

// Version is set for release builds:
//
//	go build -ldflags "-X github.com/codebled/gopherdex/internal/version.Version=v1.2.3"
//
// When it's empty, String falls back to what the go command recorded.
var Version = ""

// String returns the release version, the module version for
// "go install …@v1.2.3" builds, or "devel" plus the commit for local builds.
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "devel"
	}
	v := "devel+" + rev[:min(12, len(rev))]
	if dirty {
		v += "-dirty"
	}
	return v
}
