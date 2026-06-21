package apihttp

import (
	"encoding/json"
	"html/template"
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

// --- minimal admin UI for registering base images (not secret) ---

var baseImagesTmpl = template.Must(template.New("bi").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>kube-claw base images</title></head>
<body style="font-family:system-ui;max-width:48rem;margin:2rem auto">
<h2>Base images</h2>
<table border="1" cellpadding="6" style="border-collapse:collapse;width:100%">
<tr><th align="left">Name</th><th align="left">Image</th><th align="left">When to use</th></tr>
{{range .}}<tr><td>{{.Name}}</td><td><code>{{.Image}}</code></td><td>{{.Description}}</td></tr>{{end}}
</table>
<h3>Register a base image</h3>
<form method="POST" action="/ui/base-images">
  <p>Name <input name="name" required></p>
  <p>Image <input name="image" size="60" required placeholder="ghcr.io/org/img@sha256:..."></p>
  <p>When to use <input name="description" size="60" placeholder="has gcloud+bq, for GCP cost agents"></p>
  <button type="submit">Register</button>
</form>
</body></html>`))

func (s *Server) baseImagesPage(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.allBaseImages(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = baseImagesTmpl.Execute(w, imgs)
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
