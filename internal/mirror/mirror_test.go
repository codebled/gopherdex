package mirror

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestNotify(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	fails := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/proxy/gopherdex.dev/alice/!retry/@v/v1.0.0.info" && fails > 0 {
			fails--
			http.Error(w, "not found: fetching", http.StatusNotFound)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	n := &Notifier{
		ProxyURL: srv.URL + "/proxy", SumDBURL: srv.URL + "/sumdb",
		HTTP: srv.Client(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backoff: time.Millisecond,
	}
	n.Notify("gopherdex.dev/alice/Retry", "v1.0.0")
	n.Wait()

	want := []string{
		"/proxy/gopherdex.dev/alice/!retry/@v/v1.0.0.info",
		"/proxy/gopherdex.dev/alice/!retry/@v/v1.0.0.info",
		"/sumdb/lookup/gopherdex.dev/alice/!retry@v1.0.0",
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("requests = %v\nwant %v", seen, want)
	}
}

func TestNotifyStopsOnClientError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()
	n := &Notifier{ProxyURL: srv.URL, SumDBURL: srv.URL, HTTP: srv.Client(), Backoff: time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	n.Notify("gopherdex.dev/alice/retry", "v1.0.0")
	n.Wait()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry, no checksum lookup after a 400)", calls)
	}
}
