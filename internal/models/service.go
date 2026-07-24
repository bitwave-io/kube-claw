// Package models is the LLM registry: UI-managed model configurations
// (provider, endpoint, encrypted API key), an install-wide default, and
// per-session overrides ("use gpt5 for this thread"). The runner asks the
// controller which model a session runs on at every turn, so a switch takes
// effect immediately and API keys never sit in pod env for registry models.
package models

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
)

// keyAD is the AEAD associated data for model API keys — binds the ciphertext
// to this use so a models-table blob can't be replayed elsewhere.
const keyAD = "claw-model-api-key"

// Service owns the registry. Cipher is the same AEAD primitive the secret
// authority uses (Tink keyset on the data volume).
type Service struct {
	Store  store.Store
	Cipher secrets.Cipher
	// Catalog lists a provider's models during a sync. nil = the default HTTP
	// catalog; tests inject a stub.
	Catalog Catalog
}

// catalog returns the configured Catalog, defaulting to the HTTP one.
func (s *Service) catalog() Catalog {
	if s.Catalog != nil {
		return s.Catalog
	}
	return newHTTPCatalog()
}

// Resolved is the provider configuration the runner needs for a session's
// turn. APIKey is plaintext — it travels the same in-cluster, run-token-authed
// channel as secret materialization.
type Resolved struct {
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	ModelID   string `json:"modelId"`
	BaseURL   string `json:"baseUrl,omitempty"`
	APIKey    string `json:"apiKey,omitempty"`
	MaxTokens int    `json:"maxTokens,omitempty"` // per-call output cap; 0 = provider default
	// Source records why this model was picked: "session" (thread override)
	// or "default" (install default).
	Source string `json:"source"`
}

// Upsert validates and stores a model configuration. apiKey semantics: ""
// keeps any existing stored key (create with no key = keyless endpoint).
// makeDefault marks the model as the install default; the very first model
// registered becomes the default regardless (sessions must always resolve
// somewhere) — both inside the same transaction as the upsert, so the
// registry can never hold models with no default.
func (s *Service) Upsert(ctx context.Context, m store.Model, apiKey string, makeDefault bool) error {
	m.Name = strings.TrimSpace(m.Name)
	m.Provider = strings.ToLower(strings.TrimSpace(m.Provider))
	m.ModelID = strings.TrimSpace(m.ModelID)
	m.BaseURL = strings.TrimSpace(m.BaseURL)
	if m.Name == "" || strings.ContainsAny(m.Name, " \t\n") {
		return fmt.Errorf("model name is required (no spaces — it's the handle users type in chat)")
	}
	if !slices.Contains(store.ModelProviders, m.Provider) {
		return fmt.Errorf("provider must be one of %v (openai = any OpenAI-compatible endpoint)", store.ModelProviders)
	}
	if m.ModelID == "" {
		return fmt.Errorf("modelId is required")
	}
	if m.MaxTokens < 0 {
		return fmt.Errorf("maxTokens must be positive (or 0 for the provider default)")
	}
	if apiKey != "" {
		ct, err := s.Cipher.Encrypt([]byte(apiKey), []byte(keyAD))
		if err != nil {
			return fmt.Errorf("encrypt api key: %w", err)
		}
		m.APIKeyCiphertext = ct
	}
	// A hand-entered model is enabled by default. If the row already exists
	// (edit), preserve whatever enabled state it had so an edit doesn't silently
	// re-enable a model the admin disabled.
	m.Enabled = true
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		if existing, err := tx.GetModel(m.Name); err == nil {
			m.Enabled = existing.Enabled
		}
		if err := tx.UpsertModel(m); err != nil {
			return err
		}
		if !makeDefault {
			all, err := tx.ListModels()
			if err != nil {
				return err
			}
			makeDefault = len(all) == 1
		}
		if makeDefault {
			if err := tx.SetDefaultModel(m.Name); err != nil {
				return err
			}
		}
		return tx.AppendAudit(store.AuditEvent{Type: "model.upserted", Actor: "admin",
			Detail: map[string]any{"model": m.Name, "provider": m.Provider, "default": makeDefault}})
	})
}

// Delete removes a model. The default cannot be deleted while other models
// exist (pick a new default first — sessions must always resolve somewhere).
func (s *Service) Delete(ctx context.Context, name string) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		m, err := tx.GetModel(name)
		if err != nil {
			return err
		}
		if m.IsDefault {
			all, err := tx.ListModels()
			if err != nil {
				return err
			}
			if len(all) > 1 {
				return fmt.Errorf("%q is the default model — set another default before deleting it", name)
			}
		}
		if err := tx.DeleteModel(name); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "model.deleted", Actor: "admin",
			Detail: map[string]any{"model": name}})
	})
}

// SetDefault marks the install-wide default model.
func (s *Service) SetDefault(ctx context.Context, name string) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.SetDefaultModel(name); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "model.default", Actor: "admin",
			Detail: map[string]any{"model": name}})
	})
}

