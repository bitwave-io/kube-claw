package apihttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/version"
)

// getVersion reports the stamped release version (DESIGN.md §24, Phase 8a AC).
// Unauthenticated like /healthz — it's the upgrade-comparable identity.
func (s *Server) getVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version.Get()})
}

// Install-wide settings endpoints (DESIGN.md §24.6). The store is a free-form
// KV, but the API only exposes an allowlist of operator-settable keys — this
// surface is for install configuration, not arbitrary storage. Management
// shares the admin basic-auth credential (like connectors).

// settingKeys maps API/CLI-friendly names to store keys. Only these are
// readable/writable over HTTP.
var settingKeys = map[string]string{
	"upgrade-admin":            store.SettingUpgradeAdmin,
	"upgrade-skipped-version":  store.SettingSkippedVersion,
	"upgrade-notified-version": store.SettingNotifiedVersion,
}

// listSettings returns the allowlisted settings and their values ("" = unset).
func (s *Server) listSettings(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	out := map[string]string{}
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		for name, key := range settingKeys {
			v, err := tx.GetSetting(key)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			out[name] = v
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// setSetting sets one allowlisted setting: PUT /v1/settings/{key} {"value": "…"}.
// An empty value clears it back to unset semantics (stored empty).
func (s *Server) setSetting(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	key, ok := settingKeys[r.PathValue("key")]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown setting (known: upgrade-admin, upgrade-skipped-version, upgrade-notified-version)")
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Value = strings.TrimSpace(req.Value)
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		return tx.SetSetting(key, req.Value)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{r.PathValue("key"): req.Value})
}
