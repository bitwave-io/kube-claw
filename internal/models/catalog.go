package models

// Provider catalog clients: given a provider (kind + key + optional base URL),
// list the model ids it exposes. Each kind speaks its own list-models dialect;
// the results are normalized to bare model ids and mapped into store.Model rows
// by SyncProvider. Plain net/http, no SDK dependency (mirrors the runner's
// OpenAI adapter). A discovered gemini model is registered to run over Gemini's
// OpenAI-compatibility endpoint, so the runner needs no gemini-specific path.
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Default list+inference endpoints per provider kind.
const (
	defaultAnthropicBase = "https://api.anthropic.com/v1"
	defaultOpenAIBase    = "https://api.openai.com/v1"
	// Gemini's OpenAI-compatible surface — used for BOTH catalog listing and
	// inference, so discovered gemini models run through the existing OpenAI
	// wire-format seam in the runner with zero gemini-specific code.
	defaultGeminiOpenAIBase = "https://generativelanguage.googleapis.com/v1beta/openai"
	anthropicVersion        = "2023-06-01"
)

// DiscoveredModel is one model id a provider's catalog returned, already mapped
// to the runner wire format + inference endpoint it should be registered with.
type DiscoveredModel struct {
	ModelID     string // provider model id, e.g. "gpt-5.2", "claude-opus-4-8"
	WireFormat  string // runner Provider value: "anthropic" | "openai"
	BaseURL     string // inference base URL for the registered model ("" = wire-format default)
	InheritsKey bool   // true = the registered model resolves its key from the provider row
}

// Catalog lists a provider's available models.
type Catalog interface {
	List(ctx context.Context, p store2Provider) ([]DiscoveredModel, error)
}

// store2Provider is the subset of provider config a catalog client needs. It
// carries the DECRYPTED key (SyncProvider decrypts before calling).
type store2Provider struct {
	Kind    string
	BaseURL string
	APIKey  string
}

// httpCatalog is the production Catalog: real HTTP calls per kind.
type httpCatalog struct{ client *http.Client }

func newHTTPCatalog() *httpCatalog {
	return &httpCatalog{client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *httpCatalog) List(ctx context.Context, p store2Provider) ([]DiscoveredModel, error) {
	switch p.Kind {
	case "anthropic":
		return c.listAnthropic(ctx, p)
	case "openai":
		return c.listOpenAI(ctx, p)
	case "gemini":
		return c.listGemini(ctx, p)
	default:
		return nil, fmt.Errorf("unknown provider kind %q", p.Kind)
	}
}

// listOpenAI: GET {base}/models with a Bearer key → data[].id.
func (c *httpCatalog) listOpenAI(ctx context.Context, p store2Provider) ([]DiscoveredModel, error) {
	base := firstNonEmpty(p.BaseURL, defaultOpenAIBase)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	ids, err := c.getDataIDs(req)
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveredModel, 0, len(ids))
	for _, id := range ids {
		// Keep the provider's own base URL so gateways/self-hosted keep working.
		out = append(out, DiscoveredModel{ModelID: id, WireFormat: "openai", BaseURL: p.BaseURL, InheritsKey: true})
	}
	return out, nil
}

// listAnthropic: GET {base}/models with x-api-key + anthropic-version → data[].id.
func (c *httpCatalog) listAnthropic(ctx context.Context, p store2Provider) ([]DiscoveredModel, error) {
	base := firstNonEmpty(p.BaseURL, defaultAnthropicBase)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("x-api-key", p.APIKey)
	}
	req.Header.Set("anthropic-version", anthropicVersion)
	ids, err := c.getDataIDs(req)
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveredModel, 0, len(ids))
	for _, id := range ids {
		out = append(out, DiscoveredModel{ModelID: id, WireFormat: "anthropic", BaseURL: p.BaseURL, InheritsKey: true})
	}
	return out, nil
}

// listGemini: GET {base}/models?key=… → models[].name (strip "models/"). The
// discovered models are registered to run over the OpenAI-compat endpoint.
func (c *httpCatalog) listGemini(ctx context.Context, p store2Provider) ([]DiscoveredModel, error) {
	// Gemini's native list endpoint lives under v1beta, not the OpenAI-compat
	// path; derive it from the OpenAI base (or an explicit override root).
	root := firstNonEmpty(p.BaseURL, "https://generativelanguage.googleapis.com/v1beta")
	root = strings.TrimSuffix(strings.TrimRight(root, "/"), "/openai")
	u := root + "/models"
	if p.APIKey != "" {
		u += "?key=" + p.APIKey
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("gemini catalog: bad json: %w", err)
	}
	inferBase := geminiInferenceBase(p.BaseURL, root)
	out := make([]DiscoveredModel, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if id == "" {
			continue
		}
		out = append(out, DiscoveredModel{ModelID: id, WireFormat: "openai", BaseURL: inferBase, InheritsKey: true})
	}
	return out, nil
}

// geminiInferenceBase picks the OpenAI-compat endpoint discovered gemini models
// are registered to run against. With a custom gateway configured (baseURL != "")
// inference MUST route through that gateway's openai-compat surface (root +
// "/openai"), not Google directly — otherwise a gateway-scoped key leaks to
// Google and the gateway is bypassed. root is the already-normalized listing root
// (baseURL with a trailing "/openai" stripped). No override → Google's default.
func geminiInferenceBase(baseURL, root string) string {
	if baseURL == "" {
		return defaultGeminiOpenAIBase
	}
	return root + "/openai"
}

// getDataIDs runs a request whose body is {"data":[{"id":...}]} (the shape both
// OpenAI and Anthropic list-models endpoints share) and returns the ids.
func (c *httpCatalog) getDataIDs(req *http.Request) ([]string, error) {
	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("catalog: bad json: %w", err)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}

func (c *httpCatalog) do(req *http.Request) ([]byte, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog list returned %d: %s", resp.StatusCode, firstLine(body))
	}
	return body, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
