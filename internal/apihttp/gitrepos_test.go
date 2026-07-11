package apihttp

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/traego/kube-claw/internal/store"
)

// TestGitRepoLifecycle walks the full request→approve→grant flow for the git-repo
// plane: an admin registers a repo with read+write credentials; an agent requests
// read access (not granted → access_requested), an approver grants it, and the
// agent retrieves the read credential. It then requests write, which is granted
// and materializes the DIFFERENT write credential. Read grants never see the
// write credential.
func TestGitRepoLifecycle(t *testing.T) {
	s := fullServer(t)
	h := s.handler()

	// Seed a run + session token for the test agent (claw-agents/gcp-cost,
	// digest sha256:abc, spec sha256:spec — see testAgent()).
	if err := s.Store.Tx(t.Context(), func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost",
			SessionID: "sess-1", Phase: "Running", Source: `{"trigger":"slack","user":"u9"}`})
	}); err != nil {
		t.Fatal(err)
	}
	tok, _ := s.Signer.Issue("run-1", nil, time.Hour)

	// Register the repo (admin surface). Credentials are write-only.
	rr := do(t, h, "POST", "/v1/gitrepos",
		`{"name":"infra","url":"https://github.com/acme/infra.git","description":"terraform",
		  "readCredential":"RO-KEY","writeCredential":"RW-KEY","granters":["u-boss"]}`)
	if rr.Code != 201 {
		t.Fatalf("createGitRepo = %d (%s)", rr.Code, rr.Body)
	}

	// List never leaks credentials.
	rr = do(t, h, "GET", "/v1/gitrepos", "")
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "RO-KEY") || strings.Contains(rr.Body.String(), "RW-KEY") {
		t.Fatalf("listGitRepos leaked or failed = %d (%s)", rr.Code, rr.Body)
	}

	// Missing credential is rejected.
	if rr := do(t, h, "POST", "/v1/gitrepos", `{"name":"x","url":"https://x"}`); rr.Code != 400 {
		t.Fatalf("create without credential = %d", rr.Code)
	}

	// Agent lists what it can request (name+url, never the credential).
	rr = doAuth(t, h, "GET", "/v1/runs/run-1/available-gitrepos", "", tok)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "infra") || strings.Contains(rr.Body.String(), "RO-KEY") {
		t.Fatalf("availableGitRepos = %d (%s)", rr.Code, rr.Body)
	}

	// Request read → not granted yet, opens an access request.
	rr = doAuth(t, h, "POST", "/v1/runs/run-1/request-gitrepo",
		`{"name":"infra","access":"read","reason":"read the tf modules"}`, tok)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "access_requested") {
		t.Fatalf("request-gitrepo(read) = %d (%s)", rr.Code, rr.Body)
	}

	// Nothing to retrieve while pending.
	if rr := doAuth(t, h, "GET", "/v1/runs/run-1/requested-gitrepo?name=infra", "", tok); rr.Code != 204 {
		t.Fatalf("requested-gitrepo while pending = %d", rr.Code)
	}

	// The request shows up as Pending; grab its id.
	rr = do(t, h, "GET", "/v1/gitrepo-requests?status=Pending", "")
	var reqs []store.GitRepoRequest
	_ = json.Unmarshal(rr.Body.Bytes(), &reqs)
	if len(reqs) != 1 || reqs[0].Access != "read" {
		t.Fatalf("pending requests = %v", reqs)
	}
	reqID := reqs[0].ID

	// Approve (break-glass CLI path).
	if rr := do(t, h, "POST", "/v1/gitrepo-requests/"+reqID+"/approve", `{"approver":"u-boss"}`); rr.Code != 200 {
		t.Fatalf("approve = %d (%s)", rr.Code, rr.Body)
	}

	// Now the agent retrieves the READ credential.
	rr = doAuth(t, h, "GET", "/v1/runs/run-1/requested-gitrepo?name=infra", "", tok)
	if rr.Code != 200 {
		t.Fatalf("requested-gitrepo after approve = %d (%s)", rr.Code, rr.Body)
	}
	got := decodeRepo(t, rr.Body.Bytes())
	if got["access"] != "read" || got["url"] != "https://github.com/acme/infra.git" || got["credential"] != "RO-KEY" {
		t.Fatalf("read materialization = %v", got)
	}

	// Requesting write is a separate, higher grant.
	rr = doAuth(t, h, "POST", "/v1/runs/run-1/request-gitrepo",
		`{"name":"infra","access":"write","reason":"push a fix"}`, tok)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "access_requested") {
		t.Fatalf("request-gitrepo(write) = %d (%s)", rr.Code, rr.Body)
	}
	rr = do(t, h, "GET", "/v1/gitrepo-requests?status=Pending", "")
	reqs = nil
	_ = json.Unmarshal(rr.Body.Bytes(), &reqs)
	if len(reqs) != 1 || reqs[0].Access != "write" {
		t.Fatalf("pending write request = %v", reqs)
	}
	if rr := do(t, h, "POST", "/v1/gitrepo-requests/"+reqs[0].ID+"/approve", `{"approver":"u-boss"}`); rr.Code != 200 {
		t.Fatalf("approve write = %d (%s)", rr.Code, rr.Body)
	}

	// Now materialization returns the WRITE credential (write implies read, so
	// the write grant wins in FindValidGitRepoGrant).
	rr = doAuth(t, h, "GET", "/v1/runs/run-1/requested-gitrepo?name=infra", "", tok)
	got = decodeRepo(t, rr.Body.Bytes())
	if got["access"] != "write" || got["credential"] != "RW-KEY" {
		t.Fatalf("write materialization = %v", got)
	}

	// Revoke every grant → materialization stops.
	rr = do(t, h, "GET", "/v1/gitrepo-grants?namespace=claw-agents&agent=gcp-cost", "")
	var grants []store.GitRepoGrant
	_ = json.Unmarshal(rr.Body.Bytes(), &grants)
	if len(grants) != 2 {
		t.Fatalf("expected 2 grants, got %v", grants)
	}
	for _, g := range grants {
		if rr := do(t, h, "POST", "/v1/gitrepo-grants/"+g.ID+"/revoke", `{"approver":"u-boss","reason":"done"}`); rr.Code != 200 {
			t.Fatalf("revoke = %d (%s)", rr.Code, rr.Body)
		}
	}
	if rr := doAuth(t, h, "GET", "/v1/runs/run-1/requested-gitrepo?name=infra", "", tok); rr.Code != 204 {
		t.Fatalf("requested-gitrepo after revoke = %d", rr.Code)
	}

	// Delete the repo → it disappears from the agent's available list.
	if rr := do(t, h, "DELETE", "/v1/gitrepos/infra?namespace=claw-agents", ""); rr.Code != 200 {
		t.Fatalf("delete = %d (%s)", rr.Code, rr.Body)
	}
	if rr := doAuth(t, h, "POST", "/v1/runs/run-1/request-gitrepo", `{"name":"infra"}`, tok); rr.Code != 404 {
		t.Fatalf("request after delete = %d", rr.Code)
	}
}

