package main

// OpenAI-compatible provider adapter. The agent loop is written against
// Anthropic message types end-to-end (history, tool dispatch, accumulation);
// supporting OpenAI — and every OpenAI-compatible endpoint: vLLM, Ollama,
// OpenRouter, LM Studio — is a WIRE-FORMAT translation confined to this file,
// not a second loop. Requests are translated params→chat.completions, the SSE
// stream is accumulated, and the result is synthesized back into a canonical
// anthropic.Message (via its own unmarshaler) so the loop cannot tell
// providers apart. Plain net/http + SSE — no SDK dependency.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

const defaultOpenAIBaseURL = "https://api.openai.com/v1"

// httpStatusError carries the HTTP status of a failed provider call so the
// retry loop can distinguish permanent (4xx) from transient failures, exactly
// as it does for *anthropic.Error.
type httpStatusError struct {
	code int
	body string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("provider returned %d: %s", e.code, firstLine([]byte(e.body)))
}

// --- request translation -----------------------------------------------------

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaRequest translates MessageNewParams into an OpenAI chat.completions body.
// Anthropic-only concepts (adaptive thinking, cache control) are dropped.
// tokenParam names the output-cap parameter (max_tokens vs
// max_completion_tokens — see openaiStream); maxTokens 0 omits the cap so the
// endpoint applies its own model limit (params.MaxTokens is the Anthropic
// default, far above what many self-hosted models accept).
func oaRequest(params anthropic.MessageNewParams, modelID, tokenParam string, maxTokens int64, includeUsage bool) (map[string]any, error) {
	msgs := []oaMessage{}
	for _, sys := range params.System {
		if strings.TrimSpace(sys.Text) != "" {
			msgs = append(msgs, oaMessage{Role: "system", Content: sys.Text})
		}
	}
	for _, m := range params.Messages {
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		var wire struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			return nil, err
		}
		switch wire.Role {
		case "user":
			// User turns interleave text and tool_result blocks. OpenAI wants
			// each tool result as its own role:"tool" message (keyed by call
			// id) and the text as a user message — emit in block order.
			var texts []string
			for _, b := range wire.Content {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "tool_result":
					msgs = append(msgs, oaMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: contentText(b.Content)})
				}
			}
			if len(texts) > 0 {
				msgs = append(msgs, oaMessage{Role: "user", Content: strings.Join(texts, "\n\n")})
			}
		case "assistant":
			out := oaMessage{Role: "assistant"}
			var texts []string
			for _, b := range wire.Content {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "tool_use":
					tc := oaToolCall{ID: b.ID, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = string(b.Input)
					out.ToolCalls = append(out.ToolCalls, tc)
				}
				// thinking blocks are anthropic-internal: dropped.
			}
			out.Content = strings.Join(texts, "\n\n")
			if out.Content != "" || len(out.ToolCalls) > 0 {
				msgs = append(msgs, out)
			}
		}
	}

	tools := []map[string]any{}
	for _, t := range params.Tools {
		if t.OfTool == nil {
			continue
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.OfTool.Name,
				"description": t.OfTool.Description.Value,
				"parameters": map[string]any{
					"type":       "object",
					"properties": t.OfTool.InputSchema.Properties,
					"required":   t.OfTool.InputSchema.Required,
				},
			},
		})
	}

	body := map[string]any{
		"model":    modelID,
		"messages": msgs,
		"stream":   true,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if maxTokens > 0 {
		body[tokenParam] = maxTokens
	}
	if includeUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return body, nil
}

// contentText renders a tool_result content field (string OR block list) to text.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

// --- streaming + response synthesis -----------------------------------------

// openaiStream makes one chat.completions call over SSE and synthesizes the
// accumulated result into an anthropic.Message via its canonical wire JSON —
// downstream (history, tool dispatch, usage footer) is provider-blind.
//
// The output-cap parameter is named max_completion_tokens on api.openai.com
// and max_tokens on most self-hosted engines — but gateways and proxies front
// both, so the base URL only seeds a guess: a 400 that names the parameter
// flips it once and the working choice sticks for the session.
func (s *agentSession) openaiStream(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	base := strings.TrimRight(s.modelBaseURL, "/")
	official := base == "" || base == defaultOpenAIBaseURL
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	tokenParam := s.oaTokenParam
	if tokenParam == "" {
		tokenParam = "max_tokens"
		if official {
			tokenParam = "max_completion_tokens"
		}
	}
	for {
		msg, err := s.openaiStreamOnce(ctx, params, base, tokenParam, official)
		var httpErr *httpStatusError
		if err != nil && s.oaTokenParam == "" && errors.As(err, &httpErr) &&
			httpErr.code == http.StatusBadRequest && strings.Contains(httpErr.body, tokenParam) {
			if tokenParam == "max_tokens" {
				tokenParam = "max_completion_tokens"
			} else {
				tokenParam = "max_tokens"
			}
			s.oaTokenParam = tokenParam // one flip only; a second 400 surfaces
			continue
		}
		if err == nil {
			s.oaTokenParam = tokenParam
		}
		return msg, err
	}
}

