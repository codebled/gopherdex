// Package blob stores published files (module zips, go.mod files) by key.
//
// Blobs are write-once: a published version must never change, because the
// go command and checksum databases record its hash. Put fails with
// ErrExists rather than overwrite.
package blob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// Errors stores return, wrapped with the key; test for them with errors.Is.
var (
	// ErrExists means Put found a blob already stored under the key.
	ErrExists = errors.New("blob already exists")
	// ErrNotFound means Open found no blob under the key.
	ErrNotFound = errors.New("blob not found")
	// ErrTooLarge means Put's reader held more than maxSize bytes.
	ErrTooLarge = errors.New("blob is too large")
)

// Info describes a stored blob.
type Info struct {
	Size   int64
	SHA256 string // lower-case hex
}

// Store is where blobs live. FS implements it; an S3-compatible store can
// replace it later (ReadAt maps to HTTP range requests).
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, maxSize int64) (Info, error)
	Open(ctx context.Context, key string) (Reader, error)
}

// Reader reads a stored blob sequentially or at random offsets, which
// archive/zip needs to list a module's files without reading all of it.
type Reader interface {
	io.ReadCloser
	io.ReaderAt
	Size() int64
}

type fileReader struct {
	*os.File
	size int64
}

// Size returns the blob's length in bytes.
func (f fileReader) Size() int64 { return f.size }

// FS stores blobs as files under a directory.
type FS struct {
	root *os.Root
}

// OpenFS opens (creating if needed) a blob directory.
func OpenFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create blob directory %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open blob directory %s: %w", dir, err)
	}
	return &FS{root: root}, nil
}

// Close releases the directory.
func (s *FS) Close() error { return s.root.Close() }

// checkKey allows slash-separated keys of letters, digits and "._-!@+~".
func checkKey(key string) error {
	if !fs.ValidPath(key) || key == "." {
		return fmt.Errorf("invalid blob key %q", key)
	}
	for _, elem := range strings.Split(key, "/") {
		if strings.HasPrefix(elem, ".") {
			return fmt.Errorf("invalid blob key %q: elements must not start with a dot", key)
		}
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("/._-!@+~", r) {
			return fmt.Errorf("invalid blob key %q: character %q", key, r)
		}
	}
	return nil
}

// Put writes r to key, reading at most maxSize bytes. The data lands in a
// temporary file first and is hard-linked into place, so readers never see
// a partial blob and an existing key is never replaced.
func (s *FS) Put(ctx context.Context, key string, r io.Reader, maxSize int64) (Info, error) {
	if err := checkKey(key); err != nil {
		return Info{}, err
	}
	if _, err := s.root.Lstat(key); err == nil {
		return Info{}, fmt.Errorf("%s: %w", key, ErrExists)
	}
	if dir := path.Dir(key); dir != "." {
		if err := s.root.MkdirAll(dir, 0o750); err != nil {
			return Info{}, fmt.Errorf("create directory for %s: %w", key, err)
		}
	}

	var suffix [8]byte
	rand.Read(suffix[:])
	tmp := path.Join(path.Dir(key), ".tmp-"+hex.EncodeToString(suffix[:]))
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return Info{}, fmt.Errorf("create temporary file for %s: %w", key, err)
	}
	// Once linked into place the temporary name is redundant; failing to
	// remove it leaves only a stray dot-file that checkKey never exposes.
	defer func() { _ = s.root.Remove(tmp) }()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), &ctxReader{ctx: ctx, r: io.LimitReader(r, maxSize+1)})
	if err == nil && n > maxSize {
		err = fmt.Errorf("%s is over %d bytes: %w", key, maxSize, ErrTooLarge)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Info{}, fmt.Errorf("write %s: %w", key, err)
	}

	if err := s.root.Link(tmp, key); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Info{}, fmt.Errorf("%s: %w", key, ErrExists)
		}
		return Info{}, fmt.Errorf("store %s: %w", key, err)
	}
	return Info{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// Open returns a reader for key.
func (s *FS) Open(ctx context.Context, key string) (Reader, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := s.root.Open(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", key, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat %s: %w", key, err)
	}
	return fileReader{File: f, size: st.Size()}, nil
}

// ctxReader stops a long copy when the request is canceled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Keys calls fn with every stored key, for copying to another store.
func (s *FS) Keys(ctx context.Context, fn func(key string) error) error {
	return fs.WalkDir(s.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		return fn(p)
	})
}

// Lister is a store that can enumerate its keys.
type Lister interface {
	Store
	Keys(ctx context.Context, fn func(key string) error) error
}

// Open opens the store a -blobs setting names: an s3:// URL (see ParseS3)
// or a local directory. The returned func releases it.
func Open(spec string, getenv func(string) string) (Lister, func() error, error) {
	if strings.HasPrefix(spec, "s3://") {
		s, err := ParseS3(spec, getenv)
		if err != nil {
			return nil, nil, err
		}
		return s, func() error { return nil }, nil
	}
	s, err := OpenFS(spec)
	if err != nil {
		return nil, nil, err
	}
	return s, s.Close, nil
}

// Copy copies every blob from src to dst, skipping ones dst already has,
// and reports how many it copied and skipped.
func Copy(ctx context.Context, dst Store, src Lister, maxSize int64) (copied, skipped int, err error) {
	err = src.Keys(ctx, func(key string) error {
		r, err := src.Open(ctx, key)
		if err != nil {
			return err
		}
		defer r.Close()
		if _, err := dst.Put(ctx, key, r, maxSize); errors.Is(err, ErrExists) {
			skipped++
			return nil
		} else if err != nil {
			return err
		}
		copied++
		return nil
	})
	return copied, skipped, err
}
