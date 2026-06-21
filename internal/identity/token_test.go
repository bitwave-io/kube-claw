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
