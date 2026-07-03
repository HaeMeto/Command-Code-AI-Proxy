package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolMessage_WireFormat(t *testing.T) {
	req := ChatRequest{
		Model: "x",
		Messages: []ChatMessage{
			{Role: "user", Content: "What files are in /tmp?"},
			{Role: "assistant", Content: "Let me check.", ToolCalls: []ToolCall{
				{ID: "call_001", Type: "function", Function: FuncCall{Name: "list_files", Arguments: `{"path":"/tmp"}`}},
			}},
			{Role: "tool", Content: "file1.txt\nfile2.txt", ToolCallID: "call_001", Name: "list_files"},
			{Role: "assistant", Content: "There are two files."},
		},
	}
	out, err := ToCommandCode(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	b, _ := json.Marshal(out)
	s := string(b)
	t.Logf("wire format (compact): %s", s)

	if !strings.Contains(s, `"type":"tool_result"`) {
		t.Error("missing tool_result type")
	}
	if !strings.Contains(s, `"tool_use_id":"call_001"`) {
		t.Error("missing tool_use_id")
	}
	if !strings.Contains(s, `"content":[{"type":"text"`) {
		t.Error("tool_result content should be an array of text parts, not a plain string")
	}
}

func TestToolMessage_ToolUseBlocksInAssistant(t *testing.T) {
	req := ChatRequest{
		Model: "x",
		Messages: []ChatMessage{
			{Role: "user", Content: "list files"},
			{Role: "assistant", Content: nil, ToolCalls: []ToolCall{
				{ID: "call_a", Type: "function", Function: FuncCall{Name: "ls", Arguments: "{}"}},
				{ID: "call_b", Type: "function", Function: FuncCall{Name: "pwd", Arguments: "{}"}},
			}},
			{Role: "tool", Content: "result_a", ToolCallID: "call_a"},
			{Role: "tool", Content: "result_b", ToolCallID: "call_b"},
			{Role: "assistant", Content: "done"},
		},
	}
	out, err := ToCommandCode(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	b, _ := json.Marshal(out)
	s := string(b)
	t.Logf("wire format (compact): %s", s)

	if strings.Count(s, `"type":"tool_use"`) != 2 {
		t.Error("expected 2 tool_use blocks")
	}
	if strings.Count(s, `"type":"tool_result"`) != 2 {
		t.Error("expected 2 tool_result blocks")
	}
	roleUserCount := strings.Count(s, `"role":"user"`)
	if roleUserCount != 3 {
		t.Errorf("expected 3 user messages (1 original + 2 tool results), got %d", roleUserCount)
	}
	roleAssistantCount := strings.Count(s, `"role":"assistant"`)
	if roleAssistantCount != 2 {
		t.Errorf("expected 2 assistant messages, got %d", roleAssistantCount)
	}
}
