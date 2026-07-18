// Command manifest-sign generates ed25519 release-manifest signing keys and
// detached signatures (DESIGN.md §24.3 / T-9). Stdlib-only so it runs anywhere
// Go does (macOS LibreSSL can't do ed25519 pkeyutl; CI's OpenSSL 3 can — both
// produce the same raw 64-byte signature the supervisor verifies).
//
//	manifest-sign -keygen -dir .                # writes manifest-signing.key/.pub (PEM)
//	manifest-sign -key manifest-signing.key -in manifest.json -out manifest.json.sig
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	keygen := flag.Bool("keygen", false, "generate a keypair instead of signing")
	dir := flag.String("dir", ".", "output directory for -keygen")
	keyPath := flag.String("key", "", "PEM PKCS8 ed25519 private key (signing mode)")
	in := flag.String("in", "", "file to sign")
	out := flag.String("out", "", "signature output path (raw 64 bytes)")
	flag.Parse()

	if *keygen {
		if err := doKeygen(*dir); err != nil {
			fatal(err)
		}
		return
	}
	if *keyPath == "" || *in == "" || *out == "" {
		fatal(fmt.Errorf("signing mode needs -key, -in, -out (or use -keygen)"))
	}
	if err := doSign(*keyPath, *in, *out); err != nil {
		fatal(err)
	}
}

func doKeygen(dir string) error {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	keyFile := filepath.Join(dir, "manifest-signing.key")
	pubFile := filepath.Join(dir, "manifest-signing.pub")
	if err := os.WriteFile(keyFile,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pubFile,
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (KEEP SECRET — GitHub secret MANIFEST_SIGNING_KEY)\nwrote %s (Helm value updates.manifestPublicKey)\n", keyFile, pubFile)
	return nil
}

func doSign(keyPath, in, out string) error {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return fmt.Errorf("%s: not PEM", keyPath)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return err
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return fmt.Errorf("%s: %T is not ed25519", keyPath, parsed)
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	return os.WriteFile(out, ed25519.Sign(priv, data), 0o644)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
