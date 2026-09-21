package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

const maxFieldBytes = 1024

// handleUpload publishes a module version. It takes multipart/form-data with
// the text fields module, version and optionally repository, commit and ref,
// followed by a "zip" file part holding the module zip.
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	u, tok, ok := s.bearerUser(w, r)
	if !ok {
		return
	}
	if !s.uploadLimit.Allow("user:" + strconv.FormatInt(u.ID, 10)) {
		w.Header().Set("Retry-After", "600")
		writeJSON(w, http.StatusTooManyRequests, apiError{"You've published a lot in the last hour. Try again in a few minutes."})
		return
	}
	maxZip := s.registry.MaxZipSize
	if maxZip <= 0 {
		maxZip = registry.DefaultMaxZipSize
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		writeJSON(w, http.StatusUnsupportedMediaType, apiError{"Send the upload as multipart/form-data."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxZip+64<<10)
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{"The upload isn't valid multipart/form-data."})
		return
	}

	up := registry.Upload{User: u, Token: tok, Client: s.clientOf(r)}
	fields := map[string]*string{
		"module": &up.Module, "version": &up.Version,
		"repository": &up.Repository, "commit": &up.Commit, "ref": &up.Ref,
	}
	defer func() {
		if up.ZipFile != "" {
			os.Remove(up.ZipFile)
		}
	}()

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadReadError(w, r, err, maxZip)
			return
		}
		name := part.FormName()
		switch {
		case fields[name] != nil:
			b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if err != nil {
				s.uploadReadError(w, r, err, maxZip)
				return
			}
			if len(b) > maxFieldBytes {
				writeJSON(w, http.StatusBadRequest, apiError{fmt.Sprintf("Field %q is too long.", name)})
				return
			}
			*fields[name] = string(b)
		case name == "zip":
			if up.ZipFile != "" {
				writeJSON(w, http.StatusBadRequest, apiError{"Send exactly one zip part."})
				return
			}
			tmp, err := os.CreateTemp("", "gopherdex-upload-*.zip")
			if err != nil {
				s.apiError(w, r, fmt.Errorf("create upload file: %w", err))
				return
			}
			up.ZipFile = tmp.Name()
			n, err := io.Copy(tmp, io.LimitReader(part, maxZip+1))
			if closeErr := tmp.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				s.uploadReadError(w, r, err, maxZip)
				return
			}
			if n > maxZip {
				writeJSON(w, http.StatusRequestEntityTooLarge, apiError{fmt.Sprintf("The module zip is over the %d byte limit.", maxZip)})
				return
			}
		default:
			writeJSON(w, http.StatusBadRequest, apiError{fmt.Sprintf("Unknown form field %q.", name)})
			return
		}
	}
	if up.Module == "" || up.Version == "" || up.ZipFile == "" {
		writeJSON(w, http.StatusBadRequest, apiError{"The upload needs module, version and zip fields."})
		return
	}

	pub, err := s.registry.Publish(r.Context(), up)
	var re *registry.Error
	switch {
	case errors.As(err, &re):
		writeJSON(w, re.Status, struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}{re.Message, re.Code})
		return
	case err != nil:
		s.apiError(w, r, err)
		return
	}

	status := http.StatusCreated
	if pub.AlreadyPublished {
		status = http.StatusOK
	} else {
		s.notifyPublished(pub.Module, pub.Version, u, tok, up.Client.IP)
		if len(pub.Warnings) > 0 {
			s.notifyFindings(pub)
		}
		if s.mirror != nil {
			s.mirror.Notify(pub.Module, pub.Version)
		}
	}
	writeJSON(w, status, struct {
		*registry.Published
		URL      string `json:"url"`
		ProxyURL string `json:"proxyURL"`
		Install  string `json:"install"`
	}{pub, s.siteURL + s.project.URL(pub.Module, pub.Version), s.siteURL + proxyPrefix,
		s.installCommand(pub.Module, pub.Version)})
}

func (s *server) uploadReadError(w http.ResponseWriter, r *http.Request, err error, maxZip int64) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeJSON(w, http.StatusRequestEntityTooLarge, apiError{fmt.Sprintf("The upload is over the %d byte limit.", maxZip)})
		return
	}
	if r.Context().Err() != nil {
		return
	}
	s.log.Warn("read upload", "err", err)
	writeJSON(w, http.StatusBadRequest, apiError{"The upload was interrupted or malformed. Try again."})
}

// notifyFindings tells maintainers and administrators what the publish
// checks flagged. Maintainers always get it: it's about their code.
func (s *server) notifyFindings(pub *registry.Published) {
	var lines []string
	for _, f := range pub.Warnings {
		lines = append(lines, "  - "+f.String())
	}
	list := strings.Join(lines, "\n")
	page := s.siteURL + s.project.URL(pub.Module, "") + "?tab=manage"
	s.emailMaintainers(pub.Module, fmt.Sprintf("Publish checks flagged %s %s", pub.Module, pub.Version),
		fmt.Sprintf("%s@%s is published, but the registry's automated checks flagged it for review:\n\n%s\n\nAn administrator will look at it. If this is expected, there's nothing to do. Details: %s", pub.Module, pub.Version, list, page))
	var admins []string
	for name := range s.admins {
		admins = append(admins, name)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		emails, err := s.accounts.UserEmails(ctx, admins)
		if err != nil {
			s.log.Error("email admins", "err", err)
			return
		}
		for name, email := range emails {
			msg := mail.Message{To: email, Subject: "Review queue: " + pub.Module + " " + pub.Version,
				Body: "Hi " + name + ",\n\nThe publish checks flagged " + pub.Module + "@" + pub.Version + ":\n\n" + list + "\n\nReview it at " + s.siteURL + "/admin\n"}
			if err := s.accounts.Mailer.Send(ctx, msg); err != nil {
				s.log.Error("email admin", "user", name, "err", err)
			}
		}
	}()
}
