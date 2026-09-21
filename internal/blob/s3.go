package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// S3 stores blobs in an S3-compatible bucket: AWS S3, Cloudflare R2,
// MinIO, Backblaze B2, Google Cloud Storage (interoperability mode)… It
// speaks the S3 REST API directly with Signature Version 4.
//
// Writes are conditional (If-None-Match: *), so a published zip is never
// overwritten, even by two servers racing.
type S3 struct {
	Endpoint  string // e.g. https://s3.us-east-1.amazonaws.com or https://<account>.r2.cloudflarestorage.com
	Region    string // "auto" for R2
	Bucket    string
	Prefix    string // key prefix inside the bucket, e.g. "gopherdex/"
	PathStyle bool   // https://endpoint/bucket/key instead of https://bucket.endpoint/key
	AccessKey string
	SecretKey string
	Session   string // temporary credentials' session token, if any
	HTTP      *http.Client
	Now       func() time.Time
}

// maxMemoryBlob is the largest blob Open keeps in memory; bigger ones go
// to a temporary file.
const maxMemoryBlob = 16 << 20

// ParseS3 configures a store from a URL like
//
//	s3://bucket/prefix?region=us-east-1
//	s3://bucket?endpoint=https://<account>.r2.cloudflarestorage.com&region=auto
//	s3://bucket?endpoint=http://localhost:9000   (MinIO)
//
// Credentials come from AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and
// AWS_SESSION_TOKEN, the variables every S3 tool reads.
func ParseS3(raw string, getenv func(string) string) (*S3, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return nil, fmt.Errorf("blob store %q: want s3://bucket[/prefix][?region=…&endpoint=…]", raw)
	}
	q := u.Query()
	s := &S3{
		Bucket:    u.Host,
		Prefix:    strings.Trim(u.Path, "/"),
		Region:    q.Get("region"),
		Endpoint:  strings.TrimSuffix(q.Get("endpoint"), "/"),
		AccessKey: getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: getenv("AWS_SECRET_ACCESS_KEY"),
		Session:   getenv("AWS_SESSION_TOKEN"),
	}
	if s.Prefix != "" {
		s.Prefix += "/"
	}
	if s.Region == "" {
		s.Region = getenv("AWS_REGION")
	}
	if s.Region == "" {
		s.Region = "us-east-1"
	}
	if s.Endpoint == "" {
		s.Endpoint = "https://s3." + s.Region + ".amazonaws.com"
	} else {
		s.PathStyle = true // custom endpoints (R2, MinIO…) all accept path-style
	}
	if v := q.Get("path_style"); v != "" {
		s.PathStyle = v == "true"
	}
	if eu, err := url.Parse(s.Endpoint); err != nil || (eu.Scheme != "https" && eu.Scheme != "http") || eu.Host == "" {
		return nil, fmt.Errorf("blob store %q: endpoint must be an http(s) URL", raw)
	}
	if s.AccessKey == "" || s.SecretKey == "" {
		return nil, errors.New("S3 blob store: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}
	return s, nil
}

func (s *S3) String() string {
	return "s3://" + s.Bucket + "/" + s.Prefix + " at " + s.Endpoint
}

func (s *S3) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (s *S3) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// objectURL returns the URL of key.
func (s *S3) objectURL(key string) *url.URL {
	u, _ := url.Parse(s.Endpoint)
	escaped := escapePath(s.Prefix + key)
	if s.PathStyle {
		u.Path = "/" + s.Bucket + "/" + s.Prefix + key
		u.RawPath = "/" + escapePath(s.Bucket) + "/" + escaped
	} else {
		u.Host = s.Bucket + "." + u.Host
		u.Path = "/" + s.Prefix + key
		u.RawPath = "/" + escaped
	}
	return u
}

// escapePath URI-encodes each path segment the way SigV4 expects.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = uriEncode(seg)
	}
	return strings.Join(segs, "/")
}

