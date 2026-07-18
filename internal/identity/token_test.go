package identity

import (
	"testing"
	"time"
)

func TestSessionToken(t *testing.T) {
	s, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}

	tok, err := s.Issue("run-1", []string{"gcp-billing"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.RunID != "run-1" || !c.Allows("gcp-billing") || c.Allows("other") {
		t.Fatalf("claims wrong: %+v", c)
	}

	// Tampered token → bad signature.
	if _, err := s.Verify(tok + "x"); err == nil {
		t.Fatal("tampered token verified")
	}
	// A different signer must not verify.
	other, _ := NewSigner()
	if _, err := other.Verify(tok); err == nil {
		t.Fatal("token verified with wrong key")
	}
	// Expired token → error.
	exp, _ := s.Issue("run-1", nil, -time.Second)
	if _, err := s.Verify(exp); err == nil {
		t.Fatal("expired token verified")
	}
}

func TestRefreshTokenKinds(t *testing.T) {
	s, err := NewSigner()
	if err != nil {
		t.Fatal(err)
	}

	access, _ := s.Issue("run-1", []string{"gcp-billing"}, time.Minute)
	refresh, err := s.IssueRefresh("run-1", "pod-a", "uid-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// A refresh token is not a bearer credential.
	if _, err := s.Verify(refresh); err == nil {
		t.Fatal("refresh token accepted as access token")
	}
	// An access token cannot drive the refresh exchange.
	if _, err := s.VerifyRefresh(access); err == nil {
		t.Fatal("access token accepted as refresh token")
	}

	c, err := s.VerifyRefresh(refresh)
	if err != nil {
		t.Fatalf("verify refresh: %v", err)
	}
	if c.RunID != "run-1" || c.Kind != KindRefresh || len(c.Secrets) != 0 {
		t.Fatalf("refresh claims wrong: %+v", c)
	}
	if c.PodName != "pod-a" || c.PodUID != "uid-1" {
		t.Fatalf("refresh token must carry its pod binding: %+v", c)
	}

	// Expired refresh token → error.
	exp, _ := s.IssueRefresh("run-1", "pod-a", "uid-1", -time.Second)
	if _, err := s.VerifyRefresh(exp); err == nil {
		t.Fatal("expired refresh token verified")
	}

	// Pre-kind tokens (no kind claim) still verify as access tokens.
	legacy, _ := s.issue(Claims{RunID: "run-1", Exp: time.Now().Add(time.Minute).Unix()})
	if _, err := s.Verify(legacy); err != nil {
		t.Fatalf("legacy kindless token rejected: %v", err)
	}
}
