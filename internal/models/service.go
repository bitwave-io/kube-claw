// Package models is the LLM registry: UI-managed model configurations
// (provider, endpoint, encrypted API key), an install-wide default, and
// per-session overrides ("use gpt5 for this thread"). The runner asks the
// controller which model a session runs on at every turn, so a switch takes
// effect immediately and API keys never sit in pod env for registry models.
package models

import (
	"context"
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
}

// Resolved is the provider configuration the runner needs for a session's
// turn. APIKey is plaintext — it travels the same in-cluster, run-token-authed
// channel as secret materialization.
type Resolved struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
	BaseURL  string `json:"baseUrl,omitempty"`
	APIKey   string `json:"apiKey,omitempty"`
	// Source records why this model was picked: "session" (thread override)
	// or "default" (install default).
	Source string `json:"source"`
}

// Upsert validates and stores a model configuration. apiKey semantics: ""
// keeps any existing stored key (create with no key = keyless endpoint).
func (s *Service) Upsert(ctx context.Context, m store.Model, apiKey string) error {
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
	if apiKey != "" {
		ct, err := s.Cipher.Encrypt([]byte(apiKey), []byte(keyAD))
		if err != nil {
			return fmt.Errorf("encrypt api key: %w", err)
		}
		m.APIKeyCiphertext = ct
	}
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.UpsertModel(m); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "model.upserted", Actor: "admin",
			Detail: map[string]any{"model": m.Name, "provider": m.Provider}})
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

// SetSessionModel pins a session to a registered model ("switch_model" from
// chat). setBy is the requesting run's id for the audit trail.
func (s *Service) SetSessionModel(ctx context.Context, sessionID, name, setBy string) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		if _, err := tx.GetModel(name); err != nil {
			return err
		}
		if err := tx.SetSessionModel(sessionID, name, setBy); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "model.session_switched", Actor: setBy,
			Detail: map[string]any{"session": sessionID, "model": name}})
	})
}

// Resolve picks the model a session runs on: the session's override, else the
// install default, else ErrNotFound (caller falls back to env/legacy config).
// The API key is decrypted here and nowhere else.
func (s *Service) Resolve(ctx context.Context, sessionID string) (Resolved, error) {
	var m store.Model
	source := "default"
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		if sessionID != "" {
			if name, err := tx.GetSessionModel(sessionID); err == nil {
				if got, err := tx.GetModel(name); err == nil {
					m, source = got, "session"
					return nil
				}
			}
		}
		got, err := tx.GetDefaultModel()
		if err != nil {
			return err
		}
		m = got
		return nil
	})
	if err != nil {
		return Resolved{}, err
	}
	out := Resolved{Name: m.Name, Provider: m.Provider, ModelID: m.ModelID, BaseURL: m.BaseURL, Source: source}
	if len(m.APIKeyCiphertext) > 0 {
		pt, err := s.Cipher.Decrypt(m.APIKeyCiphertext, []byte(keyAD))
		if err != nil {
			return Resolved{}, fmt.Errorf("decrypt api key for model %q: %w", m.Name, err)
		}
		out.APIKey = string(pt)
	}
	return out, nil
}
