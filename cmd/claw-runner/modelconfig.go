package main

// Runner side of the model registry: ask the controller which model this
// session runs on (install default or a thread switch), rebuild the provider
// client when it changes, and the switch_model tool implementation.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type modelChoice struct {
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	ModelID   string `json:"modelId"`
	Notes     string `json:"notes"`
	IsDefault bool   `json:"isDefault"`
}

// refreshModelConfig resolves the session's model from the controller's
// registry. 404 (no registry) or any error keeps the current config — a fresh
// install without registered models runs on the legacy env client, and a
// transient controller blip must never mid-flight-break a working session.
func (s *agentSession) refreshModelConfig(ctx context.Context) {
	if s.controllerURL == "" || s.runID == "" {
		return
	}
	resp, err := authedDo(ctx, http.MethodGet, s.controllerURL+"/v1/runs/"+s.runID+"/model", nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var out struct {
		Model struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			ModelID  string `json:"modelId"`
			BaseURL  string `json:"baseUrl"`
			APIKey   string `json:"apiKey"`
		} `json:"model"`
		Available []modelChoice `json:"available"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return
	}

	var list strings.Builder
	for _, c := range out.Available {
		def := ""
		if c.IsDefault {
			def = " [default]"
		}
		notes := ""
		if c.Notes != "" {
			notes = " — " + c.Notes
		}
		fmt.Fprintf(&list, "- %s (%s: %s)%s%s\n", c.Name, c.Provider, c.ModelID, def, notes)
	}
	s.modelAvail = strings.TrimRight(list.String(), "\n")

	m := out.Model
	if m.Name == s.modelName && m.Provider == s.modelProvider && m.ModelID == s.modelID &&
		m.BaseURL == s.modelBaseURL && m.APIKey == s.modelAPIKey {
		return // unchanged
	}
	s.modelName, s.modelProvider, s.modelID, s.modelBaseURL, s.modelAPIKey =
		m.Name, m.Provider, m.ModelID, m.BaseURL, m.APIKey
	if m.Provider == "anthropic" {
		opts := []option.RequestOption{}
		if m.APIKey != "" {
			opts = append(opts, option.WithAPIKey(m.APIKey))
		}
		if m.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(m.BaseURL))
		}
		s.client = anthropic.NewClient(opts...)
		s.model = anthropic.Model(m.ModelID)
	}
	fmt.Printf("claw-runner: session model → %s (%s: %s)\n", m.Name, m.Provider, m.ModelID)
}

// switchModel implements the switch_model tool: list the registry, or pin
// this session to a registered model (effective immediately — the next model
// call in this very turn runs on it).
func (s *agentSession) switchModel(ctx context.Context, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		if s.modelAvail == "" {
			return "No models are registered — an operator adds them in the admin UI under Models."
		}
		cur := s.modelName
		if cur == "" {
			cur = "(legacy env config)"
		}
		return "Currently on: " + cur + "\nAvailable models:\n" + s.modelAvail
	}
	body, _ := json.Marshal(map[string]string{"model": name})
	resp, err := authedDo(ctx, http.MethodPost, s.controllerURL+"/v1/runs/"+s.runID+"/model", body)
	if err != nil {
		return "couldn't reach the controller to switch: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "No model named \"" + name + "\" is registered. Available:\n" + s.modelAvail
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("switch failed (HTTP %d)", resp.StatusCode)
	}
	s.refreshModelConfig(ctx)
	return fmt.Sprintf("Switched this conversation to %s (%s: %s) — effective immediately, including the rest of this reply.",
		s.modelName, s.modelProvider, s.modelID)
}