// List returns the registry (ciphertext included; callers must not serialize
// it — store.Model hides it from JSON).
func (s *Service) List(ctx context.Context) ([]store.Model, error) {
	var out []store.Model
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		var e error
		out, e = tx.ListModels()
		return e
	})
	return out, err
}

// ListEnabled returns only the models reachable from chat (enabled), default
// first — this is what switch_model offers and what runModel advertises.
func (s *Service) ListEnabled(ctx context.Context) ([]store.Model, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, m := range all {
		if m.Enabled {
			out = append(out, m)
		}
	}
	return out, nil
}

// SetSessionModel pins a session to a registered model ("switch_model" from
// chat). setBy is the requesting run's id for the audit trail. A disabled model
// is not switchable.
func (s *Service) SetSessionModel(ctx context.Context, sessionID, name, setBy string) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		m, err := tx.GetModel(name)
		if err != nil {
			return err
		}
		if !m.Enabled {
			return fmt.Errorf("model %q is disabled", name)
		}
		if err := tx.SetSessionModel(sessionID, name, setBy); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "model.session_switched", Actor: setBy,
			Detail: map[string]any{"session": sessionID, "model": name}})
	})
}

// SetModelEnabled flips a model's enabled flag. Disabling the install default is
// rejected — a session must always resolve somewhere (pick a new default first).
func (s *Service) SetModelEnabled(ctx context.Context, name string, enabled bool) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		m, err := tx.GetModel(name)
		if err != nil {
			return err
		}
		if !enabled && m.IsDefault {
			return fmt.Errorf("%q is the default model — set another default before disabling it", name)
		}
		if err := tx.SetModelEnabled(name, enabled); err != nil {
			return err
		}
		ev := "model.enabled"
		if !enabled {
			ev = "model.disabled"
		}
		return tx.AppendAudit(store.AuditEvent{Type: ev, Actor: "admin", Detail: map[string]any{"model": name}})
	})
}

// --- providers ---

// UpsertProvider validates and stores a provider. apiKey semantics mirror
// Upsert: "" keeps the existing stored key.
func (s *Service) UpsertProvider(ctx context.Context, p store.Provider, apiKey string) error {
	p.Name = strings.TrimSpace(p.Name)
	p.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
	p.BaseURL = strings.TrimSpace(p.BaseURL)
	p.ModelPrefix = strings.TrimSpace(p.ModelPrefix)
	if p.Name == "" || strings.ContainsAny(p.Name, " \t\n") {
		return fmt.Errorf("provider name is required (no spaces)")
	}
	if !slices.Contains(store.ProviderKinds, p.Kind) {
		return fmt.Errorf("kind must be one of %v", store.ProviderKinds)
	}
	if apiKey != "" {
		ct, err := s.Cipher.Encrypt([]byte(apiKey), []byte(keyAD))
		if err != nil {
			return fmt.Errorf("encrypt api key: %w", err)
		}
		p.APIKeyCiphertext = ct
	}
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.UpsertProvider(p); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "provider.upserted", Actor: "admin",
			Detail: map[string]any{"provider": p.Name, "kind": p.Kind}})
	})
}

// DeleteProvider removes a provider and the models its catalog discovered. If
// one of those models is the current default, deletion is refused (re-point the
// default first) — mirrors the model-delete guard.
func (s *Service) DeleteProvider(ctx context.Context, name string) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		all, err := tx.ListModels()
		if err != nil {
			return err
		}
		for _, m := range all {
			if m.ProviderName == name && m.IsDefault {
				return fmt.Errorf("provider %q owns the default model %q — set another default before deleting it", name, m.Name)
			}
		}
		if err := tx.DeleteProvider(name); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "provider.deleted", Actor: "admin",
			Detail: map[string]any{"provider": name}})
	})
}

// ListProviders returns all registered providers.
func (s *Service) ListProviders(ctx context.Context) ([]store.Provider, error) {
	var out []store.Provider
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		var e error
		out, e = tx.ListProviders()
		return e
	})
	return out, err
}

