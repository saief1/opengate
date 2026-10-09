package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/opengate-dev/opengate/internal/secure"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKeysAreStoredHashed(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p, err := s.EnsureProject(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	plain, k, err := s.CreateKey(ctx, p.ID, "bot", "researcher", "alice")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	s.db.QueryRow(`SELECT key_hash FROM virtual_keys WHERE id=?`, k.ID).Scan(&stored)
	if stored == plain || strings.Contains(stored, plain[3:]) || stored != secure.HashKey(plain) {
		t.Fatalf("key not stored as hash: %s", stored)
	}
	got, err := s.KeyByHash(ctx, secure.HashKey(plain))
	if err != nil || got.ID != k.ID || got.Agent != "researcher" {
		t.Fatalf("lookup: %v %+v", err, got)
	}
	if _, err := s.KeyByHash(ctx, secure.HashKey(plain+"x")); err != ErrNotFound {
		t.Fatal("expected not found")
	}
}

func TestProviderKeysEncrypted(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mk, _ := secure.GenerateMasterKey()
	kb, _ := secure.ParseMasterKey(mk)
	c, _ := secure.NewCipher(kb)
	pk, err := s.AddProviderKey(ctx, c, "openai", "prod", "sk-supersecret-1234")
	if err != nil {
		t.Fatal(err)
	}
	var raw sql.NullString
	s.db.QueryRow(`SELECT ciphertext FROM provider_keys WHERE id=?`, pk.ID).Scan(&raw)
	if strings.Contains(raw.String, "supersecret") {
		t.Fatal("plaintext provider key in DB")
	}
	list, _ := s.ListProviderKeys(ctx, "openai")
	pt, err := c.Decrypt(list[0].Ciphertext, ProviderKeyAAD(list[0].ID, "openai"))
	if err != nil || string(pt) != "sk-supersecret-1234" || list[0].Hint != "…1234" {
		t.Fatalf("decrypt: %v %q %q", err, pt, list[0].Hint)
	}
}

func TestSpendAggregation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()
	add := func(id, key, agent string, cost float64, ts time.Time) {
		if err := s.InsertLog(ctx, &RequestLog{ID: id, TS: ts, ProjectID: "p1", KeyID: key, Agent: agent, CostUSD: cost, Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	add("1", "k1", "a", 1.0, now)
	add("2", "k1", "a", 2.0, now.Add(-48*time.Hour))
	add("3", "k2", "b", 4.0, now)
	day := now.Add(-time.Hour)
	if v, _ := s.SpendSince(ctx, ScopeKey, "k1", day); v != 1.0 {
		t.Fatalf("key daily %v", v)
	}
	if v, _ := s.SpendSince(ctx, ScopeKey, "k1", time.Time{}); v != 3.0 {
		t.Fatalf("key total %v", v)
	}
	if v, _ := s.SpendSince(ctx, ScopeAgent, "p1/b", time.Time{}); v != 4.0 {
		t.Fatalf("agent %v", v)
	}
	if v, _ := s.SpendSince(ctx, ScopeProject, "p1", day); v != 5.0 {
		t.Fatalf("project %v", v)
	}
	sent, _ := s.MarkAlertSent(ctx, "key", "k1", "daily", day, 0.8)
	again, _ := s.MarkAlertSent(ctx, "key", "k1", "daily", day, 0.8)
	if !sent || again {
		t.Fatal("alert dedupe broken")
	}
}
