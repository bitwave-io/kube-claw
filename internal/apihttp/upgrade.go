package apihttp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// UpgradeAPI is the upgrade coordinator surface the API exposes (satisfied by
// *upgrade.Coordinator; nil when self-update is off). The CLI break-glass
// mirrors `claw secret approve` — usable when Slack (or the admin) is
// unavailable (DESIGN.md §24.6).
type UpgradeAPI interface {
	Approve(ctx context.Context, version, byUser string) error
	Status(ctx context.Context) (map[string]any, error)
	// CheckNow requests an immediate release check from the supervisor.
	CheckNow(ctx context.Context) error
}

// upgradeStatus reports the self-update state (GET /v1/upgrade/status).
func (s *Server) upgradeStatus(w http.ResponseWriter, r *http.Request) {
	if s.Upgrades == nil {
		writeErr(w, http.StatusNotFound, "self-update isn't configured on this controller")
		return
	}
	out, err := s.Upgrades.Status(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// upgradeCheck requests an immediate release check (POST /v1/upgrade/check,
// admin-gated like the other upgrade writes). The check is asynchronous — the
// supervisor polls within seconds; results land on the ControlPlane status and
// flow to Slack as usual.
func (s *Server) upgradeCheck(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	if s.Upgrades == nil {
		writeErr(w, http.StatusNotFound, "self-update isn't configured on this controller")
		return
	}
	if err := s.Upgrades.CheckNow(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "check requested"})
}

// upgradeApprove is the break-glass approval (POST /v1/upgrade/approve,
// admin-gated): approves the currently offered release for self-application.
func (s *Server) upgradeApprove(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	if s.Upgrades == nil {
		writeErr(w, http.StatusNotFound, "self-update isn't configured on this controller")
		return
	}
	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Version) == "" {
		writeErr(w, http.StatusBadRequest, `body must be {"version": "vX.Y.Z"}`)
		return
	}
	if err := s.Upgrades.Approve(r.Context(), strings.TrimSpace(req.Version), "cli"); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"approved": req.Version})
}
