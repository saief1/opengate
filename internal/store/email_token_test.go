package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/saief1/opengate/internal/secure"
)

func TestEmailTokensSingleUseAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	hash, err := secure.HashPassword("a-long-password")
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(ctx, "owner@example.com", "Owner", hash)
	if err != nil {
		t.Fatal(err)
	}
	if !u.EmailVerified {
		t.Fatal("operator-created users start verified")
	}
	plain, err := s.IssueEmailToken(ctx, u.ID, EmailTokenReset, time.Hour)
	if err != nil || plain == "" {
		t.Fatal(err)
	}
	older := plain
	plain, err = s.IssueEmailToken(ctx, u.ID, EmailTokenReset, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeEmailToken(ctx, EmailTokenReset, older, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("older token still valid: %v", err)
	}
	id, err := s.ConsumeEmailToken(ctx, EmailTokenReset, plain, time.Now())
	if err != nil || id != u.ID {
		t.Fatalf("consume: %v %s", err, id)
	}
	if _, err := s.ConsumeEmailToken(ctx, EmailTokenReset, plain, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token reused: %v", err)
	}
	short, err := s.IssueEmailToken(ctx, u.ID, EmailTokenVerify, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeEmailToken(ctx, EmailTokenVerify, short, time.Now().Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired token accepted: %v", err)
	}
	if _, err := s.ConsumeEmailToken(ctx, EmailTokenReset, short, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatal("purpose must match")
	}
}
