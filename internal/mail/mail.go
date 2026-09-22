// Package mail sends account email: verification links now, publish
// notifications later.
package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Message is a plain-text email.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Mailer sends messages.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

func (m Message) validate() error {
	if strings.ContainsAny(m.To+m.Subject, "\r\n") {
		return errors.New("mail: header values must not contain line breaks")
	}
	if _, err := mail.ParseAddress(m.To); err != nil {
		return fmt.Errorf("mail: invalid recipient: %w", err)
	}
	return nil
}

// LogMailer writes messages to the log instead of sending them. Use it in
// development: verification links appear in the server output.
type LogMailer struct {
	Log *slog.Logger
}

// Send logs m at info level, body included, after the same checks
// SMTPMailer makes, so invalid messages fail in development too.
func (l LogMailer) Send(_ context.Context, m Message) error {
	if err := m.validate(); err != nil {
		return err
	}
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("email (not sent: no SMTP server configured)", "to", m.To, "subject", m.Subject, "body", m.Body)
	return nil
}

// SMTPMailer sends through an SMTP server. It upgrades to TLS with STARTTLS
// when the server offers it, and only authenticates over TLS.
type SMTPMailer struct {
	Addr     string // host:port, e.g. smtp.example.com:587
	From     string // e.g. "Gopherdex <no-reply@gopherdex.dev>"
	Username string
	Password string
}

// Send delivers m from s.From. It returns early with ctx's error if ctx
// ends first; the SMTP exchange itself can't be interrupted, so it finishes
// in the background.
func (s SMTPMailer) Send(ctx context.Context, m Message) error {
	if err := m.validate(); err != nil {
		return err
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("mail: invalid sender %q: %w", s.From, err)
	}
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return fmt.Errorf("mail: invalid SMTP address %q: %w", s.Addr, err)
	}

	var body strings.Builder
	fmt.Fprintf(&body, "From: %s\r\n", from.String())
	fmt.Fprintf(&body, "To: %s\r\n", m.To)
	fmt.Fprintf(&body, "Subject: %s\r\n", m.Subject)
	fmt.Fprintf(&body, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	body.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	body.WriteString(strings.ReplaceAll(m.Body, "\n", "\r\n"))

	var auth smtp.Auth
	if s.Username != "" {
		// PlainAuth refuses to send credentials over an unencrypted
		// connection to anything but localhost.
		auth = smtp.PlainAuth("", s.Username, s.Password, host)
	}
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(s.Addr, auth, from.Address, []string{m.To}, []byte(body.String())) }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("mail: send to %s: %w", m.To, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mail: send to %s: %w", m.To, ctx.Err())
	}
}
