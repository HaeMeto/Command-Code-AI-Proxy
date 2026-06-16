// Package openai translates OpenAI Chat Completions <-> CommandCode
// native shape so OpenAI SDKs and routers (OpenRouter, LiteLLM,
// langchain.openai) can target this proxy as a drop-in OpenAI base URL.
package openai

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"time"
)

// DefaultProvider is sent as `params.provider` to upstream when the
// caller doesn't specify otherwise.
const DefaultProvider = "command-code"

var stopReasonMap = map[string]string{
	"end_turn":       "stop",
	"stop":           "stop",
	"stop_sequence":  "stop",
	"max_tokens":     "length",
	"length":         "length",
	"tool_use":       "tool_calls",
	"content_filter": "content_filter",
}

// ChatRequest is the subset of the OpenAI Chat Completions request body
// that we read. Unknown fields are ignored.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`

	// Optional CommandCode passthrough fields.
	Memory   string         `json:"memory,omitempty"`
	Provider string         `json:"provider,omitempty"`
	Config   map[string]any `json:"config,omitempty"`
}

// ChatMessage is one message in messages[]. Content can be a string or
// an array of content parts; we accept both via the custom unmarshaler
// in normalizeMessages.
type ChatMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

// ToolCall is an OpenAI tool_call in an assistant message.
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function FuncCall `json:"function"`
}

// FuncCall is the function details inside a tool_call.
type FuncCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// CommandCodeBody is the upstream shape this package emits.
type CommandCodeBody struct {
	Memory string         `json:"memory"`
	Params CommandParams  `json:"params"`
	Config map[string]any `json:"config"`
}

// CommandParams is the inner params of the upstream body.
type CommandParams struct {
	Provider string               `json:"provider"`
	Model    string               `json:"model"`
	Messages []CommandCodeMessage `json:"messages"`
}

// CommandCodeMessage is one message in the upstream request.
//
// CommandCode (Anthropic-style API) requires:
//   - role in {"user", "assistant"}
//   - content as an array of typed parts (NOT a plain string)
//
// We always emit content as an array even when there is just one text
// part, because the upstream rejects strings outright with a 400.
type CommandCodeMessage struct {
	Role    string                   `json:"role"`
	Content []CommandCodeContentPart `json:"content"`
}

// CommandCodeContentPart is a single typed content fragment. CommandCode
// uses Anthropic-style content blocks; we only emit text parts (image
// inputs are flattened to a placeholder text part since CommandCode
// support for vision through this proxy isn't validated yet).
type CommandCodeContentPart struct {
	Type       string  `json:"type"`
	Text       string  `json:"text,omitempty"`
	ID         string  `json:"id,omitempty"`
	Name       string  `json:"name,omitempty"`
	ToolCallID *string `json:"toolCallId,omitempty"`
	ToolUseID  *string `json:"tool_use_id,omitempty"`
	ToolName   *string `json:"toolName,omitempty"`
	Content    any     `json:"content,omitempty"`
	Arguments  any     `json:"arguments,omitempty"`
}

// ToolCallPart is a tool_use block in an assistant message.
type ToolCallPart struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// FlattenContent collapses a string-or-array content value into a single
// string. Image parts are replaced with a placeholder.
func FlattenContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		out := ""
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := m["text"].(string); ok && t != "" {
				out += t
				continue
			}
			if m["type"] == "image_url" {
				out += "[image omitted]"
			}
		}
		return out
	}
	return ""
}

// flattenToParts converts an OpenAI content value (string or array of
// typed parts) into the CommandCode array-of-parts shape.
func flattenToParts(content any) []CommandCodeContentPart {
	switch c := content.(type) {
	case string:
		return []CommandCodeContentPart{{Type: "text", Text: c}}
	case []any:
		parts := make([]CommandCodeContentPart, 0, len(c))
		for _, p := range c {
			switch v := p.(type) {
			case string:
				parts = append(parts, CommandCodeContentPart{Type: "text", Text: v})
			case map[string]any:
				switch v["type"] {
				case "text":
					if t, ok := v["text"].(string); ok {
						parts = append(parts, CommandCodeContentPart{Type: "text", Text: t})
					}
				case "image_url", "image", "input_image":
					parts = append(parts, CommandCodeContentPart{Type: "text", Text: "[image omitted]"})
				}
			}
		}
		if len(parts) == 0 {
			parts = []CommandCodeContentPart{{Type: "text", Text: ""}}
		}
		return parts
	}
	return []CommandCodeContentPart{{Type: "text", Text: ""}}
}

