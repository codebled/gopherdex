package registry

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"

	"github.com/parthiban-sivakumar/gopherdex/internal/module"
)

// Source is a set of hosted modules: the database-backed Registry, or the
// directory-backed store used for fixtures.
type Source interface {
	Modules(ctx context.Context) ([]string, error)
	Versions(ctx context.Context, modPath string) ([]string, error)
	Info(ctx context.Context, modPath, version string) (module.Info, error)
	Latest(ctx context.Context, modPath string) (module.Info, error)
	GoMod(ctx context.Context, modPath, version string) ([]byte, error)
	Zip(ctx context.Context, modPath, version string) (io.WriterTo, error)
	VersionFS(ctx context.Context, modPath, version string) (fs.FS, io.Closer, error)
}

// Multi serves modules from several sources. Each module belongs to the
// first source that has it.
type Multi []Source

func (m Multi) source(ctx context.Context, modPath string) (Source, error) {
	for _, s := range m {
		_, err := s.Versions(ctx, modPath)
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, module.ErrNotFound) || errors.Is(err, ErrQuarantined) {
			return nil, err
		}
	}
	return nil, module.ErrNotFound
}

// Modules lists the modules of every source, sorted, without duplicates.
func (m Multi) Modules(ctx context.Context) ([]string, error) {
	var all []string
	for _, s := range m {
		paths, err := s.Modules(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, paths...)
	}
	slices.Sort(all)
	return slices.Compact(all), nil
}

// Versions lists the versions of modPath from the source that has it.
func (m Multi) Versions(ctx context.Context, modPath string) ([]string, error) {
	s, err := m.source(ctx, modPath)
	if err != nil {
		return nil, err
	}
	return s.Versions(ctx, modPath)
}

// Info describes one version of modPath from the source that has it.
func (m Multi) Info(ctx context.Context, modPath, version string) (module.Info, error) {
	s, err := m.source(ctx, modPath)
	if err != nil {
		return module.Info{}, err
	}
	return s.Info(ctx, modPath, version)
}

// Latest describes the latest version of modPath from the source that
// has it.
func (m Multi) Latest(ctx context.Context, modPath string) (module.Info, error) {
	s, err := m.source(ctx, modPath)
	if err != nil {
		return module.Info{}, err
	}
	return s.Latest(ctx, modPath)
}

// GoMod returns the go.mod of one version of modPath from the source
// that has it.
func (m Multi) GoMod(ctx context.Context, modPath, version string) ([]byte, error) {
	s, err := m.source(ctx, modPath)
	if err != nil {
		return nil, err
	}
	return s.GoMod(ctx, modPath, version)
}

// Zip returns the module zip of one version of modPath from the source
// that has it.
func (m Multi) Zip(ctx context.Context, modPath, version string) (io.WriterTo, error) {
	s, err := m.source(ctx, modPath)
	if err != nil {
		return nil, err
	}
	return s.Zip(ctx, modPath, version)
}

// VersionFS exposes the files of one version of modPath from the source
// that has it. Close the returned io.Closer when done.
func (m Multi) VersionFS(ctx context.Context, modPath, version string) (fs.FS, io.Closer, error) {
	s, err := m.source(ctx, modPath)
	if err != nil {
		return nil, nil, err
	}
	return s.VersionFS(ctx, modPath, version)
}
