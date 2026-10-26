// Package mailer sends transactional email. A Resend API key is preferred;
// SMTP is the fallback; with neither set, messages are written to the log
// so local development works without an account.
package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// Message is one plain-text email.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Sender delivers a message. Configured is false for the log-only sender,
// which means the process should not block sign-in on email verification.
type Sender interface {
	Send(ctx context.Context, m Message) error
	Configured() bool
}

// FromEnv picks Resend (RESEND_API_KEY), then SMTP (SMTP_HOST), then a
// log-only sender. MAIL_FROM is the From address (RESEND_FROM and SMTP_FROM
// are accepted aliases).
func FromEnv(log *slog.Logger) Sender {
	if log == nil {
		log = slog.Default()
	}
	from := first(os.Getenv("MAIL_FROM"), os.Getenv("RESEND_FROM"), os.Getenv("SMTP_FROM"))
	if key := strings.TrimSpace(os.Getenv("RESEND_API_KEY")); key != "" {
		return &Resend{APIKey: key, From: from, Log: log, HTTP: &http.Client{Timeout: 15 * time.Second}}
	}
	if host := strings.TrimSpace(os.Getenv("SMTP_HOST")); host != "" {
		port := strings.TrimSpace(os.Getenv("SMTP_PORT"))
		if port == "" {
			port = "587"
		}
		return &SMTP{
			Host: host, Port: port, From: from, Log: log,
			User: os.Getenv("SMTP_USER"), Password: os.Getenv("SMTP_PASSWORD"),
		}
	}
	return &Log{Log: log}
}

func first(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Log writes the message to the logger and does not deliver it.
type Log struct{ Log *slog.Logger }

func (l *Log) Configured() bool { return false }

func (l *Log) Send(_ context.Context, m Message) error {
	l.Log.Info("mail (no provider configured; not sent)", "to", m.To, "subject", m.Subject, "body", m.Text)
	return nil
}

// Resend sends through https://api.resend.com.
type Resend struct {
	APIKey string
	From   string
	Log    *slog.Logger
	HTTP   *http.Client
	// BaseURL overrides the API host (tests).
	BaseURL string
}

func (r *Resend) Configured() bool { return true }

func (r *Resend) Send(ctx context.Context, m Message) error {
	if strings.TrimSpace(r.From) == "" {
		return fmt.Errorf("MAIL_FROM is required when RESEND_API_KEY is set")
	}
	if strings.TrimSpace(m.To) == "" {
		return fmt.Errorf("missing recipient")
	}
	payload, err := json.Marshal(map[string]any{
		"from": r.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Text,
	})
	if err != nil {
		return err
	}
	base := r.BaseURL
	if base == "" {
		base = "https://api.resend.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/emails", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	c := r.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("resend HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// SMTP delivers via a submission server (STARTTLS, port 587 by default).
type SMTP struct {
	Host, Port, User, Password, From string
	Log                              *slog.Logger
}

func (s *SMTP) Configured() bool { return true }

func (s *SMTP) Send(_ context.Context, m Message) error {
	if strings.TrimSpace(s.From) == "" {
		return fmt.Errorf("MAIL_FROM is required when SMTP_HOST is set")
	}
	addr := s.Host + ":" + s.Port
	var auth smtp.Auth
	if s.User != "" {
		auth = smtp.PlainAuth("", s.User, s.Password, s.Host)
	}
	msg := []byte("From: " + s.From + "\r\n" +
		"To: " + m.To + "\r\n" +
		"Subject: " + m.Subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" + m.Text)
	return smtp.SendMail(addr, auth, s.From, []string{m.To}, msg)
}
