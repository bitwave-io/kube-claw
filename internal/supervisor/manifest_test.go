package supervisor

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const goodManifest = `{
  "schemaVersion": 1, "channel": "stable", "version": "v0.5.0",
  "images": {
    "controller": "docker.io/bitwavecode/kube-claw-controller@sha256:aaa",
    "runner": "docker.io/bitwavecode/kube-claw-runner@sha256:bbb"
  },
  "notes": "test"
}`

// manifestServer serves a manifest and (optionally) its signature.
func manifestServer(t *testing.T, manifest string, sig []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest-stable.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(manifest))
	})
	if sig != nil {
		mux.HandleFunc("/manifest-stable.json.sig", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(sig)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	return pub, priv, pemStr
}

// TestFetchManifestSigned: signature round-trip (raw and base64), tampering
// rejected, missing signature fail-closed, unsigned mode without a key.
func TestFetchManifestSigned(t *testing.T) {
	ctx := context.Background()
	pub, priv, pemStr := testKeypair(t)
	sig := ed25519.Sign(priv, []byte(goodManifest))

	// PEM parsing round-trip (the env/values path).
	parsed, err := ParseManifestPublicKeys(pemStr)
	if err != nil || len(parsed) != 1 || !parsed[0].Equal(pub) {
		t.Fatalf("ParseManifestPublicKeys: %v (%d keys)", err, len(parsed))
	}
	if k, err := ParseManifestPublicKeys(""); err != nil || k != nil {
		t.Fatalf("empty key = (%v, %v), want unsigned mode", k, err)
	}
	if _, err := ParseManifestPublicKeys("not pem"); err == nil {
		t.Fatal("garbage key must error")
	}

	// Valid raw signature verifies.
	srv := manifestServer(t, goodManifest, sig)
	m, err := FetchManifestSigned(ctx, srv.URL+"/manifest-stable.json", []ed25519.PublicKey{pub})
	if err != nil {
		t.Fatalf("signed fetch: %v", err)
	}
	if m.Version != "v0.5.0" {
		t.Fatalf("version = %q", m.Version)
	}

	// Base64 signature (some pipelines encode) also verifies.
	srv64 := manifestServer(t, goodManifest, []byte(base64.StdEncoding.EncodeToString(sig)+"\n"))
	if _, err := FetchManifestSigned(ctx, srv64.URL+"/manifest-stable.json", []ed25519.PublicKey{pub}); err != nil {
		t.Fatalf("base64-signed fetch: %v", err)
	}

	// Tampered manifest → rejected.
	tampered := strings.Replace(goodManifest, "sha256:aaa", "sha256:evil", 1)
	srvBad := manifestServer(t, tampered, sig)
	if _, err := FetchManifestSigned(ctx, srvBad.URL+"/manifest-stable.json", []ed25519.PublicKey{pub}); err == nil ||
		!strings.Contains(err.Error(), "verification FAILED") {
		t.Fatalf("tampered manifest err = %v, want verification failure", err)
	}

	// Key configured but no signature published → fail closed.
	srvNoSig := manifestServer(t, goodManifest, nil)
	if _, err := FetchManifestSigned(ctx, srvNoSig.URL+"/manifest-stable.json", []ed25519.PublicKey{pub}); err == nil ||
		!strings.Contains(err.Error(), "refusing unsigned") {
		t.Fatalf("missing signature err = %v, want fail-closed", err)
	}

	// No key configured → unsigned mode accepts the same manifest.
	if _, err := FetchManifestSigned(ctx, srvNoSig.URL+"/manifest-stable.json", nil); err != nil {
		t.Fatalf("unsigned mode: %v", err)
	}

	// Wrong key → rejected.
	otherPub, _, _ := testKeypair(t)
	if _, err := FetchManifestSigned(ctx, srv.URL+"/manifest-stable.json", []ed25519.PublicKey{otherPub}); err == nil {
		t.Fatal("wrong key must fail verification")
	}

	// Rotation ring: the signature verifies against ANY configured key, so an
	// install carrying [retired, current] keeps working across a key rollover.
	if _, err := FetchManifestSigned(ctx, srv.URL+"/manifest-stable.json", []ed25519.PublicKey{otherPub, pub}); err != nil {
		t.Fatalf("key ring must verify with any member: %v", err)
	}

	// Concatenated PEM blocks parse as a ring.
	_, _, pem2 := testKeypair(t)
	if keys, err := ParseManifestPublicKeys(pemStr + pem2); err != nil || len(keys) != 2 {
		t.Fatalf("concatenated PEM ring = %d keys, err %v", len(keys), err)
	}
}
