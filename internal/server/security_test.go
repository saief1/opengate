package server

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/provider"
)

func TestClientIPRightmostUntrustedHop(t *testing.T) {
	mk := func(trust bool, cidrs ...string) *Server {
		s := &Server{cfg: &config.Config{TrustProxyHeaders: trust}}
		for _, c := range cidrs {
			_, n, _ := net.ParseCIDR(c)
			s.trusted = append(s.trusted, n)
		}
		return s
	}
	cases := []struct {
		s          *Server
		peer, xff  string
		want, name string
	}{
		{mk(false), "10.0.0.5:1", "1.1.1.1", "10.0.0.5", "proxy headers off"},
		{mk(true), "10.0.0.5:1", "6.6.6.6, 203.0.113.9", "203.0.113.9", "one proxy: rightmost hop, spoofed left ignored"},
		{mk(true, "10.0.0.0/8"), "10.0.0.5:1", "6.6.6.6, 203.0.113.9, 10.0.0.7", "203.0.113.9", "skip trusted hops"},
		{mk(true, "10.0.0.0/8"), "198.51.100.3:1", "6.6.6.6", "198.51.100.3", "XFF from an untrusted peer is ignored"},
		{mk(true), "10.0.0.5:1", "garbage", "10.0.0.5", "unparseable hop falls back to peer"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		r.Header.Set("X-Forwarded-For", c.xff)
		if got := c.s.clientIP(r); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestLockoutIsBoundedAndExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLockout(3, time.Minute, 10*time.Minute)
	l.now = func() time.Time { return now }
	l.cap = 100
	for i := 0; i < 1000; i++ {
		l.fail("ip-" + itoa(i))
	}
	if n := l.size(); n > 100 {
		t.Fatalf("table grew to %d", n)
	}
	// A locked entry survives eviction pressure; stale ones expire.
	for i := 0; i < 3; i++ {
		l.fail("attacker")
	}
	for i := 0; i < 500; i++ {
		l.fail("spray-" + itoa(i))
	}
	if locked, _ := l.locked("attacker"); !locked {
		t.Fatal("lock evicted by spraying")
	}
	now = now.Add(11 * time.Minute)
	l.fail("fresh")
	if locked, _ := l.locked("attacker"); locked {
		t.Fatal("lock must expire")
	}
	if n := l.size(); n > 2 {
		t.Fatalf("expired entries kept: %d", n)
	}
}

func TestNormalizeMaxTokens(t *testing.T) {
	a, b := 10, 900
	req := &provider.ChatRequest{MaxCompletionTokens: &a, MaxTokens: &b}
	raw := map[string]json.RawMessage{"max_completion_tokens": json.RawMessage("10"), "max_tokens": json.RawMessage("900")}
	normalizeMaxTokens(req, raw)
	if req.MaxOutputTokens() != 900 || req.MaxTokens != nil || string(raw["max_completion_tokens"]) != "900" || raw["max_tokens"] != nil {
		t.Fatalf("not normalized: %+v %s", req, raw)
	}
	for _, c := range []struct {
		t      string
		hosted bool
	}{{"function", false}, {"custom", false}, {"web_search", true}, {"file_search", true}, {"code_interpreter", true}, {"image_generation", true}, {"mcp", true}} {
		if hostedTool(c.t) != c.hosted {
			t.Errorf("%s: hosted=%v", c.t, !c.hosted)
		}
	}
}

// v1.0.1 N3: argon2id work is bounded; a saturated gate answers 429.
func TestPasswordGateSaturates(t *testing.T) {
	s := &Server{pwSem: make(chan struct{}, 1)}
	r := httptest.NewRequest("POST", "/admin/api/auth/login", nil)
	if !s.pwSlot(httptest.NewRecorder(), r) {
		t.Fatal("first slot must be granted")
	}
	w := httptest.NewRecorder()
	start := time.Now()
	if s.pwSlot(w, r) || w.Code != 429 || time.Since(start) < time.Second {
		t.Fatalf("saturated gate: code=%d after %v", w.Code, time.Since(start))
	}
	s.pwFree()
	if !s.pwSlot(httptest.NewRecorder(), r) {
		t.Fatal("slot must be reusable after free")
	}
}
