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
	body, err := oaRequest(params, "gpt-5.2", true)
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
	if body2, _ := oaRequest(params, "llama", false); body2["max_tokens"] == nil {
		t.Fatal("self-hosted endpoint must send max_tokens")
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
		Messages: []anthropic.MessageParam{param}}, "gpt-5.2", false)
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
