package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// TestOARequestTranslation: an Anthropic-typed history (system, user text,
// assistant text+tool_use, user tool_result) translates to the OpenAI
// chat.completions shape with order and ids preserved.
func TestOARequestTranslation(t *testing.T) {
	params := anthropic.MessageNewParams{
		MaxTokens: 1000,
		System:    []anthropic.TextBlockParam{{Text: "be helpful"}},
		Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
			Name:        "bash",
			Description: anthropic.String("run a command"),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{"command": map[string]any{"type": "string"}},
				Required:   []string{"command"},
			},
		}}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("list the pods")),
			anthropic.NewAssistantMessage(
				anthropic.NewTextBlock("checking"),
				anthropic.NewToolUseBlock("call_1", map[string]any{"command": "kubectl get pods"}, "bash"),
			),
			anthropic.NewUserMessage(anthropic.NewToolResultBlock("call_1", "pod-a Running", false)),
		},
	}
	body, err := oaRequest(params, "gpt-5.2", "max_completion_tokens", 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]oaMessage)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (system, user, assistant, tool): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "system" || msgs[0].Content != "be helpful" {
		t.Fatalf("system = %+v", msgs[0])
	}
	if msgs[1].Role != "user" || msgs[1].Content != "list the pods" {
		t.Fatalf("user = %+v", msgs[1])
	}
	a := msgs[2]
	if a.Role != "assistant" || a.Content != "checking" || len(a.ToolCalls) != 1 ||
		a.ToolCalls[0].ID != "call_1" || a.ToolCalls[0].Function.Name != "bash" ||
		!strings.Contains(a.ToolCalls[0].Function.Arguments, "kubectl get pods") {
		t.Fatalf("assistant = %+v", a)
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "call_1" || msgs[3].Content != "pod-a Running" {
		t.Fatalf("tool result = %+v", msgs[3])
	}
	if _, ok := body["max_completion_tokens"]; !ok {
		t.Fatal("official endpoint must send max_completion_tokens")
	}
	if body2, _ := oaRequest(params, "llama", "max_tokens", 1000, false); body2["max_tokens"] == nil {
		t.Fatal("self-hosted endpoint must send max_tokens")
	}
	// No registry cap → no cap parameter at all: the endpoint's own model
	// limit applies (sending a 32k default 400s on smaller engines).
	if body3, _ := oaRequest(params, "llama", "max_tokens", 0, false); body3["max_tokens"] != nil {
		t.Fatal("cap must be omitted when maxTokens is 0")
	}
	tools := body["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["function"].(map[string]any)["name"] != "bash" {
		t.Fatalf("tools = %+v", tools)
	}
}

