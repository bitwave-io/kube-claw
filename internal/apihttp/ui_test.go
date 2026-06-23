package apihttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func TestUIIntake(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := secrets.NewLocalCipher(filepath.Join(dir, "master.keyset"))
	svc := &secrets.Service{Store: st, Cipher: cipher}
	if _, err := svc.CreateSecret(ctx, "claw-agents", "k", "generic", "", nil); err != nil {
		t.Fatal(err)
	}
	tok, err := svc.MintIntakeToken(ctx, "claw-agents", "k", "")
	if err != nil {
		t.Fatal(err)
	}

	ui := &UIServer{Secrets: svc}
	h := ui.handler()

	// GET form renders (does NOT consume the token).
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/secret-intake/"+tok, nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "textarea") {
		t.Fatalf("form = %d body=%s", rr.Code, rr.Body)
	}

	// POST submits the value (form-encoded).
	post := func(token, value string) int {
		form := url.Values{"value": {value}}
		req := httptest.NewRequest(http.MethodPost, "/ui/secret-intake/"+token, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := post(tok, `{"private_key":"x"}`); code != 200 {
		t.Fatalf("submit = %d", code)
	}
	// reuse → generic 404 (single-use, no oracle)
	if code := post(tok, "again"); code != 404 {
		t.Fatalf("reuse = %d, want 404", code)
	}
	// bogus token → generic 404
	if code := post("deadbeef", "x"); code != 404 {
		t.Fatalf("bogus = %d, want 404", code)
	}
	// empty value → 400
	if code := post(tok, ""); code != 400 {
		t.Fatalf("empty value = %d, want 400", code)
	}
}
