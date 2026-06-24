package apihttp

import (
	"encoding/json"
	"net/http"

	"github.com/traego/kube-claw/internal/store"
)

type createBaseImageReq struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	Description string `json:"description"`
}

func (s *Server) createBaseImage(w http.ResponseWriter, r *http.Request) {
	var req createBaseImageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Image == "" {
		writeErr(w, http.StatusBadRequest, "name and image are required")
		return
	}
	if err := s.saveBaseImage(r, req); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "status": "registered"})
}

func (s *Server) listBaseImages(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.allBaseImages(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, imgs)
}

func (s *Server) saveBaseImage(r *http.Request, req createBaseImageReq) error {
	return s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if err := tx.CreateBaseImage(store.BaseImage{Name: req.Name, Image: req.Image, Description: req.Description}); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "baseimage.registered", Actor: "api", Detail: map[string]any{"name": req.Name, "image": req.Image}})
	})
}

func (s *Server) allBaseImages(r *http.Request) ([]store.BaseImage, error) {
	var out []store.BaseImage
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListBaseImages()
		out = got
		return e
	})
	if out == nil {
		out = []store.BaseImage{}
	}
	return out, err
}

// --- admin UI for registering base images (rendered in the dashboard chrome) ---

func (s *Server) baseImagesPage(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.allBaseImages(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	body := `<p class=mut>Base images are the containers agents run in. Register a generic shell base for general agents and specialized images (a cloud SDK, etc.) for specialized ones; an agent references one by name. The "when to use" text helps the router pick the right agent.</p>
<table><tr><th>Name</th><th>Image</th><th>When to use</th></tr>
{{range .D}}<tr>
<td><code>{{.Name}}</code></td><td><code>{{.Image}}</code></td><td class=mut>{{.Description}}</td>
</tr>{{else}}<tr><td colspan=3 class=mut>No base images yet.</td></tr>{{end}}</table>

<h2>Register a base image</h2>
<form method=post action=/ui/base-images style="background:#fff;border:1px solid var(--line);border-radius:8px;padding:1rem;max-width:660px">
<label>Name</label><br><input name=name required placeholder="e.g. default, gcloud, aws" style="width:100%"><br><br>
<label>Image</label><br><input name=image required placeholder="REGION-docker.pkg.dev/PROJECT/REPO/claw-runner-bash:TAG" style="width:100%"><br><br>
<label>When to use</label><br><input name=description style="width:100%" placeholder="generic shell base (bash, curl); or 'Google Cloud SDK — GCP/billing queries'"><br><br>
<button>Register</button>
</form>`
	s.renderDash(w, "images", "Base images", body, imgs)
}

func (s *Server) baseImagesSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad form")
		return
	}
	req := createBaseImageReq{Name: r.PostFormValue("name"), Image: r.PostFormValue("image"), Description: r.PostFormValue("description")}
	if req.Name == "" || req.Image == "" {
		writeErr(w, http.StatusBadRequest, "name and image are required")
		return
	}
	if err := s.saveBaseImage(r, req); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/base-images", http.StatusSeeOther)
}