// SyncProvider pulls a provider's catalog and reconciles it into the models
// table: each discovered id is upserted as a model (handle = prefix+sanitized
// id). A model's existing enabled flag is PRESERVED across resyncs, so an admin
// disable sticks. Models that vanished from the catalog are left in place (v1:
// no auto-prune — safer than yanking a model a thread might be pinned to). The
// sync outcome (timestamp / error) is recorded on the provider row.
func (s *Service) SyncProvider(ctx context.Context, name string) error {
	var prov store.Provider
	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		p, err := tx.GetProvider(name)
		prov = p
		return err
	}); err != nil {
		return err
	}
	key := ""
	if len(prov.APIKeyCiphertext) > 0 {
		pt, err := s.Cipher.Decrypt(prov.APIKeyCiphertext, []byte(keyAD))
		if err != nil {
			return fmt.Errorf("decrypt provider key %q: %w", name, err)
		}
		key = string(pt)
	}
	discovered, listErr := s.catalog().List(ctx, store2Provider{Kind: prov.Kind, BaseURL: prov.BaseURL, APIKey: key})
	if listErr != nil {
		// Record the failure and surface it; don't touch existing models.
		_ = s.Store.Tx(ctx, func(tx store.Tx) error {
			return tx.SetProviderSyncState(name, store.NowRFC3339(), listErr.Error())
		})
		return listErr
	}
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		// Does the registry already have a default? If not, the first model this
		// sync adds becomes it — a synced-from-empty registry must resolve
		// somewhere, exactly as the first hand-entered model does in Upsert.
		hasDefault := true
		if _, err := tx.GetDefaultModel(); errors.Is(err, store.ErrNotFound) {
			hasDefault = false
		}
		added, firstHandle := 0, ""
		skipped := []string{}
		for _, d := range discovered {
			handle := sanitizeHandle(prov.ModelPrefix + d.ModelID)
			if handle == "" {
				continue
			}
			enabled := true
			if existing, err := tx.GetModel(handle); err == nil {
				// A handle already owned by a manual model (ProviderName "") or a
				// DIFFERENT provider is not ours to overwrite — clobbering it would
				// silently repoint someone else's model at this provider's endpoint
				// and key. Skip it and record the collision. The admin resolves it
				// by renaming or setting a ModelPrefix on this provider.
				if existing.ProviderName != name {
					skipped = append(skipped, handle)
					continue
				}
				enabled = existing.Enabled // preserve a prior manual disable of our own row
			} else {
				added++
			}
			if firstHandle == "" {
				firstHandle = handle
			}
			if err := tx.UpsertModel(store.Model{
				Name:         handle,
				Provider:     d.WireFormat,
				ModelID:      d.ModelID,
				BaseURL:      d.BaseURL,
				ProviderName: name,
				Enabled:      enabled,
				// no api_key_cipher: discovered models inherit the provider key
			}); err != nil {
				return err
			}
		}
		if !hasDefault && firstHandle != "" {
			if err := tx.SetDefaultModel(firstHandle); err != nil {
				return err
			}
		}
		if err := tx.SetProviderSyncState(name, store.NowRFC3339(), ""); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "provider.synced", Actor: "sync",
			Detail: map[string]any{"provider": name, "discovered": len(discovered), "new": added, "skippedCollisions": skipped}})
	})
}

// sanitizeHandle makes a chat-safe model handle: no whitespace (spaces →
// hyphens), trimmed. Preserves case and provider id punctuation.
func sanitizeHandle(s string) string {
	s = strings.TrimSpace(s)
	return strings.Join(strings.Fields(s), "-")
}

// Resolve picks the model a session runs on: the session's override, else the
// install default, else ErrNotFound (caller falls back to env/legacy config).
// The API key is decrypted here and nowhere else.
func (s *Service) Resolve(ctx context.Context, sessionID string) (Resolved, error) {
	var m store.Model
	var prov store.Provider
	var haveProv bool
	source := "default"
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		// A session override only resolves if the pinned model is still enabled;
		// otherwise fall through to the default (disabling a model must retire it
		// everywhere, not strand a thread on a dead endpoint).
		if sessionID != "" {
			if name, err := tx.GetSessionModel(sessionID); err == nil {
				if got, err := tx.GetModel(name); err == nil && got.Enabled {
					m, source = got, "session"
				}
			}
		}
		if source != "session" {
			got, err := tx.GetDefaultModel()
			if err != nil {
				return err
			}
			m = got
		}
		// Discovered models carry no key of their own — load the provider row so
		// its key and base URL can be inherited below.
		if m.ProviderName != "" {
			if p, err := tx.GetProvider(m.ProviderName); err == nil {
				prov, haveProv = p, true
			}
		}
		return nil
	})
	if err != nil {
		return Resolved{}, err
	}
	out := Resolved{Name: m.Name, Provider: m.Provider, ModelID: m.ModelID, BaseURL: m.BaseURL, MaxTokens: m.MaxTokens, Source: source}
	// A model's own key/base URL win; provider-discovered models inherit theirs
	// from the provider row.
	keyCipher := m.APIKeyCiphertext
	if haveProv {
		if out.BaseURL == "" {
			out.BaseURL = prov.BaseURL
		}
		if len(keyCipher) == 0 {
			keyCipher = prov.APIKeyCiphertext
		}
	}
	if len(keyCipher) > 0 {
		pt, err := s.Cipher.Decrypt(keyCipher, []byte(keyAD))
		if err != nil {
			return Resolved{}, fmt.Errorf("decrypt api key for model %q: %w", m.Name, err)
		}
		out.APIKey = string(pt)
	}
	return out, nil
}
