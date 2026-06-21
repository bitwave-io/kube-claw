package apihttp

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
)

// UIServer serves ONLY the one-time secret-intake page (DESIGN.md §8.3). It runs
// on a SEPARATE listener from the internal API so an Ingress misconfig can never
// reach /v1/* — this mux has no other routes registered.
type UIServer struct {
	Addr    string
	Secrets *secrets.Service
}

func (s *UIServer) NeedLeaderElection() bool { return false }

func (s *UIServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/secret-intake/{token}", s.form)
	mux.HandleFunc("POST /ui/secret-intake/{token}", s.submit)
	return mux
}

func (s *UIServer) Start(ctx context.Context) error {
	srv := &http.Server{Addr: s.Addr, Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	logf.Log.WithName("ui").Info("serving secret-intake UI", "addr", s.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

var intakeForm = template.Must(template.New("intake").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>kube-claw secret intake</title></head>
<body style="font-family:system-ui;max-width:40rem;margin:3rem auto">
<h2>Submit secret value</h2>
<p>This is a one-time link. The value is encrypted and never echoed back.</p>
<form method="POST" action="/ui/secret-intake/{{.Token}}">
  <textarea name="value" rows="10" style="width:100%" placeholder="paste secret value"></textarea>
  <p><button type="submit">Submit</button></p>
</form>
</body></html>`))

func (s *UIServer) form(w http.ResponseWriter, r *http.Request) {
	// Do not validate the token on GET (avoid consuming it / leaking validity to
	// URL-preview bots). Just render the form.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = intakeForm.Execute(w, map[string]string{"Token": r.PathValue("token")})
}

func (s *UIServer) submit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	value := r.PostFormValue("value")
	if value == "" {
		http.Error(w, "value is required", http.StatusBadRequest)
		return
	}

	err := s.Secrets.SubmitIntake(r.Context(), token, []byte(value))
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><body style="font-family:system-ui;max-width:40rem;margin:3rem auto">`+
			`<h2>Stored.</h2><p>The secret was encrypted and saved. This link is now spent.</p></body>`)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrTokenUsed), errors.Is(err, store.ErrTokenExpired):
		// Generic 404 for all three — no oracle distinguishing unknown/used/expired.
		http.Error(w, "invalid or expired link", http.StatusNotFound)
	default:
		logf.Log.WithName("ui").Error(err, "intake submit failed") // never logs the value
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