// prependSystemText puts a system instruction text in front of the
// first text part of `parts`, separated by two newlines.
func prependSystemText(sysText string, parts []CommandCodeContentPart) []CommandCodeContentPart {
	if sysText == "" {
		return parts
	}
	if len(parts) == 0 {
		return []CommandCodeContentPart{{Type: "text", Text: sysText}}
	}
	if parts[0].Type == "text" {
		if parts[0].Text == "" {
			parts[0].Text = sysText
		} else {
			parts[0].Text = sysText + "\n\n" + parts[0].Text
		}
		return parts
	}
	return append([]CommandCodeContentPart{{Type: "text", Text: sysText}}, parts...)
}

// normalizeMessages adapts OpenAI messages to the CommandCode shape:
//   - system messages are folded into the next user message (their text
//     is prepended). Multiple consecutive systems are joined with "\n\n".
//     A trailing system (no user after) is attached to the last user
//     message, or otherwise becomes a new user message at the end.
//   - role is restricted to user/assistant. Anything else is
//     rewritten to "user" (CommandCode rejects unknown roles with 400).
//   - tool role messages are converted to assistant messages with
//     tool-result content blocks (CommandCode's expected format).
//   - assistant messages with tool_calls get tool_use content blocks.
//   - content is always emitted as a non-empty array of typed parts.
func normalizeMessages(in []ChatMessage) []CommandCodeMessage {
	out := make([]CommandCodeMessage, 0, len(in))
	var pendingSystem string

	appendSystem := func(s string) {
		if pendingSystem == "" {
			pendingSystem = s
		} else {
			pendingSystem += "\n\n" + s
		}
	}

	for _, m := range in {
		role := m.Role
		if role == "" {
			role = "user"
		}
		if role == "system" || role == "developer" {
			appendSystem(FlattenContent(m.Content))
			continue
		}
		if role == "tool" {
			content := FlattenContent(m.Content)
			tuid := m.ToolCallID
			parts := []CommandCodeContentPart{{
				Type:      "tool_result",
				ToolUseID: &tuid,
				Content:   []CommandCodeContentPart{{Type: "text", Text: content}},
			}}
			out = append(out, CommandCodeMessage{Role: "user", Content: parts})
			continue
		}
		if role != "user" && role != "assistant" {
			role = "user"
		}
		var parts []CommandCodeContentPart
		if role == "assistant" && len(m.ToolCalls) > 0 {
			if m.Content != nil {
				parts = flattenToParts(m.Content)
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, CommandCodeContentPart{
					Type:      "tool_use",
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		} else {
			parts = flattenToParts(m.Content)
		}
		if role == "user" && pendingSystem != "" {
			parts = prependSystemText(pendingSystem, parts)
			pendingSystem = ""
		}
		out = append(out, CommandCodeMessage{Role: role, Content: parts})
	}

	// Trailing system text: attach to last user message, or append a new
	// user message so it isn't silently dropped.
	if pendingSystem != "" {
		attached := false
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].Role == "user" {
				out[i].Content = prependSystemText(pendingSystem, out[i].Content)
				attached = true
				break
			}
		}
		if !attached {
			out = append(out, CommandCodeMessage{
				Role:    "user",
				Content: []CommandCodeContentPart{{Type: "text", Text: pendingSystem}},
			})
		}
	}

	return out
}

// BuildDefaultConfig is used when the OpenAI request didn't pass a
// `config` field. The values mirror what the CommandCode CLI sends.
func BuildDefaultConfig() map[string]any {
	return map[string]any{
		"workingDir":    "",
		"date":          time.Now().UTC().Format("2006-01-02"),
		"environment":   fmt.Sprintf("%s-%s, Go %s", runtime.GOOS, runtime.GOARCH, runtime.Version()),
		"structure":     []string{},
		"isGitRepo":     false,
		"currentBranch": "",
		"mainBranch":    "main",
		"gitStatus":     "",
		"recentCommits": []string{},
	}
}

// ToCommandCode validates the OpenAI request and returns the upstream
// body. Returns an error with an OpenAI-style message string.
func ToCommandCode(req ChatRequest) (CommandCodeBody, error) {
	if req.Model == "" {
		return CommandCodeBody{}, errors.New(`field "model" is required`)
	}
	if len(req.Messages) == 0 {
		return CommandCodeBody{}, errors.New(`field "messages" must be a non-empty array`)
	}
	provider := req.Provider
	if provider == "" {
		provider = DefaultProvider
	}
	cfg := req.Config
	if cfg == nil {
		cfg = BuildDefaultConfig()
	}
	return CommandCodeBody{
		Memory: req.Memory,
		Params: CommandParams{
			Provider: provider,
			Model:    req.Model,
			Messages: normalizeMessages(req.Messages),
		},
		Config: cfg,
	}, nil
}