func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Put uploads r to key unless key exists. The data is spooled to a
// temporary file first, to learn its size and SHA-256, which S3 needs up
// front and which lets the upload be signed over its content.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, maxSize int64) (Info, error) {
	if err := checkKey(key); err != nil {
		return Info{}, err
	}
	tmp, err := os.CreateTemp("", "gopherdex-blob-*")
	if err != nil {
		return Info{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), &ctxReader{ctx: ctx, r: io.LimitReader(r, maxSize+1)})
	if err != nil {
		return Info{}, fmt.Errorf("write %s: %w", key, err)
	}
	if n > maxSize {
		return Info{}, fmt.Errorf("%s is over %d bytes: %w", key, maxSize, ErrTooLarge)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return Info{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(key).String(), tmp)
	if err != nil {
		return Info{}, err
	}
	req.ContentLength = n
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("If-None-Match", "*") // never overwrite
	s.sign(req, sum)
	resp, err := s.client().Do(req)
	if err != nil {
		return Info{}, fmt.Errorf("upload %s: %w", key, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return Info{Size: n, SHA256: sum}, nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return Info{}, fmt.Errorf("%s: %w", key, ErrExists)
	default:
		return Info{}, fmt.Errorf("upload %s: %s", key, s3Error(resp))
	}
}

// Open downloads key. Module zips are small, and archive/zip reads them
// at many offsets, so the whole object is fetched once rather than with a
// range request per read.
func (s *S3) Open(ctx context.Context, key string) (Reader, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key).String(), nil)
	if err != nil {
		return nil, err
	}
	s.sign(req, emptySHA256)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", key, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	default:
		return nil, fmt.Errorf("download %s: %s", key, s3Error(resp))
	}
	if resp.ContentLength >= 0 && resp.ContentLength <= maxMemoryBlob {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxMemoryBlob+1))
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", key, err)
		}
		return &memReader{Reader: bytes.NewReader(data)}, nil
	}
	f, err := os.CreateTemp("", "gopherdex-blob-*")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name()) // unlinked: the space is freed when f is closed
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("download %s: %w", key, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return fileReader{File: f, size: n}, nil
}

// Keys lists the blobs under the store's prefix, for copying and checks.
func (s *S3) Keys(ctx context.Context, fn func(key string) error) error {
	token := ""
	for {
		u, _ := url.Parse(s.Endpoint)
		q := url.Values{"list-type": {"2"}, "prefix": {s.Prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		if s.PathStyle {
			u.Path = "/" + s.Bucket + "/"
		} else {
			u.Host = s.Bucket + "." + u.Host
			u.Path = "/"
		}
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		s.sign(req, emptySHA256)
		resp, err := s.client().Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("list %s: %s", s.Bucket, s3ErrorText(resp.Status, body))
		}
		var page listResult
		if err := xmlUnmarshal(body, &page); err != nil {
			return fmt.Errorf("list %s: %w", s.Bucket, err)
		}
		for _, c := range page.Contents {
			if err := fn(strings.TrimPrefix(c.Key, s.Prefix)); err != nil {
				return err
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return nil
		}
		token = page.NextContinuationToken
	}
}

type listResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

type memReader struct{ *bytes.Reader }

func (m *memReader) Close() error { return nil }
func (m *memReader) Size() int64  { return m.Reader.Size() }

func s3Error(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return s3ErrorText(resp.Status, body)
}

// s3ErrorText turns an S3 XML error into "403 Forbidden: Code (message)".
func s3ErrorText(status string, body []byte) string {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xmlUnmarshal(body, &e) == nil && e.Code != "" {
		return fmt.Sprintf("%s: %s (%s)", status, e.Code, e.Message)
	}
	return status
}

// ---- Signature Version 4 ----

var emptySHA256 = hex.EncodeToString(func() []byte { h := sha256.Sum256(nil); return h[:] }())

// sign adds SigV4 headers to req, whose body hashes to payloadSHA256.
// https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html
func (s *S3) sign(req *http.Request, payloadSHA256 string) {
	now := s.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadSHA256)
	if s.Session != "" {
		req.Header.Set("X-Amz-Security-Token", s.Session)
	}
	req.Header.Set("Host", req.URL.Host)

	var names []string
	for name := range req.Header {
		names = append(names, strings.ToLower(name))
	}
	names = append(names, "host")
	sort.Strings(names)
	names = dedupe(names)
	var canonHeaders strings.Builder
	for _, name := range names {
		v := req.Host
		if name != "host" {
			v = strings.Join(req.Header.Values(http.CanonicalHeaderKey(name)), ",")
		} else if v == "" {
			v = req.URL.Host
		}
		canonHeaders.WriteString(name + ":" + strings.TrimSpace(v) + "\n")
	}
	signed := strings.Join(names, ";")

	// Canonical query: keys and values URI-encoded, sorted by key.
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonQuery []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			canonQuery = append(canonQuery, uriEncode(k)+"="+uriEncode(v))
		}
	}
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, strings.Join(canonQuery, "&"), canonHeaders.String(), signed, payloadSHA256}, "\n")
	scope := day + "/" + s.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA256([]byte("AWS4"+s.SecretKey), day)
	key = hmacSHA256(key, s.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
	req.Header.Del("Host") // net/http sends req.Host itself
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func xmlUnmarshal(data []byte, v any) error { return xml.Unmarshal(data, v) }

// Check verifies the bucket is reachable with the configured credentials,
// so a wrong setting fails at start-up rather than on the first publish.
func (s *S3) Check(ctx context.Context) error {
	stop := errors.New("stop")
	err := s.Keys(ctx, func(string) error { return stop })
	if err != nil && !errors.Is(err, stop) {
		return fmt.Errorf("S3 blob store %s: %w", s, err)
	}
	return nil
}
