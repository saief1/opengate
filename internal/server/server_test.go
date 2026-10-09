package server

import (
	"testing"
	"time"

	"github.com/opengate-dev/opengate/internal/store"
)

func TestPolicyMatching(t *testing.T) {
	p := store.ModelPolicy{AllowedModels: []string{"openai/*", "smart"}, DeniedModels: []string{"openai/o1*"}}
	cases := []struct {
		req  string
		t    targetSpec
		want bool
	}{
		{"openai/gpt-4o", targetSpec{"openai", "gpt-4o"}, true},
		{"openai/o1-pro", targetSpec{"openai", "o1-pro"}, false},
		{"smart", targetSpec{"anthropic", "claude-sonnet-4-5"}, true}, // allowed alias covers its targets
		{"anthropic/claude-sonnet-4-5", targetSpec{"anthropic", "claude-sonnet-4-5"}, false},
	}
	for _, c := range cases {
		if got := policyAllows(p, c.req, c.t); got != c.want {
			t.Errorf("%s -> %s: got %v", c.req, c.t, got)
		}
	}
	if !policyAllows(store.ModelPolicy{}, "x", targetSpec{"a", "b"}) {
		t.Fatal("empty policy allows everything")
	}
}

func TestLockout(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLockout(3, time.Minute, 10*time.Minute)
	l.now = func() time.Time { return now }
	l.fail("1.2.3.4")
	l.fail("1.2.3.4")
	now = now.Add(2 * time.Minute) // failures age out of the window
	l.fail("1.2.3.4")
	if locked, _ := l.locked("1.2.3.4"); locked {
		t.Fatal("old failures must not count")
	}
	l.fail("1.2.3.4")
	if !l.fail("1.2.3.4") {
		t.Fatal("third failure in window should lock")
	}
	if locked, d := l.locked("1.2.3.4"); !locked || d != 10*time.Minute {
		t.Fatalf("locked=%v d=%v", locked, d)
	}
	if locked, _ := l.locked("5.6.7.8"); locked {
		t.Fatal("other IPs unaffected")
	}
	l.success("1.2.3.4") // success during lockout does not unlock
	if locked, _ := l.locked("1.2.3.4"); !locked {
		t.Fatal("still locked")
	}
	now = now.Add(11 * time.Minute)
	if locked, _ := l.locked("1.2.3.4"); locked {
		t.Fatal("lock expires")
	}
}
