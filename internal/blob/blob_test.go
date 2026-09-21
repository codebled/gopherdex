package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	key := "gopherdex.dev/alice/retry/@v/v1.0.0.zip"
	info, err := s.Put(ctx, key, strings.NewReader("zip bytes"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("zip bytes"))
	if info.Size != 9 || info.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("Info = %+v", info)
	}

	rc, err := s.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "zip bytes" {
		t.Fatalf("read %q", got)
	}

	if _, err := s.Put(ctx, key, strings.NewReader("different"), 1<<20); !errors.Is(err, ErrExists) {
		t.Fatalf("second Put = %v, want ErrExists", err)
	}

	entries, _ := os.ReadDir(filepath.Join(dir, "gopherdex.dev/alice/retry/@v"))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestPutLimitsAndKeys(t *testing.T) {
	ctx := context.Background()
	s, err := OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Put(ctx, "big", strings.NewReader("12345"), 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized Put = %v, want ErrTooLarge", err)
	}
	if _, err := s.Open(ctx, "big"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oversized blob was stored: %v", err)
	}
	for _, key := range []string{"../escape", "/abs", "a/../b", ".hidden", "a/.tmp-x", "sp ace", ""} {
		if _, err := s.Put(ctx, key, strings.NewReader("x"), 10); err == nil {
			t.Errorf("Put(%q) accepted an invalid key", key)
		}
	}
}
