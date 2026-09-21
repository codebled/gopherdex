// Package store serves Go modules from a directory on disk.
//
// The layout mirrors the GOPROXY URL space. Module paths and versions are
// stored escaped (upper-case letters become "!" + lower-case) so modules that
// differ only in case do not collide on case-insensitive file systems:
//
//	<root>/<module>/@v/<version>/       module source: go.mod, *.go, LICENSE …
//	<root>/<module>/@v/<version>.json   optional metadata: {"Time": …, "Origin": …}
//
// All file access goes through os.Root, so a crafted module path can never
// read outside the store directory.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/gomod"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
)

const maxGoModSize = 16 << 20 // limit enforced by the go command

// Store reads modules from a directory.
type Store struct {
	root *os.Root
	fsys fs.FS
}

// Open opens the store rooted at dir.
func Open(dir string) (*Store, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open module store %q: %w", dir, err)
	}
	return &Store{root: root, fsys: root.FS()}, nil
}

// Close releases the store directory.
func (s *Store) Close() error { return s.root.Close() }

type versionMeta struct {
	Time   time.Time      `json:"Time"`
	Origin *module.Origin `json:"Origin,omitempty"`
}

// Modules lists the paths of all modules in the store.
func (s *Store) Modules(ctx context.Context) ([]string, error) {
	var mods []string
	err := fs.WalkDir(s.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() || p == "." {
			return nil
		}
		if d.Name() == "@v" {
			if mod, err := module.UnescapePath(path.Dir(p)); err == nil {
				mods = append(mods, mod)
			}
			return fs.SkipDir
		}
		if strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan module store: %w", err)
	}
	slices.Sort(mods)
	return mods, nil
}

// Versions returns a module's versions from lowest to highest.
func (s *Store) Versions(ctx context.Context, modPath string) ([]string, error) {
	esc, err := module.EscapePath(modPath)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(s.fsys, esc+"/@v")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("module %s: %w", modPath, module.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read versions of %s: %w", modPath, err)
	}
	versions := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if v, err := module.UnescapeVersion(e.Name()); err == nil {
			versions = append(versions, v)
		}
	}
	module.Sort(versions)
	return versions, ctx.Err()
}

// versionDir returns the directory holding modPath@version, or ErrNotFound.
func (s *Store) versionDir(modPath, version string) (string, fs.FileInfo, error) {
	escPath, err := module.EscapePath(modPath)
	if err != nil {
		return "", nil, err
	}
	escVersion, err := module.EscapeVersion(version)
	if err != nil {
		return "", nil, err
	}
	dir := escPath + "/@v/" + escVersion
	st, err := fs.Stat(s.fsys, dir)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !st.IsDir()) {
		return "", nil, fmt.Errorf("%s@%s: %w", modPath, version, module.ErrNotFound)
	}
	if err != nil {
		return "", nil, fmt.Errorf("stat %s@%s: %w", modPath, version, err)
	}
	return dir, st, nil
}

// Info returns the metadata for one version. Time and Origin come from the
// version's .json file when present; otherwise Time is the directory's
// modification time.
func (s *Store) Info(ctx context.Context, modPath, version string) (module.Info, error) {
	dir, st, err := s.versionDir(modPath, version)
	if err != nil {
		return module.Info{}, err
	}
	info := module.Info{Version: version, Time: st.ModTime().UTC().Truncate(time.Second)}
	data, err := fs.ReadFile(s.fsys, dir+".json")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Metadata is optional.
	case err != nil:
		return module.Info{}, fmt.Errorf("read metadata for %s@%s: %w", modPath, version, err)
	default:
		var meta versionMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			return module.Info{}, fmt.Errorf("parse %s.json: %w", dir, err)
		}
		if !meta.Time.IsZero() {
			info.Time = meta.Time.UTC()
		}
		info.Origin = meta.Origin
	}
	return info, ctx.Err()
}

// Latest returns the Info of the version the go command would pick for
// @latest.
func (s *Store) Latest(ctx context.Context, modPath string) (module.Info, error) {
	versions, err := s.Versions(ctx, modPath)
	if err != nil {
		return module.Info{}, err
	}
	latest := module.Latest(versions)
	if latest == "" {
		return module.Info{}, fmt.Errorf("module %s has no versions: %w", modPath, module.ErrNotFound)
	}
	return s.Info(ctx, modPath, latest)
}

// GoMod returns the go.mod file for a version. Versions without one get the
// minimal file the protocol requires. A go.mod that declares a different
// module path is an error, because the go command would reject it.
func (s *Store) GoMod(ctx context.Context, modPath, version string) ([]byte, error) {
	dir, _, err := s.versionDir(modPath, version)
	if err != nil {
		return nil, err
	}
	name := dir + "/go.mod"
	st, err := fs.Stat(s.fsys, name)
	if errors.Is(err, fs.ErrNotExist) {
		return []byte("module " + modPath + "\n"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat go.mod of %s@%s: %w", modPath, version, err)
	}
	if st.Size() > maxGoModSize {
		return nil, fmt.Errorf("go.mod of %s@%s is %d bytes, over the %d byte limit", modPath, version, st.Size(), maxGoModSize)
	}
	data, err := fs.ReadFile(s.fsys, name)
	if err != nil {
		return nil, fmt.Errorf("read go.mod of %s@%s: %w", modPath, version, err)
	}
	f, err := gomod.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("go.mod of %s@%s: %w", modPath, version, err)
	}
	if f.Module != modPath {
		return nil, fmt.Errorf("go.mod of %s@%s declares module %q", modPath, version, f.Module)
	}
	return data, ctx.Err()
}

// VersionFS returns the source tree of one version. Close the returned
// io.Closer when done.
func (s *Store) VersionFS(ctx context.Context, modPath, version string) (fs.FS, io.Closer, error) {
	dir, _, err := s.versionDir(modPath, version)
	if err != nil {
		return nil, nil, err
	}
	sub, err := fs.Sub(s.fsys, dir)
	return sub, io.NopCloser(nil), err
}