// MapFinishReason translates upstream stop_reason to OpenAI finish_reason.
func MapFinishReason(s string) string {
	if s == "" {
		return "stop"
	}
	if v, ok := stopReasonMap[s]; ok {
		return v
	}
	return "stop"
}

// FlattenUpstreamContent extracts text from the upstream content shape,
// which is an array of {type:"text", text:"..."} objects.
func FlattenUpstreamContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		out := ""
		for _, p := range c {
			switch v := p.(type) {
			case string:
				out += v
			case map[string]any:
				if t, ok := v["text"].(string); ok {
					out += t
				}
			}
		}
		return out
	}
	return ""
}

// ChatCompletion is the OpenAI Chat Completion response shape.
type ChatCompletion struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []ChatChoice   `json:"choices"`
	Usage   ChatUsage      `json:"usage"`
}

// ChatChoice is one entry in choices[].
type ChatChoice struct {
	Index        int            `json:"index"`
	Message      ChatRespMsg    `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

// ChatRespMsg is the response message body.
type ChatRespMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatUsage is OpenAI's usage shape.
type ChatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// ToOpenAI converts an upstream parsed body into an OpenAI ChatCompletion.
func ToOpenAI(upstream map[string]any, requestModel string) ChatCompletion {
	id, _ := upstream["id"].(string)
	if id == "" {
		id = "chatcmpl-" + randomHex(12)
	}
	role, _ := upstream["role"].(string)
	if role == "" {
		role = "assistant"
	}
	stopReason, _ := upstream["stop_reason"].(string)

	model := requestModel
	if model == "" {
		if m, ok := upstream["model"].(string); ok {
			model = m
		}
	}

	usage, _ := upstream["usage"].(map[string]any)
	in := numAsInt(usage["input_tokens"])
	out := numAsInt(usage["output_tokens"])

	return ChatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatChoice{
			{
				Index:        0,
				Message:      ChatRespMsg{Role: role, Content: FlattenUpstreamContent(upstream["content"])},
				FinishReason: MapFinishReason(stopReason),
			},
		},
		Usage: ChatUsage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out},
	}
}

// StreamChunk is one frame of the SSE response.
type StreamChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []StreamChoice      `json:"choices"`
}

// StreamChoice is one choice within a chunk.
type StreamChoice struct {
	Index        int                    `json:"index"`
	Delta        map[string]any         `json:"delta"`
	FinishReason any                    `json:"finish_reason"`
}

// BuildStreamChunks emits the role chunk -> content chunk -> finish
// chunk sequence for an already-buffered upstream response. Output is
// terminated externally by writing "data: [DONE]\n\n".
func BuildStreamChunks(upstream map[string]any, requestModel string) []StreamChunk {
	id, _ := upstream["id"].(string)
	if id == "" {
		id = "chatcmpl-" + randomHex(12)
	}
	model := requestModel
	if model == "" {
		if m, ok := upstream["model"].(string); ok {
			model = m
		}
	}
	created := time.Now().Unix()
	text := FlattenUpstreamContent(upstream["content"])
	stopReason, _ := upstream["stop_reason"].(string)
	finishReason := MapFinishReason(stopReason)

	chunks := []StreamChunk{
		{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []StreamChoice{{Index: 0, Delta: map[string]any{"role": "assistant"}, FinishReason: nil}},
		},
	}
	if text != "" {
		chunks = append(chunks, StreamChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []StreamChoice{{Index: 0, Delta: map[string]any{"content": text}, FinishReason: nil}},
		})
	}
	chunks = append(chunks, StreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []StreamChoice{{Index: 0, Delta: map[string]any{}, FinishReason: finishReason}},
	})
	return chunks
}

// Model is one entry in the /v1/models response.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelsResponse is the /v1/models payload.
type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// BuildModelsResponse returns the OpenAI-shaped list for the given IDs.
func BuildModelsResponse(ids []string) ModelsResponse {
	now := time.Now().Unix()
	out := make([]Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, Model{ID: id, Object: "model", Created: now, OwnedBy: DefaultProvider})
	}
	return ModelsResponse{Object: "list", Data: out}
}

// MergeModelLists deduplicates model IDs across multiple sources while
// preserving order: env-configured IDs first (so the static
// OPENAI_MODELS list stays the canonical advertised set), then any
// extras (e.g. models discovered from analytics history) appended in
// order. Empty strings are skipped.
func MergeModelLists(sources ...[]string) []string {
	seen := make(map[string]struct{}, 16)
	var out []string
	for _, src := range sources {
		for _, id := range src {
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

func numAsInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	}
	return 0
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// astronomically unlikely; fall back to deterministic value
		return "abcdef0123456789"[:n*2]
	}
	return hex.EncodeToString(b)
}
