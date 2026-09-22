package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

func login(ctx context.Context, args []string, env Env) error {
	fs := newFlags("login", env)
	registryFlag := fs.String("registry", "", "registry URL, e.g. https://gopherdex.dev")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errUsage
	}
	cfg, path, err := loadConfig(env)
	if err != nil {
		return err
	}
	registry, err := resolveRegistry(*registryFlag, env, cfg)
	if err != nil {
		return err
	}

	token := strings.TrimSpace(env.Getenv("GOPHERDEX_TOKEN"))
	if token == "" {
		if token, err = readToken(env, registry); err != nil {
			return err
		}
	}
	who, err := callWhoami(ctx, env, registry, token)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 401 {
			return fmt.Errorf("%s rejected that token: %w", registry, err)
		}
		return err
	}

	cfg.Registries[registry] = registryConfig{Token: token}
	cfg.Default = registry
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Signed in to %s as @%s. Token saved to %s.\n", registry, who.Username, path)
	fmt.Fprintf(env.Stdout, "You can publish modules under %s/.\n", who.Namespace)
	if !who.EmailVerified {
		fmt.Fprintf(env.Stdout, "Note: verify your email at %s/account before publishing.\n", registry)
	}
	return nil
}

// readToken prompts without echo on a terminal, or reads one line from
// piped input (echo "$TOKEN" | gopherdex login).
func readToken(env Env, registry string) (string, error) {
	if f, ok := env.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(env.Stderr, "Create an API token at %s/account, then paste it here: ", registry)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(env.Stderr)
		if err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return "", errors.New("no token given. Paste it when prompted, pipe it in, or set GOPHERDEX_TOKEN")
	}
	return token, nil
}

func logout(ctx context.Context, args []string, env Env) error {
	fs := newFlags("logout", env)
	registryFlag := fs.String("registry", "", "registry URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, path, err := loadConfig(env)
	if err != nil {
		return err
	}
	registry, err := resolveRegistry(*registryFlag, env, cfg)
	if err != nil {
		return err
	}
	if _, ok := cfg.Registries[registry]; !ok {
		fmt.Fprintf(env.Stdout, "No token saved for %s.\n", registry)
		return nil
	}
	delete(cfg.Registries, registry)
	if cfg.Default == registry {
		cfg.Default = ""
	}
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Removed the saved token for %s. Revoke it at %s/account if you no longer need it.\n", registry, registry)
	return nil
}

func whoami(ctx context.Context, args []string, env Env) error {
	fs := newFlags("whoami", env)
	registryFlag := fs.String("registry", "", "registry URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := loadConfig(env)
	if err != nil {
		return err
	}
	registry, err := resolveRegistry(*registryFlag, env, cfg)
	if err != nil {
		return err
	}
	token, err := resolveToken(registry, env, cfg)
	if err != nil {
		return err
	}
	who, err := callWhoami(ctx, env, registry, token)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "@%s on %s\n", who.Username, registry)
	fmt.Fprintf(env.Stdout, "  namespace  %s/\n", who.Namespace)
	for _, ns := range who.Namespaces {
		if ns != who.Namespace {
			fmt.Fprintf(env.Stdout, "  org        %s/\n", ns)
		}
	}
	expires := "never expires"
	if who.Token.ExpiresAt != nil {
		expires = "expires " + who.Token.ExpiresAt.Format("2 Jan 2006")
	}
	fmt.Fprintf(env.Stdout, "  token      %q, %s\n", who.Token.Name, expires)
	if !who.EmailVerified {
		fmt.Fprintf(env.Stdout, "  email      not verified: confirm it at %s/account before publishing\n", registry)
	}
	return nil
}

func yank(ctx context.Context, args []string, env Env, doYank bool) error {
	name := "unyank"
	if doYank {
		name = "yank"
	}
	fs := newFlags(name, env)
	registryFlag := fs.String("registry", "", "registry URL")
	reason := fs.String("reason", "", "why the release is yanked, shown on its page")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUsage
	}
	modPath, version, ok := strings.Cut(fs.Arg(0), "@")
	if !ok || modPath == "" || version == "" {
		return fmt.Errorf("name the release as MODULE@VERSION, e.g. gopherdex %s gopherdex.dev/you/lib@v1.2.3", name)
	}
	cfg, _, err := loadConfig(env)
	if err != nil {
		return err
	}
	registry, err := resolveRegistry(*registryFlag, env, cfg)
	if err != nil {
		return err
	}
	token, err := resolveToken(registry, env, cfg)
	if err != nil {
		return err
	}
	resp, err := callYank(ctx, env, registry, token, modPath, version, *reason, doYank)
	if err != nil {
		return err
	}
	if doYank {
		fmt.Fprintf(env.Stdout, "Yanked %s@%s. New installs skip it; builds that already use it keep working.\n", modPath, version)
		fmt.Fprintf(env.Stdout, "To warn users of the public Go mirror too, add \"retract %s\" to go.mod and publish a new version.\n", version)
	} else {
		fmt.Fprintf(env.Stdout, "Restored %s@%s. It's installable again.\n", modPath, version)
	}
	fmt.Fprintf(env.Stdout, "  view  %s\n", resp.URL)
	return nil
}
