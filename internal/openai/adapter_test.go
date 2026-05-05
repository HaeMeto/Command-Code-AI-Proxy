package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToCommandCode_StringContent(t *testing.T) {
	req := ChatRequest{
		Model: "moonshotai/Kimi-K2.5",
		Messages: []ChatMessage{
			{Role: "user", Content: "Hello"},
		},
	}
	out, err := ToCommandCode(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Params.Provider != "command-code" {
		t.Errorf("provider = %q, want command-code", out.Params.Provider)
	}
	if out.Params.Model != "moonshotai/Kimi-K2.5" {
		t.Errorf("model = %q", out.Params.Model)
	}
	if len(out.Params.Messages) != 1 {
		t.Fatalf("messages = %+v", out.Params.Messages)
	}
	parts := out.Params.Messages[0].Content
	if len(parts) != 1 || parts[0].Type != "text" || parts[0].Text != "Hello" {
		t.Errorf("content parts = %+v, want [{text Hello}]", parts)
	}
}

func TestToCommandCode_ArrayContent(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":"Hi "},{"type":"text","text":"there"}]}]}`)
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := ToCommandCode(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	parts := out.Params.Messages[0].Content
	if len(parts) != 2 {
		t.Fatalf("parts = %+v, want 2", parts)
	}
	if parts[0].Text != "Hi " || parts[1].Text != "there" {
		t.Errorf("parts texts = %q, %q", parts[0].Text, parts[1].Text)
	}
}

func TestToCommandCode_SystemFoldedIntoNextUser(t *testing.T) {
	req := ChatRequest{
		Model: "x",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "hi"},
		},
	}
	out, err := ToCommandCode(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out.Params.Messages) != 1 {
		t.Fatalf("messages = %+v, want 1 (system folded)", out.Params.Messages)
	}
	m := out.Params.Messages[0]
	if m.Role != "user" {
		t.Errorf("role = %q, want user", m.Role)
	}
	if len(m.Content) != 1 || m.Content[0].Type != "text" {
		t.Fatalf("content = %+v", m.Content)
	}
	if m.Content[0].Text != "You are helpful.\n\nhi" {
		t.Errorf("folded text = %q", m.Content[0].Text)
	}
}

func TestToCommandCode_MultipleSystemsCollapsed(t *testing.T) {
	req := ChatRequest{
		Model: "x",
		Messages: []ChatMessage{
			{Role: "system", Content: "Rule 1"},
			{Role: "system", Content: "Rule 2"},
			{Role: "user", Content: "hello"},
		},
	}
	out, _ := ToCommandCode(req)
	if len(out.Params.Messages) != 1 {
		t.Fatalf("messages = %+v", out.Params.Messages)
	}
	got := out.Params.Messages[0].Content[0].Text
	want := "Rule 1\n\nRule 2\n\nhello"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestToCommandCode_SystemWithoutFollowingUser(t *testing.T) {
	req := ChatRequest{
		Model: "x",
		Messages: []ChatMessage{
			{Role: "user", Content: "earlier user message"},
			{Role: "system", Content: "trailing system"},
		},
	}
	out, _ := ToCommandCode(req)
	if len(out.Params.Messages) != 1 {
		t.Fatalf("messages = %+v", out.Params.Messages)
	}
	got := out.Params.Messages[0].Content[0].Text
	if got != "trailing system\n\nearlier user message" {
		t.Errorf("trailing fold = %q", got)
	}
}

func TestToCommandCode_OnlySystem_BecomesUser(t *testing.T) {
	req := ChatRequest{
		Model:    "x",
		Messages: []ChatMessage{{Role: "system", Content: "act as helpful"}},
	}
	out, _ := ToCommandCode(req)
	if len(out.Params.Messages) != 1 || out.Params.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", out.Params.Messages)
	}
	if out.Params.Messages[0].Content[0].Text != "act as helpful" {
		t.Errorf("text = %q", out.Params.Messages[0].Content[0].Text)
	}
}

func TestToCommandCode_UnknownRoleCoercedToUser(t *testing.T) {
	req := ChatRequest{
		Model: "x",
		Messages: []ChatMessage{
			{Role: "function", Content: "unsupported role"},
		},
	}
	out, _ := ToCommandCode(req)
	if out.Params.Messages[0].Role != "user" {
		t.Errorf("role = %q, want user (coerced from function)", out.Params.Messages[0].Role)
	}
}

func TestToCommandCode_AssistantRolePassesThrough(t *testing.T) {
	req := ChatRequest{
		Model: "x",
		Messages: []ChatMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello back"},
			{Role: "user", Content: "thanks"},
		},
	}
	out, _ := ToCommandCode(req)
	roles := []string{out.Params.Messages[0].Role, out.Params.Messages[1].Role, out.Params.Messages[2].Role}
	if roles[0] != "user" || roles[1] != "assistant" || roles[2] != "user" {
		t.Errorf("roles = %v", roles)
	}
}

