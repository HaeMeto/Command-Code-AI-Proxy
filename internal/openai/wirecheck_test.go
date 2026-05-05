package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestWireFormat_RealOpenRouterPayload reproduces the exact request shape
// from a real OpenRouter call and asserts the serialized upstream body
// satisfies the constraints CommandCode reported in its 400 error:
//   - role in {"user","assistant","tool"}
//   - content as array
func TestWireFormat_RealOpenRouterPayload(t *testing.T) {
	raw := `{
		"model": "moonshotai/Kimi-K2.5",
		"messages": [
			{"role": "system", "content": "# Role\nYou are a coding assistant."},
			{"role": "user", "content": "## Workspace context\n\nhai"}
		],
		"stream": false
	}`
	var req ChatRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cc, err := ToCommandCode(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	out, err := json.Marshal(cc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(out)

	// Forbidden: any system role on the wire.
	if strings.Contains(wire, `"role":"system"`) {
		t.Errorf("system role leaked through to upstream:\n%s", wire)
	}
	// Forbidden: content as a bare string.
	if strings.Contains(wire, `"content":"`) {
		t.Errorf("content serialized as string (would 400 upstream):\n%s", wire)
	}
	// Required: content as an array of typed parts.
	if !strings.Contains(wire, `"content":[{"type":"text"`) {
		t.Errorf("content not array of typed parts:\n%s", wire)
	}
	// Required: system text folded into user message.
	if !strings.Contains(wire, "Role\\nYou are a coding assistant.") {
		t.Errorf("system instruction lost:\n%s", wire)
	}
	if !strings.Contains(wire, "Workspace context") {
		t.Errorf("user message lost:\n%s", wire)
	}
	// Required: only one message after folding.
	if len(cc.Params.Messages) != 1 {
		t.Errorf("messages count = %d, want 1 (system folded into user)", len(cc.Params.Messages))
	}
	// Print on failure for debugging.
	if t.Failed() {
		fmt.Println(wire)
	}
}
