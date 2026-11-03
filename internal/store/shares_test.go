package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCreateAndGetShare(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	sh, err := s.CreateShare(ctx, "tab=logs&app=web&range=1d", "crash=12", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if sh.Token == "" || sh.QS != "tab=logs&app=web&range=1d" || sh.Hash != "crash=12" {
		t.Fatalf("share = %+v", sh)
	}
	if time.Until(time.UnixMilli(sh.ExpiresMs)) < 23*time.Hour {
		t.Errorf("expires_ms too soon: %v", time.Until(time.UnixMilli(sh.ExpiresMs)))
	}
	got, err := s.GetShare(ctx, sh.Token)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Token != sh.Token || got.QS != sh.QS || got.Hash != sh.Hash {
		t.Errorf("round trip = %+v", got)
	}
}

func TestShareNothingToShare(t *testing.T) {
	s := open(t)
	if _, err := s.CreateShare(context.Background(), "", "", time.Hour); err == nil {
		t.Fatal("empty qs/hash must be refused")
	}
}

func TestShareExpiryStopsResolving(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	// Negative ttl: already-expired row (test hook, see shareTTL).
	sh, err := s.CreateShare(ctx, "tab=logs", "", -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetShare(ctx, sh.Token); err == nil {
		t.Fatal("expired link must stop resolving")
	}
	// Unknown tokens fail the same way.
	if _, err := s.GetShare(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("unknown token must fail")
	}
}

func TestShareSweepDropsExpired(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old, err := s.CreateShare(ctx, "tab=logs", "", -time.Hour) // already expired
	if err != nil {
		t.Fatal(err)
	}
	keep, err := s.CreateShare(ctx, "tab=logs&fresh=1", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateShare(ctx, "trigger", "", time.Hour); err != nil { // sweep on create
		t.Fatal(err)
	}
	if _, err := s.GetShare(ctx, old.Token); err == nil {
		t.Error("expired link should have been swept")
	}
	if _, err := s.GetShare(ctx, keep.Token); err != nil {
		t.Errorf("live link lost during sweep: %v", err)
	}
}

func TestShareTokenShape(t *testing.T) {
	s := open(t)
	sh, err := s.CreateShare(context.Background(), "tab=logs", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Token) != 32 || strings.ToLower(sh.Token) != sh.Token {
		t.Errorf("token shape = %q, want 32 lowercase hex chars", sh.Token)
	}
}