func (s *agentSession) openaiStreamOnce(ctx context.Context, params anthropic.MessageNewParams, base, tokenParam string, official bool) (*anthropic.Message, error) {
	// The registry's per-model cap; 0 omits the parameter so the endpoint
	// applies its own limit (sending Anthropic's 32k default gets a permanent
	// 400 from engines whose context is smaller).
	body, err := oaRequest(params, s.modelID, tokenParam, int64(s.modelMaxTokens), official)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.modelAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.modelAPIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &httpStatusError{code: resp.StatusCode, body: string(b)}
	}

	var text strings.Builder
	toolCalls := map[int]*oaToolCall{}
	served := s.modelID
	finish := ""
	nextIdx := 0
	var inTok, outTok int64

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   string       `json:"content"`
					ToolCalls []oaToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue // tolerate non-JSON keepalives from lenient servers
		}
		// Post-200 failures (rate-limit aborts, content filters) arrive as an
		// in-stream error event; swallowing it would synthesize an empty
		// message and lose the provider's actual complaint.
		if chunk.Error != nil {
			return nil, fmt.Errorf("provider stream error: %s", chunk.Error.Message)
		}
		if chunk.Model != "" {
			served = chunk.Model
		}
		if chunk.Usage != nil {
			inTok, outTok = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
			text.WriteString(c.Delta.Content)
			for _, tc := range c.Delta.ToolCalls {
				idx := tc.Index
				if idx >= nextIdx {
					nextIdx = idx + 1
				}
				// Lenient servers deliver complete tool calls with no index
				// (JSON zero-value): a distinct id landing on an occupied slot
				// is a NEW call, not a continuation — merging would overwrite
				// the first call and concatenate their argument JSON.
				if cur, ok := toolCalls[idx]; ok && tc.ID != "" && cur.ID != "" && cur.ID != tc.ID {
					idx = nextIdx
					nextIdx++
				}
				cur, ok := toolCalls[idx]
				if !ok {
					cp := tc
					toolCalls[idx] = &cp
					continue
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Function.Name = tc.Function.Name
				}
				cur.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("stream read: %w", err)
	}

	// Synthesize the canonical Anthropic message JSON and let the SDK's own
	// unmarshaler build the Message (unions, raw JSON access, ToParam all work).
	content := []map[string]any{}
	if t := text.String(); strings.TrimSpace(t) != "" {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	idxs := make([]int, 0, len(toolCalls))
	for i := range toolCalls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	toolUses := 0
	for _, i := range idxs {
		tc := toolCalls[i]
		input := map[string]any{}
		if args := strings.TrimSpace(tc.Function.Arguments); args != "" {
			if json.Unmarshal([]byte(args), &input) != nil || input == nil {
				if finish == "length" {
					// Cut off mid-arguments — dropping the call (instead of
					// executing it with empty input) lets the max_tokens
					// continuation below re-issue it whole.
					continue
				}
				return nil, fmt.Errorf("provider sent unparseable arguments for tool %s: %s", tc.Function.Name, firstLine([]byte(args)))
			}
		}
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", toolUses)
		}
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": tc.Function.Name, "input": input})
		toolUses++
	}
	// An empty assistant message must never enter history: the loop would
	// store it, and every later Anthropic call on this session 400s on the
	// empty content — erroring here lets callModel retry instead.
	if len(content) == 0 {
		return nil, fmt.Errorf("provider returned an empty completion (finish_reason %q)", finish)
	}
	stop := "end_turn"
	switch {
	case finish == "length":
		// Surfacing max_tokens (instead of a final-looking end_turn) lets the
		// loop take its continuation branch rather than posting a truncated
		// reply as the answer.
		stop = "max_tokens"
	case toolUses > 0 || finish == "tool_calls":
		stop = "tool_use"
	}
	wire, err := json.Marshal(map[string]any{
		"id": "msg_oa", "type": "message", "role": "assistant",
		"model": served, "stop_reason": stop, "stop_sequence": nil,
		"content": content,
		"usage": map[string]any{
			"input_tokens": inTok, "output_tokens": outTok,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		},
	})
	if err != nil {
		return nil, err
	}
	var msg anthropic.Message
	if err := json.Unmarshal(wire, &msg); err != nil {
		return nil, fmt.Errorf("synthesize message: %w", err)
	}
	return &msg, nil
}
