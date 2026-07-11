package apihttp

import (
	"net/http"
	"strings"

	"github.com/traego/kube-claw/internal/gitrepo"
	"github.com/traego/kube-claw/internal/store"
)

// gitReposPage lists registered repositories and offers a register form + delete,
// mirroring the secrets page. Credentials are write-only: they are entered here
// but never rendered back (they are json:"-" and simply not shown).
func (s *Server) gitReposPage(w http.ResponseWriter, r *http.Request) {
	var repos []store.GitRepo
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListGitRepos()
		repos = got
		return e
	})
	// Decorate each row with which credentials are set (never the values).
	type repoRow struct {
		store.GitRepo
		HasRead  bool
		HasWrite bool
	}
	rows := make([]repoRow, 0, len(repos))
	for _, rp := range repos {
		rows = append(rows, repoRow{GitRepo: rp, HasRead: rp.HasReadCredential(), HasWrite: rp.HasWriteCredential()})
	}
	body := `<p class=mut>Repositories agents can request access to. An agent requests a repo by name at a level (read or write); a granter approves it into a durable grant. Credentials are write-only — entered here, handed only to a granted agent, and never shown again.</p>
<table><tr><th>Name</th><th>Namespace</th><th>URL</th><th>Description</th><th>Creds</th><th>Approvers</th><th>Created</th><th></th></tr>
{{range .D}}<tr>
<td><code>{{.Name}}</code></td><td>{{.Namespace}}</td>
<td class=mut><span class=snip>{{.URL}}</span></td>
<td class=mut>{{.Description}}</td>
<td>{{if .HasRead}}<code>read</code> {{end}}{{if .HasWrite}}<code>write</code>{{end}}{{if not .HasRead}}{{if not .HasWrite}}<span class=mut>—</span>{{end}}{{end}}</td>
<td>{{range .Granters}}<code>{{.}}</code> {{end}}</td>
<td class=mut>{{.CreatedAt}}</td>
<td>
<form method=post action=/ui/gitrepos/delete style=margin:0 onsubmit="return confirm('Delete repo {{.Name}}? This removes its credentials, grants, and requests and cannot be undone.')">
<input type=hidden name=namespace value="{{.Namespace}}"><input type=hidden name=name value="{{.Name}}">
<button style="background:#c5221f;border-color:#c5221f">Delete</button></form>
</td>
</tr>{{else}}<tr><td colspan=8 class=mut>No repos yet.</td></tr>{{end}}</table>

<h2>Register a repo</h2>
<form method=post action=/ui/gitrepos/create style="background:#fff;border:1px solid var(--line);border-radius:8px;padding:1rem;max-width:660px">
<label>Name</label><br><input name=name required placeholder="e.g. infra" style="width:100%"><br><br>
<label>Namespace</label><br><input name=namespace placeholder="claw-agents (default)" style="width:100%"><br><br>
<label>URL</label><br><input name=url required placeholder="https://github.com/acme/infra.git" style="width:100%"><br><br>
<label>Description</label><br><input name=description style="width:100%" placeholder="terraform modules"><br><br>
<label>Read credential</label><br><input name=readCredential type=password autocomplete=off style="width:100%" placeholder="read-only token or deploy key"><br><br>
<label>Write credential</label><br><input name=writeCredential type=password autocomplete=off style="width:100%" placeholder="read-write token or deploy key"><br><br>
<label>Approvers (comma-separated Slack user ids)</label><br><input name=granters style="width:100%" placeholder="U_BOSS, U_LEAD"><br><br>
<p class=mut style="margin:.2rem 0 .8rem">At least one credential is required. A read grant hands back the read credential; a write grant the write credential.</p>
<button>Register</button>
</form>`
	s.renderDash(w, "gitrepos", "Git repos", body, rows)
}

// uiCreateGitRepo registers a repo from the dashboard form.
func (s *Server) uiCreateGitRepo(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad form")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	url := strings.TrimSpace(r.PostFormValue("url"))
	readCred := r.PostFormValue("readCredential")
	writeCred := r.PostFormValue("writeCredential")
	if name == "" || url == "" {
		writeErr(w, http.StatusBadRequest, "name and url are required")
		return
	}
	if !gitrepo.ValidRepoName(name) {
		writeErr(w, http.StatusBadRequest, "invalid repo name")
		return
	}
	if readCred == "" && writeCred == "" {
		writeErr(w, http.StatusBadRequest, "at least one of read or write credential is required")
		return
	}
	ns := strings.TrimSpace(r.PostFormValue("namespace"))
	if ns == "" {
		ns = "claw-agents"
	}
	repo := store.GitRepo{
		ID:              gitrepo.NewGitRepoID(),
		Namespace:       ns,
		Name:            name,
		URL:             url,
		Description:     r.PostFormValue("description"),
		ReadCredential:  readCred,
		WriteCredential: writeCred,
		Granters:        parseGranters(r.PostFormValue("granters")),
	}
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if e := tx.CreateGitRepo(repo); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "gitrepo.created", Actor: "ui",
			Detail: map[string]any{"repo": repo.ID, "name": repo.Name, "namespace": repo.Namespace}})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/gitrepos", http.StatusSeeOther)
}

// uiDeleteGitRepo removes a repo (and its grants/requests) from the dashboard.
func (s *Server) uiDeleteGitRepo(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ns, name := r.FormValue("namespace"), r.FormValue("name")
	if ns == "" || name == "" {
		writeErr(w, http.StatusBadRequest, "namespace and name required")
		return
	}
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if e := tx.DeleteGitRepo(ns, name); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "gitrepo.deleted", Actor: "ui",
			Detail: map[string]any{"namespace": ns, "name": name}})
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/gitrepos", http.StatusSeeOther)
}

// uiApproveGitRepoRequest approves a pending git-repo request (break-glass).
func (s *Server) uiApproveGitRepoRequest(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id := r.FormValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	if _, err := s.GitRepos.Approve(r.Context(), id, "ui", "approved via dashboard"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/requests", http.StatusSeeOther)
}

// uiDenyGitRepoRequest denies a pending git-repo request from the dashboard.
func (s *Server) uiDenyGitRepoRequest(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id := r.FormValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	if err := s.GitRepos.Deny(r.Context(), id, "ui", "denied via dashboard"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/requests", http.StatusSeeOther)
}

// parseGranters splits a comma-separated granter list, trimming blanks.
func parseGranters(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