func TestGitRepoRequestValidation(t *testing.T) {
	s := fullServer(t)
	h := s.handler()
	_ = s.Store.Tx(t.Context(), func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost", Phase: "Running"})
	})
	tok, _ := s.Signer.Issue("run-1", nil, time.Hour)

	// Register a read-only repo (no write credential).
	if rr := do(t, h, "POST", "/v1/gitrepos",
		`{"name":"ro","url":"https://x","readCredential":"K"}`); rr.Code != 201 {
		t.Fatalf("create = %d (%s)", rr.Code, rr.Body)
	}
	// Requesting write against a repo with no write credential is a 400.
	if rr := doAuth(t, h, "POST", "/v1/runs/run-1/request-gitrepo", `{"name":"ro","access":"write"}`, tok); rr.Code != 400 {
		t.Fatalf("write on read-only repo = %d (%s)", rr.Code, rr.Body)
	}
	// Bogus access level is a 400.
	if rr := doAuth(t, h, "POST", "/v1/runs/run-1/request-gitrepo", `{"name":"ro","access":"admin"}`, tok); rr.Code != 400 {
		t.Fatalf("bogus access = %d", rr.Code)
	}
	// A foreign session token can't drive this run.
	other, _ := s.Signer.Issue("run-other", nil, time.Hour)
	if rr := doAuth(t, h, "POST", "/v1/runs/run-1/request-gitrepo", `{"name":"ro"}`, other); rr.Code != 401 {
		t.Fatalf("foreign token = %d", rr.Code)
	}
}

