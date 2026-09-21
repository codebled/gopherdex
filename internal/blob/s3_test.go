package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSigV4 checks the signer against the "GET Object" example in the S3
// documentation (Authenticating Requests: Using the Authorization Header).
func TestSigV4(t *testing.T) {
	s := &S3{Endpoint: "https://s3.amazonaws.com", Region: "us-east-1", Bucket: "examplebucket",
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }}
	req, _ := http.NewRequest(http.MethodGet, s.objectURL("test.txt").String(), nil)
	if req.URL.String() != "https://examplebucket.s3.amazonaws.com/test.txt" {
		t.Fatalf("url = %s", req.URL)
	}
	req.Header.Set("Range", "bytes=0-9")
	s.sign(req, emptySHA256)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization:\n got %s\nwant %s", got, want)
	}
}

func TestParseS3(t *testing.T) {
	env := map[string]string{"AWS_ACCESS_KEY_ID": "id", "AWS_SECRET_ACCESS_KEY": "secret"}
	getenv := func(k string) string { return env[k] }
	s, err := ParseS3("s3://modules/gopherdex?region=eu-west-1", getenv)
	if err != nil {
		t.Fatal(err)
	}
	if s.Endpoint != "https://s3.eu-west-1.amazonaws.com" || s.PathStyle || s.Prefix != "gopherdex/" {
		t.Errorf("AWS: %+v", s)
	}
	if u := s.objectURL("modules/a/@v/v1.0.0-ab.zip").String(); u != "https://modules.s3.eu-west-1.amazonaws.com/gopherdex/modules/a/%40v/v1.0.0-ab.zip" {
		t.Errorf("object URL = %s", u)
	}
	s, err = ParseS3("s3://modules?endpoint=https://acct.r2.cloudflarestorage.com&region=auto", getenv)
	if err != nil || !s.PathStyle || s.objectURL("k").String() != "https://acct.r2.cloudflarestorage.com/modules/k" {
		t.Errorf("R2: %+v, %v", s, err)
	}
	for _, bad := range []string{"s3://", "http://bucket", "s3://b?endpoint=ftp://x"} {
		if _, err := ParseS3(bad, getenv); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParseS3("s3://b", func(string) string { return "" }); err == nil {
		t.Error("accepted without credentials")
	}
}

// TestS3Store runs against a real S3-compatible server when
// GOPHERDEX_TEST_S3 names a bucket URL, for example with MinIO:
//
//	docker run -d -p 9000:9000 minio/minio server /data
//	GOPHERDEX_TEST_S3='s3://test?endpoint=http://localhost:9000' AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin go test ./internal/blob
func TestS3Store(t *testing.T) {
	spec := os.Getenv("GOPHERDEX_TEST_S3")
	if spec == "" {
		t.Skip("set GOPHERDEX_TEST_S3 to test against an S3-compatible server")
	}
	s, err := ParseS3(spec, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	s.Prefix += "test-" + time.Now().Format("150405.000000") + "/"
	ctx := context.Background()
	key := "modules/example.com/!azure/x/@v/v1.0.0+incompatible-ab12.zip"
	big := bytes.Repeat([]byte("gopher "), 3<<20) // over maxMemoryBlob: goes through a temp file
	info, err := s.Put(ctx, key, bytes.NewReader(big), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(big)) {
		t.Errorf("size = %d", info.Size)
	}
	if _, err := s.Put(ctx, key, strings.NewReader("different"), 64<<20); !errors.Is(err, ErrExists) {
		t.Errorf("overwrite: %v, want ErrExists", err)
	}
	r, err := s.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	part := make([]byte, 6)
	if _, err := r.ReadAt(part, 7); err != nil || string(part) != "gopher" || r.Size() != int64(len(big)) {
		t.Errorf("ReadAt = %q, %v; size %d", part, err, r.Size())
	}
	all, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(all, big) {
		t.Error("content differs")
	}
	if _, err := s.Put(ctx, "small.txt", strings.NewReader("hi"), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "big.txt", strings.NewReader("too long"), 3); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := s.Open(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	var keys []string
	if err := s.Keys(ctx, func(k string) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Errorf("keys = %v", keys)
	}

	// Copying from disk skips what's already there.
	fs, err := OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	fs.Put(ctx, "small.txt", strings.NewReader("hi"), 10)
	fs.Put(ctx, "modules/new.zip", strings.NewReader("zip"), 10)
	copied, skipped, err := Copy(ctx, s, fs, 10)
	if err != nil || copied != 1 || skipped != 1 {
		t.Errorf("copy: copied %d skipped %d, %v", copied, skipped, err)
	}
}
