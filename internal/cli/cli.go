// Package cli implements the gopherdex command, which authors use to sign in
// to a registry and publish Go modules from git tags.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/codebled/gopherdex/internal/version"
)

// Env is everything the command touches outside itself, so tests can
// replace it.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Dir    string // working directory
	HTTP   *http.Client
}

// OSEnv returns the real process environment.
func OSEnv() Env {
	dir, _ := os.Getwd()
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Dir: dir, HTTP: http.DefaultClient}
}

const usage = `gopherdex publishes Go modules to a Gopherdex registry.

Usage:
  gopherdex login   [--registry URL]         save an API token for a registry
  gopherdex whoami  [--registry URL]         show who the saved token belongs to
  gopherdex publish [--registry URL] [--dry-run] [--dir DIR] [VERSION]
                                             publish a tagged release of the module in DIR
  gopherdex yank    [--registry URL] [--reason TEXT] MODULE@VERSION
                                             hide a release from new installs (pinned builds keep working)
  gopherdex unyank  [--registry URL] MODULE@VERSION
                                             make a yanked release installable again
  gopherdex logout  [--registry URL]         forget the saved token

Environment:
  GOPHERDEX_REGISTRY   registry URL, instead of --registry or the saved default
  GOPHERDEX_TOKEN      API token, instead of the one saved by login (for CI)
  GOPHERDEX_CONFIG     credentials file (default: your user config directory)

In GitHub Actions with no token, publish uses trusted publishing: it trades the
job's OIDC ID token for a short-lived token. Give the workflow
"permissions: id-token: write" and add a trusted publisher on the registry.
`

// errUsage marks errors that should print usage.
var errUsage = errors.New("usage")

// Main runs the command and returns the process exit code.
func Main(args []string, env Env) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(env.Stdout, usage)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintf(env.Stdout, "gopherdex %s\n", version.String())
		return 0
	}
	cmds := map[string]func(context.Context, []string, Env) error{
		"login":   login,
		"logout":  logout,
		"whoami":  whoami,
		"publish": publish,
		"yank":    func(ctx context.Context, args []string, env Env) error { return yank(ctx, args, env, true) },
		"unyank":  func(ctx context.Context, args []string, env Env) error { return yank(ctx, args, env, false) },
	}
	run, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(env.Stderr, "gopherdex: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err := run(context.Background(), args[1:], env); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(env.Stderr, usage)
			return 2
		}
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(env.Stderr, "gopherdex %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func newFlags(name string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

// resolveRegistry picks the registry URL: flag, then $GOPHERDEX_REGISTRY,
// then the default saved by login.
func resolveRegistry(flagValue string, env Env, cfg *config) (string, error) {
	raw := flagValue
	if raw == "" {
		raw = env.Getenv("GOPHERDEX_REGISTRY")
	}
	if raw == "" {
		raw = cfg.Default
	}
	if raw == "" {
		return "", errors.New("no registry chosen. Pass --registry https://your-registry or run gopherdex login --registry URL first")
	}
	return normalizeRegistry(raw)
}

// normalizeRegistry requires https, except for loopback addresses used in
// local development, so tokens never cross the network in clear text.
func normalizeRegistry(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return "", fmt.Errorf("registry %q must be a URL like https://gopherdex.dev", raw)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return "", fmt.Errorf("registry %q must use https; plain http is only allowed for localhost", raw)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func resolveToken(registry string, env Env, cfg *config) (string, error) {
	if t := strings.TrimSpace(env.Getenv("GOPHERDEX_TOKEN")); t != "" {
		return t, nil
	}
	if c, ok := cfg.Registries[registry]; ok && c.Token != "" {
		return c.Token, nil
	}
	return "", fmt.Errorf("you're not signed in to %s. Create a token at %s/account, then run: gopherdex login --registry %s", registry, registry, registry)
}
