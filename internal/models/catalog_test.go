package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCatalogParsing(t *testing.T) {
	ctx := context.Background()

	t.Run("openai", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				t.Fatalf("unexpected path %q", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
				t.Fatalf("missing/incorrect bearer: %q", got)
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.2"},{"id":"gpt-5-mini"},{"id":""}]}`))
		}))
		defer srv.Close()
		got, err := newHTTPCatalog().List(ctx, store2Provider{Kind: "openai", BaseURL: srv.URL, APIKey: "sk-test"})
		if err != nil {
			t.Fatal(err)
		}
		// Empty id is dropped; wire format is openai.
		if len(got) != 2 || got[0].ModelID != "gpt-5.2" || got[0].WireFormat != "openai" {
			t.Fatalf("openai parse = %+v", got)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("x-api-key"); got != "sk-ant" {
				t.Fatalf("missing x-api-key: %q", got)
			}
			if r.Header.Get("anthropic-version") == "" {
				t.Fatal("missing anthropic-version header")
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-8"}]}`))
		}))
		defer srv.Close()
		got, err := newHTTPCatalog().List(ctx, store2Provider{Kind: "anthropic", BaseURL: srv.URL, APIKey: "sk-ant"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ModelID != "claude-opus-4-8" || got[0].WireFormat != "anthropic" {
			t.Fatalf("anthropic parse = %+v", got)
		}
	})

	t.Run("gemini custom gateway routes inference through the gateway", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("key") != "gem-key" {
				t.Fatalf("missing key query: %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro"},{"name":"models/gemini-2.5-flash"}]}`))
		}))
		defer srv.Close()
		// BaseURL points at a custom gateway; the "models/" prefix is stripped and
		// discovered models are registered over the gateway's OpenAI-compat surface
		// (root + /openai) — NOT Google's default, so the gateway isn't bypassed.
		got, err := newHTTPCatalog().List(ctx, store2Provider{Kind: "gemini", BaseURL: srv.URL, APIKey: "gem-key"})
		if err != nil {
			t.Fatal(err)
		}
		wantBase := srv.URL + "/openai"
		if len(got) != 2 || got[0].ModelID != "gemini-2.5-pro" || got[0].WireFormat != "openai" ||
			got[0].BaseURL != wantBase {
			t.Fatalf("gemini parse = %+v (want inference base %q)", got, wantBase)
		}
	})

	t.Run("geminiInferenceBase selects gateway vs default", func(t *testing.T) {
		if got := geminiInferenceBase("", "https://generativelanguage.googleapis.com/v1beta"); got != defaultGeminiOpenAIBase {
			t.Fatalf("no override → %q, want default %q", got, defaultGeminiOpenAIBase)
		}
		if got := geminiInferenceBase("https://gw.internal/gemini", "https://gw.internal/gemini"); got != "https://gw.internal/gemini/openai" {
			t.Fatalf("override → %q, want gateway openai surface", got)
		}
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
		}))
		defer srv.Close()
		if _, err := newHTTPCatalog().List(ctx, store2Provider{Kind: "openai", BaseURL: srv.URL, APIKey: "x"}); err == nil {
			t.Fatal("expected error on 401")
		}
	})
}
