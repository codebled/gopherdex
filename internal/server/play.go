package server

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const maxPlaySize = 64 << 10

var playIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// handlePlay shares a documentation example with the Go Playground and
// sends the browser there to run it. Examples come from published source,
// so nothing private is shared.
func (s *server) handlePlay(w http.ResponseWriter, r *http.Request) {
	if !s.playground {
		s.notFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPlaySize+4<<10)
	if !parseForm(w, r) {
		return
	}
	code := r.PostFormValue("code")
	if code == "" || len(code) > maxPlaySize || !strings.Contains(code, "package main") {
		http.Error(w, "Send a complete program.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.playgroundURL+"/share", strings.NewReader(code))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		s.log.Warn("share with the Go Playground", "err", err)
		http.Error(w, "The Go Playground didn't answer. Try again in a moment.", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	id, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	if resp.StatusCode != http.StatusOK || !playIDPattern.Match(id) {
		s.log.Warn("share with the Go Playground", "status", resp.Status)
		http.Error(w, "The Go Playground couldn't take this example.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "https://go.dev/play/p/"+string(id), http.StatusSeeOther)
}