func TestToCommandCode_StringContentAlwaysWrapped(t *testing.T) {
	// Regression: CommandCode rejects content as a plain string with 400.
	// Even single-string content must serialize to JSON as an array.
	req := ChatRequest{
		Model:    "x",
		Messages: []ChatMessage{{Role: "user", Content: "plain string"}},
	}
	out, _ := ToCommandCode(req)
	b, _ := json.Marshal(out)
	s := string(b)
	if !strings.Contains(s, `"content":[{"type":"text","text":"plain string"}]`) {
		t.Errorf("serialized content not wrapped as array; got: %s", s)
	}
	if strings.Contains(s, `"content":"plain string"`) {
		t.Errorf("serialized content is a bare string (would 400 on upstream): %s", s)
	}
}

func TestToCommandCode_Validation(t *testing.T) {
	if _, err := ToCommandCode(ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "x"}}}); err == nil {
		t.Error("expected error for missing model")
	}
	if _, err := ToCommandCode(ChatRequest{Model: "x"}); err == nil {
		t.Error("expected error for missing messages")
	}
}

func TestToOpenAI(t *testing.T) {
	upstream := map[string]any{
		"id":          "msg_abc",
		"role":        "assistant",
		"content":     []any{map[string]any{"type": "text", "text": " Hello! How can I help you today?"}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": float64(7280), "output_tokens": float64(10)},
	}
	cc := ToOpenAI(upstream, "moonshotai/Kimi-K2.5")
	if cc.ID != "msg_abc" || cc.Object != "chat.completion" {
		t.Errorf("id/object = %q/%q", cc.ID, cc.Object)
	}
	if cc.Model != "moonshotai/Kimi-K2.5" {
		t.Errorf("model = %q", cc.Model)
	}
	if cc.Choices[0].Message.Content != " Hello! How can I help you today?" {
		t.Errorf("content = %q", cc.Choices[0].Message.Content)
	}
	if cc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", cc.Choices[0].FinishReason)
	}
	if cc.Usage.PromptTokens != 7280 || cc.Usage.CompletionTokens != 10 || cc.Usage.TotalTokens != 7290 {
		t.Errorf("usage = %+v", cc.Usage)
	}
}

func TestBuildStreamChunks(t *testing.T) {
	upstream := map[string]any{
		"id":          "msg_abc",
		"content":     []any{map[string]any{"type": "text", "text": "Hi"}},
		"stop_reason": "max_tokens",
	}
	chunks := BuildStreamChunks(upstream, "x")
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	if r, _ := chunks[0].Choices[0].Delta["role"].(string); r != "assistant" {
		t.Errorf("first chunk role = %q", r)
	}
	if c, _ := chunks[1].Choices[0].Delta["content"].(string); c != "Hi" {
		t.Errorf("second chunk content = %q", c)
	}
	if fr, _ := chunks[2].Choices[0].FinishReason.(string); fr != "length" {
		t.Errorf("finish chunk = %q, want length", fr)
	}
}

func TestBuildModelsResponse(t *testing.T) {
	out := BuildModelsResponse([]string{"a", "b"})
	if out.Object != "list" || len(out.Data) != 2 {
		t.Errorf("got %+v", out)
	}
	if out.Data[0].ID != "a" || out.Data[0].OwnedBy != "command-code" {
		t.Errorf("data[0] = %+v", out.Data[0])
	}
}

func TestMergeModelLists(t *testing.T) {
	cases := []struct {
		name    string
		sources [][]string
		want    []string
	}{
		{
			name:    "env only",
			sources: [][]string{{"a", "b"}},
			want:    []string{"a", "b"},
		},
		{
			name:    "env plus extras dedupes preserving env order",
			sources: [][]string{{"a", "b"}, {"b", "c"}},
			want:    []string{"a", "b", "c"},
		},
		{
			name:    "empty strings ignored",
			sources: [][]string{{"a", ""}, {"", "b"}},
			want:    []string{"a", "b"},
		},
		{
			name:    "no sources",
			sources: nil,
			want:    nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MergeModelLists(c.sources...)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]string{
		"":              "stop",
		"end_turn":      "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"unknown_thing": "stop",
	}
	for in, want := range cases {
		if got := MapFinishReason(in); got != want {
			t.Errorf("MapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFlattenContentWithImage(t *testing.T) {
	body := []byte(`{"role":"user","content":[{"type":"text","text":"see "},{"type":"image_url","image_url":{"url":"x"}}]}`)
	var m ChatMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := FlattenContent(m.Content)
	if !strings.Contains(got, "see") || !strings.Contains(got, "[image omitted]") {
		t.Errorf("flatten = %q", got)
	}
}
