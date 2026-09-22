package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"
)

func publish(ctx context.Context, args []string, env Env) error {
	fs := newFlags("publish", env)
	registryFlag := fs.String("registry", "", "registry URL")
	dryRun := fs.Bool("dry-run", false, "build and check the module zip without uploading")
	dirFlag := fs.String("dir", ".", "directory inside the module to publish")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errUsage
	}

	dir := *dirFlag
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(env.Dir, dir)
	}
	modRoot, err := findModuleRoot(dir)
	if err != nil {
		return err
	}
	repoRoot, err := git(ctx, modRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%s isn't in a git repository. gopherdex publish builds releases from git tags: run git init, commit your code and tag a version first", modRoot)
	}
	repoRoot, modRoot = realPath(repoRoot), realPath(modRoot)
	subdir, err := filepath.Rel(repoRoot, modRoot)
	if err != nil {
		return err
	}
	subdir = filepath.ToSlash(subdir)
	tagPrefix := ""
	if subdir == "." {
		subdir = ""
	} else {
		tagPrefix = subdir + "/"
	}

	// Pick the version and its tag.
	version := fs.Arg(0)
	if version == "" {
		if version, err = versionAtHEAD(ctx, modRoot, tagPrefix); err != nil {
			return err
		}
	}
	if !semver.IsValid(version) || semver.Canonical(version) != version || strings.Contains(version, "+") {
		return fmt.Errorf("%q isn't a release version. Use a full semantic version like v1.2.3 or v1.2.3-rc.1", version)
	}
	if module.IsPseudoVersion(version) {
		return fmt.Errorf("%s is a pseudo-version. Tag a release instead: git tag v1.2.3", version)
	}
	tag := tagPrefix + version
	commit, err := git(ctx, modRoot, "rev-parse", "--verify", "--quiet", "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return fmt.Errorf("there's no git tag %s. Create it with: git tag %s", tag, tag)
	}

	// The release is built from the tag, so read go.mod from the tag too.
	goModAtTag, err := git(ctx, repoRoot, "show", commit+":"+tagPrefix+"go.mod")
	if err != nil {
		return fmt.Errorf("tag %s has no %sgo.mod. Commit go.mod before tagging", tag, tagPrefix)
	}
	mf, err := modfile.ParseLax("go.mod", []byte(goModAtTag+"\n"), nil)
	if err != nil {
		return fmt.Errorf("go.mod at tag %s can't be parsed: %w", tag, err)
	}
	if mf.Module == nil {
		return fmt.Errorf("go.mod at tag %s has no module line", tag)
	}
	modPath := mf.Module.Mod.Path
	_, pathMajor, _ := module.SplitPathVersion(modPath)
	if err := module.CheckPathMajor(version, pathMajor); err != nil {
		major := semver.Major(version)
		if major != "v0" && major != "v1" {
			return fmt.Errorf("%s is a %s release, so go.mod must say module %s/%s (Go's major version rule)", version, major, strings.TrimSuffix(modPath, pathMajor), major)
		}
		return fmt.Errorf("%s is a %s release, so the module path must not end in %s", version, major, pathMajor)
	}

	cfg, _, err := loadConfig(env)
	if err != nil {
		return err
	}
	registry, regErr := resolveRegistry(*registryFlag, env, cfg)
	token, tokErr := "", regErr
	if regErr == nil {
		token, tokErr = resolveToken(registry, env, cfg)
	}
	// In GitHub Actions without an API token, use trusted publishing.
	trustedAs := ""
	if tokErr != nil && regErr == nil && !*dryRun {
		switch {
		case inGitHubActions(env):
			minted, err := trustedPublishToken(ctx, env, registry, modPath)
			if err != nil {
				return err
			}
			token, tokErr, trustedAs = minted.Token, nil, minted.Publisher
		case env.Getenv("GITHUB_ACTIONS") == "true":
			return errNoIDTokenPermission
		}
	}
	if !*dryRun && tokErr != nil {
		return tokErr
	}
	if tokErr == nil && trustedAs == "" {
		who, err := callWhoami(ctx, env, registry, token)
		if err != nil {
			return err
		}
		// Whether this account may publish modPath (its own namespace, an
		// organization, or a module it co-maintains) is the registry's call;
		// its refusal explains what to do.
		if !who.EmailVerified {
			return fmt.Errorf("verify your email at %s/account before publishing", registry)
		}
	}

	// Build the zip exactly as the go command would download it.
	tmp, err := os.CreateTemp("", "gopherdex-publish-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	mv := module.Version{Path: modPath, Version: version}
	if err := modzip.CreateFromVCS(tmp, mv, repoRoot, commit, subdir); err != nil {
		tmp.Close()
		return fmt.Errorf("build the module zip from tag %s: %w", tag, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	checked, err := modzip.CheckZip(mv, tmp.Name())
	if err != nil {
		return fmt.Errorf("the module zip isn't valid: %w", err)
	}
	h1, err := dirhash.HashZip(tmp.Name(), dirhash.Hash1)
	if err != nil {
		return err
	}
	st, err := os.Stat(tmp.Name())
	if err != nil {
		return err
	}

	repository := repositoryURL(ctx, repoRoot)
	out := env.Stdout
	fmt.Fprintf(out, "%s %s@%s\n", map[bool]string{true: "Checked", false: "Publishing"}[*dryRun], modPath, version)
	fmt.Fprintf(out, "  tag      %s (%s)\n", tag, commit[:min(12, len(commit))])
	fmt.Fprintf(out, "  files    %d, %s zipped\n", len(checked.Valid), humanBytes(st.Size()))
	if len(checked.Omitted) > 0 {
		fmt.Fprintf(out, "  omitted  %d (vendor, nested modules or VCS files)\n", len(checked.Omitted))
	}
	fmt.Fprintf(out, "  hash     %s\n", h1)
	if repository != "" {
		fmt.Fprintf(out, "  source   %s\n", repository)
	}
	if trustedAs != "" {
		fmt.Fprintf(out, "  auth     trusted publisher, %s\n", trustedAs)
	}
	if *dryRun {
		if tokErr != nil {
			fmt.Fprintf(out, "Dry run: nothing uploaded. (Not signed in, so namespace permissions weren't checked: %v)\n", tokErr)
		} else {
			fmt.Fprintf(out, "Dry run: nothing uploaded. Run without --dry-run to publish to %s.\n", registry)
		}
		return nil
	}

	resp, err := callUpload(ctx, env, registry, token, uploadFields{
		Module: modPath, Version: version, ZipFile: tmp.Name(),
		Repository: repository, Commit: commit, Ref: "refs/tags/" + tag,
	})
	if err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	if resp.H1 != h1 {
		return fmt.Errorf("the registry stored hash %s, but the zip built here hashes to %s. Please report this", resp.H1, h1)
	}
	if resp.AlreadyPublished {
		fmt.Fprintf(out, "%s@%s was already published with identical content. Nothing changed.\n", modPath, version)
	} else {
		fmt.Fprintf(out, "Published %s@%s\n", modPath, version)
	}
	fmt.Fprintf(out, "  view     %s\n", resp.URL)
	install := resp.Install
	if install == "" { // older registries
		install = "GOPROXY=" + resp.ProxyURL + " go get " + modPath + "@" + version
	}
	fmt.Fprintf(out, "  install  %s\n", install)
	if len(resp.Warnings) > 0 {
		fmt.Fprintf(out, "\nThe registry's publish checks flagged %d thing(s) for its administrators to review:\n", len(resp.Warnings))
		for _, w := range resp.Warnings {
			loc := w.File
			if w.Line > 0 {
				loc = fmt.Sprintf("%s:%d", w.File, w.Line)
			}
			if loc != "" {
				loc = " (" + loc + ")"
			}
			fmt.Fprintf(out, "  warning  %s%s\n", w.Message, loc)
		}
	}
	return nil
}

func findModuleRoot(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if st, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && st.Mode().IsRegular() {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no go.mod found in %s or its parents. Run gopherdex publish inside a Go module", dir)
		}
	}
}

// versionAtHEAD returns the release tag on the current commit.
func versionAtHEAD(ctx context.Context, dir, tagPrefix string) (string, error) {
	if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return "", errors.New("this repository has no commits yet. Commit your code, tag a version (git tag v0.1.0), then publish")
	}
	out, err := git(ctx, dir, "tag", "--points-at", "HEAD")
	if err != nil {
		return "", err
	}
	var versions []string
	for _, t := range strings.Fields(out) {
		v, ok := strings.CutPrefix(t, tagPrefix)
		if ok && semver.IsValid(v) && semver.Canonical(v) == v && !strings.Contains(v, "+") {
			versions = append(versions, v)
		}
	}
	switch len(versions) {
	case 0:
		return "", fmt.Errorf("the current commit has no %sv* release tag. Tag it (git tag %sv0.1.0) or name a version: gopherdex publish v0.1.0", tagPrefix, tagPrefix)
	case 1:
		return versions[0], nil
	default:
		return "", fmt.Errorf("the current commit has several release tags (%s). Name the one to publish: gopherdex publish %s", strings.Join(versions, ", "), versions[0])
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git isn't installed or isn't on PATH")
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

var scpLike = regexp.MustCompile(`^[\w.-]+@([\w.-]+):(.+)$`)

// repositoryURL turns the origin remote into a public https URL, dropping
// any credentials. It returns "" when there is no usable remote.
func repositoryURL(ctx context.Context, repoRoot string) string {
	raw, err := git(ctx, repoRoot, "remote", "get-url", "origin")
	if err != nil || raw == "" {
		return ""
	}
	if m := scpLike.FindStringSubmatch(raw); m != nil { // git@github.com:owner/repo.git
		raw = "https://" + m[1] + "/" + m[2]
	}
	u, err := url.Parse(strings.TrimSuffix(raw, ".git"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "ssh") {
		return ""
	}
	u.Scheme, u.User, u.RawQuery, u.Fragment = "https", nil, "", ""
	u.Host = u.Hostname() // ssh ports aren't web ports
	return u.String()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
