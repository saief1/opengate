package mailer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFromEnvSelection(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("SMTP_HOST", "")
	if _, ok := FromEnv(log).(*Log); !ok {
		t.Fatal("expected log mailer when unset")
	}
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_FROM", "OpenGate <noreply@example.com>")
	s, ok := FromEnv(log).(*SMTP)
	if !ok || s.Host != "smtp.example.com" || s.Port != "587" || s.From == "" || !s.Configured() {
		t.Fatalf("smtp mailer: %#v", FromEnv(log))
	}
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("MAIL_FROM", "OpenGate <noreply@opengatehq.com>")
	r, ok := FromEnv(log).(*Resend)
	if !ok || !r.Configured() || r.From != "OpenGate <noreply@opengatehq.com>" {
		t.Fatalf("resend should win over SMTP: %#v", FromEnv(log))
	}
}

func TestResendSend(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/emails" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"em_1"}`))
	}))
	defer srv.Close()
	r := &Resend{APIKey: "re_test", From: "OpenGate <noreply@example.com>", BaseURL: srv.URL, HTTP: srv.Client()}
	if err := r.Send(context.Background(), Message{To: "a@b.c", Subject: "Verify", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer re_test" || got["subject"] != "Verify" {
		t.Fatalf("auth %q body %v", auth, got)
	}
	r.From = ""
	if err := r.Send(context.Background(), Message{To: "a@b.c"}); err == nil {
		t.Fatal("missing from must fail")
	}
}

func TestLogDoesNotClaimDelivery(t *testing.T) {
	l := &Log{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if l.Configured() {
		t.Fatal("log mailer is not a delivery provider")
	}
	if err := l.Send(context.Background(), Message{To: "a@b.c", Subject: "s", Text: "body"}); err != nil {
		t.Fatal(err)
	}
}