// TestOpenAIStream: an SSE stream with text, a chunked tool call, and usage
// synthesizes an anthropic.Message the loop can consume — content blocks
// answer AsAny(), the raw tool input round-trips, usage and model land.
func TestOpenAIStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"model":"gpt-5.2","choices":[{"delta":{"content":"On it. "}}]}`,
		`data: {"choices":[{"delta":{"content":"Checking now."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"bash","arguments":"{\"comm"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"uptime\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":45}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n"

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "gpt-5.2" || req["stream"] != true {
			t.Errorf("request = %v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	s := &agentSession{modelProvider: "openai", modelID: "gpt-5.2", modelBaseURL: srv.URL, modelAPIKey: "sk-test"}
	msg, err := s.openaiStream(context.Background(), anthropic.MessageNewParams{MaxTokens: 100,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if string(msg.Model) != "gpt-5.2" || msg.StopReason != "tool_use" {
		t.Fatalf("model=%s stop=%s", msg.Model, msg.StopReason)
	}
	if msg.Usage.InputTokens != 120 || msg.Usage.OutputTokens != 45 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
	var text, toolName, toolID, toolArgs string
	for _, b := range msg.Content {
		switch v := b.AsAny().(type) {
		case anthropic.TextBlock:
			text = v.Text
		case anthropic.ToolUseBlock:
			toolName, toolID, toolArgs = v.Name, b.ID, v.JSON.Input.Raw()
		}
	}
	if text != "On it. Checking now." {
		t.Fatalf("text = %q", text)
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(toolArgs), &in); err != nil || toolName != "bash" || toolID != "call_9" || in.Command != "uptime" {
		t.Fatalf("tool call = name %q id %q args %q (%v)", toolName, toolID, toolArgs, err)
	}
	// History append must produce params the NEXT translation can read back.
	param := msg.ToParam()
	round, err := oaRequest(anthropic.MessageNewParams{MaxTokens: 10,
		Messages: []anthropic.MessageParam{param}}, "gpt-5.2", "max_tokens", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	rmsgs := round["messages"].([]oaMessage)
	if len(rmsgs) != 1 || len(rmsgs[0].ToolCalls) != 1 || rmsgs[0].ToolCalls[0].ID != "call_9" {
		t.Fatalf("round-trip = %+v", rmsgs)
	}

	// Permanent HTTP errors surface with their status for retry classification.
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv401.Close()
	s2 := &agentSession{modelProvider: "openai", modelID: "m", modelBaseURL: srv401.URL}
	_, err = s2.openaiStream(context.Background(), anthropic.MessageNewParams{MaxTokens: 10})
	var httpErr *httpStatusError
	if err == nil || !errorsAs(err, &httpErr) || httpErr.code != 401 {
		t.Fatalf("401 must surface as httpStatusError, got %v", err)
	}
}

// sseServer serves a fixed SSE body for one-shot stream tests.
func sseServer(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()
	body := strings.Join(lines, "\n\n") + "\n"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
}

// TestOpenAIStreamLengthCutoff: finish_reason "length" must surface as
// stop_reason max_tokens (so the loop's continuation branch fires), and a tool
// call cut off mid-arguments must be dropped, not executed with empty input.
func TestOpenAIStreamLengthCutoff(t *testing.T) {
	srv := sseServer(t,
		`data: {"choices":[{"delta":{"content":"Half an ans"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"upt"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`)
	defer srv.Close()
	s := &agentSession{modelProvider: "openai", modelID: "m", modelBaseURL: srv.URL}
	msg, err := s.openaiStream(context.Background(), anthropic.MessageNewParams{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.StopReason != "max_tokens" {
		t.Fatalf("stop = %s, want max_tokens", msg.StopReason)
	}
	for _, b := range msg.Content {
		if _, ok := b.AsAny().(anthropic.ToolUseBlock); ok {
			t.Fatal("truncated tool call must be dropped, not synthesized with partial input")
		}
	}
}

// TestOpenAIStreamEmptyAndErrors: an empty completion and an in-stream error
// event must both fail the call (retryable) instead of synthesizing an empty
// assistant message that poisons the session history.
func TestOpenAIStreamEmptyAndErrors(t *testing.T) {
	empty := sseServer(t,
		`data: {"choices":[{"delta":{"content":"  "}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)
	defer empty.Close()
	s := &agentSession{modelProvider: "openai", modelID: "m", modelBaseURL: empty.URL}
	if _, err := s.openaiStream(context.Background(), anthropic.MessageNewParams{}); err == nil ||
		!strings.Contains(err.Error(), "empty completion") {
		t.Fatalf("whitespace-only stream must error, got %v", err)
	}

	errEvt := sseServer(t,
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
		`data: {"error":{"message":"rate limit reached mid-stream","type":"rate_limit"}}`,
		`data: [DONE]`)
	defer errEvt.Close()
	s2 := &agentSession{modelProvider: "openai", modelID: "m", modelBaseURL: errEvt.URL}
	if _, err := s2.openaiStream(context.Background(), anthropic.MessageNewParams{}); err == nil ||
		!strings.Contains(err.Error(), "rate limit reached mid-stream") {
		t.Fatalf("in-stream error event must surface, got %v", err)
	}
}

// TestOpenAIStreamIndexlessToolCalls: complete tool calls delivered without
// index fields (Ollama-style) must stay distinct calls, not merge into one
// with concatenated argument JSON.
func TestOpenAIStreamIndexlessToolCalls(t *testing.T) {
	srv := sseServer(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"bash","arguments":"{\"command\":\"date\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"bash","arguments":"{\"command\":\"uptime\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	defer srv.Close()
	s := &agentSession{modelProvider: "openai", modelID: "m", modelBaseURL: srv.URL}
	msg, err := s.openaiStream(context.Background(), anthropic.MessageNewParams{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range msg.Content {
		if v, ok := b.AsAny().(anthropic.ToolUseBlock); ok {
			var in struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(v.JSON.Input.Raw()), &in); err != nil {
				t.Fatalf("tool input must stay parseable: %v", err)
			}
			got = append(got, b.ID+":"+in.Command)
		}
	}
	if len(got) != 2 || got[0] != "call_a:date" || got[1] != "call_b:uptime" {
		t.Fatalf("index-less tool calls merged or mangled: %v", got)
	}
}

// TestOpenAIStreamTokenParamFallback: an endpoint that rejects the guessed
// output-cap parameter by name gets one retry with the other name, and the
// working choice sticks on the session.
func TestOpenAIStreamTokenParamFallback(t *testing.T) {
	var params []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["max_tokens"]; ok {
			params = append(params, "max_tokens")
			http.Error(w, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`, http.StatusBadRequest)
			return
		}
		params = append(params, "max_completion_tokens")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n"))
	}))
	defer srv.Close()
	// Non-official base URL guesses max_tokens first; the 400 flips it.
	s := &agentSession{modelProvider: "openai", modelID: "m", modelBaseURL: srv.URL, modelMaxTokens: 4096}
	msg, err := s.openaiStream(context.Background(), anthropic.MessageNewParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) == 0 {
		t.Fatal("flipped retry must return the completion")
	}
	if len(params) != 2 || params[0] != "max_tokens" || params[1] != "max_completion_tokens" {
		t.Fatalf("param sequence = %v, want [max_tokens max_completion_tokens]", params)
	}
	if s.oaTokenParam != "max_completion_tokens" {
		t.Fatalf("working param must stick, got %q", s.oaTokenParam)
	}
	// Next call uses the stuck param directly — no repeated probing.
	if _, err := s.openaiStream(context.Background(), anthropic.MessageNewParams{}); err != nil {
		t.Fatal(err)
	}
	if len(params) != 3 || params[2] != "max_completion_tokens" {
		t.Fatalf("param sequence after stick = %v", params)
	}
}

// errorsAs avoids importing errors just for the test.
func errorsAs(err error, target **httpStatusError) bool {
	for err != nil {
		if e, ok := err.(*httpStatusError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
