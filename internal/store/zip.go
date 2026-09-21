package store

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"unicode"
)

const (
	maxZipSize     = 500 << 20 // total uncompressed size the go command accepts
	maxLicenseSize = 16 << 20
)

// Zip prepares the module zip for one version. Every file is checked while
// the plan is built, so problems surface before any response bytes are sent;
// WriteTo then streams the archive.
//
// Files are stored under "<module>@<version>/". Following the module zip
// rules, the plan leaves out VCS directories, vendor directories, nested
// modules (subdirectories with their own go.mod), symlinks and other
// irregular files.
func (s *Store) Zip(ctx context.Context, modPath, version string) (io.WriterTo, error) {
	dir, _, err := s.versionDir(modPath, version)
	if err != nil {
		return nil, err
	}
	z := &modZip{fsys: s.fsys, dir: dir, prefix: modPath + "@" + version + "/"}
	var total int64
	seen := map[string]string{} // lower-case name → name, to catch case collisions
	err = fs.WalkDir(s.fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		if d.IsDir() {
			if rel == "" {
				return nil
			}
			switch d.Name() {
			case ".git", ".hg", ".svn", ".bzr", "vendor":
				return fs.SkipDir
			}
			if _, err := fs.Stat(s.fsys, p+"/go.mod"); err == nil {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if err := checkFileName(rel); err != nil {
			return err
		}
		if other, dup := seen[strings.ToLower(rel)]; dup {
			return fmt.Errorf("files %q and %q differ only in case", other, rel)
		}
		seen[strings.ToLower(rel)] = rel
		fi, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case rel == "go.mod" && fi.Size() > maxGoModSize:
			return fmt.Errorf("go.mod is %d bytes, over the %d byte limit", fi.Size(), maxGoModSize)
		case rel == "LICENSE" && fi.Size() > maxLicenseSize:
			return fmt.Errorf("LICENSE is %d bytes, over the %d byte limit", fi.Size(), maxLicenseSize)
		}
		total += fi.Size()
		if total > maxZipSize {
			return fmt.Errorf("module source is over the %d byte limit", maxZipSize)
		}
		z.files = append(z.files, zipFile{name: rel, size: fi.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("prepare zip for %s@%s: %w", modPath, version, err)
	}
	return z, nil
}

type modZip struct {
	fsys   fs.FS
	dir    string
	prefix string
	files  []zipFile
}

type zipFile struct {
	name string // relative to dir
	size int64
}

func (z *modZip) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: w}
	zw := zip.NewWriter(cw)
	for _, f := range z.files {
		if err := z.add(zw, f); err != nil {
			return cw.n, err
		}
	}
	if err := zw.Close(); err != nil {
		return cw.n, fmt.Errorf("finish zip: %w", err)
	}
	return cw.n, nil
}

func (z *modZip) add(zw *zip.Writer, f zipFile) error {
	src, err := z.fsys.Open(z.dir + "/" + f.name)
	if err != nil {
		return fmt.Errorf("open %s: %w", f.name, err)
	}
	defer src.Close()
	dst, err := zw.CreateHeader(&zip.FileHeader{Name: z.prefix + f.name, Method: zip.Deflate})
	if err != nil {
		return fmt.Errorf("add %s to zip: %w", f.name, err)
	}
	n, err := io.Copy(dst, src)
	if err != nil {
		return fmt.Errorf("write %s to zip: %w", f.name, err)
	}
	if n != f.size {
		return fmt.Errorf("%s changed size while zipping (%d → %d bytes)", f.name, f.size, n)
	}
	return nil
}

// checkFileName applies the go command's rules for file names inside module
// zips: letters, digits and a small set of punctuation.
func checkFileName(name string) error {
	for _, elem := range strings.Split(name, "/") {
		for _, r := range elem {
			if unicode.IsLetter(r) || '0' <= r && r <= '9' || strings.ContainsRune("!#$%&()+,-.=@[]^_{}~ ", r) {
				continue
			}
			return fmt.Errorf("file name %q contains %q, which module zips do not allow", name, r)
		}
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