func TestGitRepoManagementRequiresAdmin(t *testing.T) {
	s := fullServer(t)
	s.AdminPassword = "hunter2"
	h := s.handler()

	if rr := do(t, h, "POST", "/v1/gitrepos", `{"name":"x","url":"https://x","readCredential":"K"}`); rr.Code != 401 {
		t.Fatalf("create without admin = %d", rr.Code)
	}
	if rr := do(t, h, "GET", "/v1/gitrepos", ""); rr.Code != 401 {
		t.Fatalf("list without admin = %d", rr.Code)
	}
	r := httptest.NewRequest("POST", "/v1/gitrepos", strings.NewReader(`{"name":"x","url":"https://x","readCredential":"K"}`))
	r.SetBasicAuth("admin", "hunter2")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != 201 {
		t.Fatalf("create with admin = %d (%s)", rr.Code, rr.Body)
	}
}

// TestGitRepoUI exercises the dashboard: register a repo, confirm it lists
// without leaking credentials, that an agent's access request surfaces on the
// requests page, and that approving it there works (break-glass).
func TestGitRepoUI(t *testing.T) {
	s := fullServer(t)
	h := s.handler()

	// Register via the UI form.
	form := url.Values{
		"name": {"infra"}, "url": {"https://github.com/acme/infra.git"},
		"description": {"tf"}, "readCredential": {"RO-KEY"}, "writeCredential": {"RW-KEY"},
		"granters": {"U_BOSS, U_LEAD"},
	}
	if rr := postForm(t, h, "/ui/gitrepos/create", form); rr.Code != 303 && rr.Code != 200 {
		t.Fatalf("ui create = %d (%s)", rr.Code, rr.Body)
	}

	// The list page shows the repo but never the credentials.
	rr := do(t, h, "GET", "/ui/gitrepos", "")
	b := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(b, "infra") || !strings.Contains(b, "U_BOSS") {
		t.Fatalf("gitReposPage = %d body=%s", rr.Code, b)
	}
	if strings.Contains(b, "RO-KEY") || strings.Contains(b, "RW-KEY") {
		t.Fatal("gitReposPage leaked a credential")
	}

	// An agent opens an access request; it should appear on /ui/requests.
	_ = s.Store.Tx(t.Context(), func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost",
			SessionID: "sess-1", Phase: "Running", Source: `{"trigger":"slack","user":"u9"}`})
	})
	tok, _ := s.Signer.Issue("run-1", nil, time.Hour)
	if rr := doAuth(t, h, "POST", "/v1/runs/run-1/request-gitrepo",
		`{"name":"infra","access":"write","reason":"push a fix"}`, tok); rr.Code != 200 {
		t.Fatalf("request-gitrepo = %d (%s)", rr.Code, rr.Body)
	}
	rr = do(t, h, "GET", "/ui/requests", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "infra") || !strings.Contains(rr.Body.String(), "push a fix") {
		t.Fatalf("requestsPage missing gitrepo request = %d body=%s", rr.Code, rr.Body)
	}

	// Approve it from the dashboard, then the run's grant materializes.
	var reqID string
	_ = s.Store.Tx(t.Context(), func(tx store.Tx) error {
		got, _ := tx.ListGitRepoRequests("Pending")
		if len(got) > 0 {
			reqID = got[0].ID
		}
		return nil
	})
	if rr := postForm(t, h, "/ui/gitrepo-requests/approve", url.Values{"id": {reqID}}); rr.Code != 303 && rr.Code != 200 {
		t.Fatalf("ui approve = %d (%s)", rr.Code, rr.Body)
	}
	if rr := doAuth(t, h, "GET", "/v1/runs/run-1/requested-gitrepo?name=infra", "", tok); rr.Code != 200 {
		t.Fatalf("materialize after ui approve = %d (%s)", rr.Code, rr.Body)
	}

	// Delete from the UI.
	if rr := postForm(t, h, "/ui/gitrepos/delete", url.Values{"namespace": {"claw-agents"}, "name": {"infra"}}); rr.Code != 303 && rr.Code != 200 {
		t.Fatalf("ui delete = %d (%s)", rr.Code, rr.Body)
	}
	// The empty page still contains the URL as a form placeholder, so assert on
	// the "no repos" empty-state row instead.
	if rr := do(t, h, "GET", "/ui/gitrepos", ""); !strings.Contains(rr.Body.String(), "No repos yet.") {
		t.Fatalf("repo still listed after delete: %s", rr.Body)
	}
}

// postForm issues a form-encoded POST (the dashboard handlers expect ParseForm).
func postForm(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeRepo(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if enc := m["credential"]; enc != "" {
		dec, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("credential not base64: %v", err)
		}
		m["credential"] = string(dec)
	}
	return m
}
