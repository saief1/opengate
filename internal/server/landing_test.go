package server

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/saief1/opengate/internal/config"
)

const landingFixture = `<link rel="icon" href="favicon.svg"><link rel="stylesheet" href="landing.css"><a href="/signup">Start free</a><span class="pill x43">Recommended</span><a class="b x44" href="/signup?plan=team">Start 14‑day trial</a><a href="#pricing">p</a><a href="https://x.dev">x</a><script src="landing.js"></script>`

func landingServer(disabled bool) *Server {
	cfg := &config.Config{}
	cfg.Cloud.BillingDisabled = disabled
	return &Server{cfg: cfg, webFS: fstest.MapFS{"landing.html": {Data: []byte(landingFixture)}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestRootServesLanding(t *testing.T) {
	rec := httptest.NewRecorder()
	landingServer(false).serveLanding(rec, httptest.NewRequest("GET", "/", nil))
	b := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, want := range []string{`href="/ui/landing.css"`, `href="/ui/favicon.svg"`, `src="/ui/landing.js"`, `href="#pricing"`, `href="https://x.dev"`, `/signup?plan=team`} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
}

func TestLandingBillingDisabled(t *testing.T) {
	rec := httptest.NewRecorder()
	landingServer(true).serveLanding(rec, httptest.NewRequest("GET", "/", nil))
	b := rec.Body.String()
	if strings.Contains(b, "plan=") || !strings.Contains(b, "Coming soon") {
		t.Fatalf("paid CTA not neutralized: %s", b)
	}
}

func TestPricingBillingDisabled(t *testing.T) {
	rec := httptest.NewRecorder()
	landingServer(true).pagePricing(rec, httptest.NewRequest("GET", "/pricing", nil))
	if b := rec.Body.String(); !strings.Contains(b, "Coming soon") || !strings.Contains(b, "/signup") {
		t.Fatalf("pricing: %s", b)
	}
}
