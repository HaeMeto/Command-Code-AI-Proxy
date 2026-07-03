package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/analytics"
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/config"
)

func newDB(t *testing.T) *analytics.DB {
	t.Helper()
	db, err := analytics.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// upstreamMock starts an httptest server that returns a fixed CommandCode-shaped response.
func upstreamMock(t *testing.T, headersSeen *http.Header, body any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if headersSeen != nil {
			*headersSeen = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func TestForward_Success_RecordsAnalytics(t *testing.T) {
	var seen http.Header
	upstream := upstreamMock(t, &seen, map[string]any{
		"id": "msg_test", "role": "assistant",
		"content":     []any{map[string]any{"type": "text", "text": "Hi"}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 7280, "output_tokens": 10, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
	})
	defer upstream.Close()

	db := newDB(t)
	cfg := config.Config{UpstreamURL: upstream.URL, DefaultVersion: "0.18.10"}
	p := New(cfg, db, http.DefaultClient)

	body := []byte(`{"params":{"provider":"command-code","model":"moonshotai/Kimi-K2.5"},"memory":""}`)
	req := httptest.NewRequest(http.MethodPost, "/alpha/generate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer user_test_token")
	req.Header.Set("X-Command-Code-Version", "0.18.10")
	req.Header.Set("Content-Type", "application/json")

	res := p.Forward(req, ForwardOptions{Body: body})
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200; err=%q", res.StatusCode, res.ErrorMessage)
	}
	if seen.Get("Authorization") != "Bearer user_test_token" {
		t.Errorf("auth not forwarded: %q", seen.Get("Authorization"))
	}
	if seen.Get("X-Command-Code-Version") != "0.18.10" {
		t.Errorf("version not forwarded: %q", seen.Get("X-Command-Code-Version"))
	}

	s, err := db.Summary()
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if s.Totals.TotalRequests != 1 || s.Totals.TotalInputTokens != 7280 || s.Totals.TotalOutputTokens != 10 {
		t.Errorf("recorded summary = %+v", s.Totals)
	}
}

func TestForward_NDJSONResponse(t *testing.T) {
	ndjson := []byte(`{"type":"start"}
{"type":"text-start","id":"0"}
{"type":"text-delta","id":"0","text":" Hello"}
{"type":"text-end","id":"0"}
{"type":"finish-step","finishReason":"stop","usage":{"inputTokens":5000,"outputTokens":5}}
{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":5000,"outputTokens":5}}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		_, _ = w.Write(ndjson)
	}))
	defer upstream.Close()

	db := newDB(t)
	cfg := config.Config{UpstreamURL: upstream.URL, DefaultVersion: "0.18.10"}
	p := New(cfg, db, http.DefaultClient)

	body := []byte(`{"params":{"provider":"command-code","model":"x"},"memory":""}`)
	req := httptest.NewRequest(http.MethodPost, "/alpha/generate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")

	res := p.Forward(req, ForwardOptions{Body: body})
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, err=%q", res.StatusCode, res.ErrorMessage)
	}
	if res.ParsedBody == nil {
		t.Fatal("ParsedBody is nil")
	}
	content := res.ParsedBody["content"]
	parts, ok := content.([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content = %+v, want 1 part", content)
	}
	part := parts[0].(map[string]any)
	if part["text"] != " Hello" {
		t.Errorf("text = %q, want %q", part["text"], " Hello")
	}
	if res.ParsedBody["stop_reason"] != "stop" {
		t.Errorf("stop_reason = %q", res.ParsedBody["stop_reason"])
	}

	s, _ := db.Summary()
	if s.Totals.TotalInputTokens != 5000 || s.Totals.TotalOutputTokens != 5 {
		t.Errorf("tokens = %+v", s.Totals)
	}
}

func TestForward_DefaultTokenAppliedWhenMissing(t *testing.T) {
	var seen http.Header
	upstream := upstreamMock(t, &seen, map[string]any{"id": "x", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
	defer upstream.Close()

	db := newDB(t)
	cfg := config.Config{UpstreamURL: upstream.URL, DefaultToken: "default_user_token", DefaultVersion: "0.18.10"}
	p := New(cfg, db, http.DefaultClient)

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/alpha/generate", bytes.NewReader(body))
	res := p.Forward(req, ForwardOptions{Body: body})

	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200; err=%q", res.StatusCode, res.ErrorMessage)
	}
	if seen.Get("Authorization") != "Bearer default_user_token" {
		t.Errorf("default token not applied: %q", seen.Get("Authorization"))
	}
}

func TestForward_AuthMissingReturns401(t *testing.T) {
	db := newDB(t)
	cfg := config.Config{UpstreamURL: "http://127.0.0.1:1", DefaultVersion: "0.18.10"}
	p := New(cfg, db, http.DefaultClient)

	req := httptest.NewRequest(http.MethodPost, "/alpha/generate", bytes.NewReader([]byte(`{}`)))
	res := p.Forward(req, ForwardOptions{Body: []byte(`{}`)})

	if res.StatusCode != 401 || !res.AuthMissing {
		t.Errorf("res = %+v, want 401 AuthMissing", res)
	}
}

func TestForward_UpstreamUnreachableReturns502(t *testing.T) {
	db := newDB(t)
	// Port 1 is virtually guaranteed to be closed.
	cfg := config.Config{UpstreamURL: "http://127.0.0.1:1", DefaultVersion: "0.18.10"}
	p := New(cfg, db, http.DefaultClient)
	body := []byte(`{}`)

	req := httptest.NewRequest(http.MethodPost, "/alpha/generate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	res := p.Forward(req, ForwardOptions{Body: body})

	if res.StatusCode != 502 || res.ErrorMessage == "" {
		t.Errorf("res = %+v, want 502 with error", res)
	}
}

func TestNativeHandler_PassesThroughBody(t *testing.T) {
	upstreamBody := map[string]any{
		"id": "msg_passthrough", "role": "assistant",
		"content": []any{map[string]any{"type": "text", "text": "OK"}},
	}
	upstream := upstreamMock(t, nil, upstreamBody)
	defer upstream.Close()

	db := newDB(t)
	cfg := config.Config{UpstreamURL: upstream.URL, DefaultVersion: "0.18.10"}
	p := New(cfg, db, http.DefaultClient)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/alpha/generate", strings.NewReader(`{"params":{"provider":"command-code","model":"x"}}`))
	req.Header.Set("Authorization", "Bearer x")
	p.Handler().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	got, _ := io.ReadAll(w.Body)
	if !bytes.Contains(got, []byte("msg_passthrough")) {
		t.Errorf("response body did not contain upstream id; got=%s", string(got))
	}
}
