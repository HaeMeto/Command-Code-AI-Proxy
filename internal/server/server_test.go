package server

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
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/proxy"
)

// fixtureUpstream is the canonical CommandCode response from the user issue.
var fixtureUpstream = map[string]any{
	"id":      "msg_faf0b2b3-01fc-4c6b-9d47-ddeba29fa0a8",
	"type":    "message",
	"role":    "assistant",
	"content": []any{map[string]any{"type": "text", "text": " Hello! How can I help you today?"}},
	"model":   "",
	"stop_reason": "end_turn",
	"usage": map[string]any{
		"input_tokens":                7280,
		"output_tokens":               10,
		"cache_read_input_tokens":     0,
		"cache_creation_input_tokens": 0,
	},
}

func newApp(t *testing.T, cfg config.Config) (http.Handler, *analytics.DB) {
	t.Helper()
	db, err := analytics.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	p := proxy.New(cfg, db, http.DefaultClient)
	return Build(cfg, db, p), db
}

func startUpstream(t *testing.T, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthz(t *testing.T) {
	app, _ := newApp(t, config.Config{})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
	var got map[string]bool
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got["ok"] {
		t.Errorf("body = %v", got)
	}
}

func TestNativeAlphaGenerate_E2E(t *testing.T) {
	upstream := startUpstream(t, fixtureUpstream)
	app, db := newApp(t, config.Config{UpstreamURL: upstream.URL, DefaultVersion: "0.18.10"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/alpha/generate", strings.NewReader(`{"params":{"provider":"command-code","model":"moonshotai/Kimi-K2.5"}}`))
	req.Header.Set("Authorization", "Bearer user_test_token")
	app.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Native pass-through should preserve CommandCode shape (NOT translated to OpenAI).
	if _, hasChoices := got["choices"]; hasChoices {
		t.Errorf("native passthrough leaked OpenAI shape: %+v", got)
	}
	if got["id"] != "msg_faf0b2b3-01fc-4c6b-9d47-ddeba29fa0a8" {
		t.Errorf("id = %v", got["id"])
	}

	s, _ := db.Summary()
	if s.Totals.TotalInputTokens != 7280 || s.Totals.TotalOutputTokens != 10 {
		t.Errorf("analytics = %+v", s.Totals)
	}
}

func TestOpenAIChatCompletion_NonStream(t *testing.T) {
	upstream := startUpstream(t, fixtureUpstream)
	app, db := newApp(t, config.Config{UpstreamURL: upstream.URL, DefaultVersion: "0.18.10"})

	body := `{"model":"moonshotai/Kimi-K2.5","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt int `json:"prompt_tokens"`
			Compl  int `json:"completion_tokens"`
			Total  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q", got.Object)
	}
	if got.Model != "moonshotai/Kimi-K2.5" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Choices[0].Message.Content != " Hello! How can I help you today?" {
		t.Errorf("content = %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.Choices[0].FinishReason)
	}
	if got.Usage.Prompt != 7280 || got.Usage.Compl != 10 || got.Usage.Total != 7290 {
		t.Errorf("usage = %+v", got.Usage)
	}

	// Analytics path should be /v1/chat/completions
	rec, _ := db.Recent(1)
	if len(rec) != 1 || rec[0].Path != "/v1/chat/completions" {
		t.Errorf("recent path = %+v", rec)
	}
}

func TestOpenAIChatCompletion_Streaming(t *testing.T) {
	upstream := startUpstream(t, fixtureUpstream)
	app, _ := newApp(t, config.Config{UpstreamURL: upstream.URL, DefaultVersion: "0.18.10"})

	body := `{"model":"moonshotai/Kimi-K2.5","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	out := w.Body.String()
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("no role chunk: %s", out)
	}
	if !strings.Contains(out, "Hello! How can I help you today?") {
		t.Errorf("no content chunk: %s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Errorf("not terminated with [DONE]: ...%q", out[len(out)-80:])
	}
}

func TestModels(t *testing.T) {
	app, _ := newApp(t, config.Config{OpenAIModels: []string{"moonshotai/Kimi-K2.5", "claude-sonnet-4"}})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	_ = json.NewDecoder(w.Body).Decode(&got)
	if got.Object != "list" || len(got.Data) != 2 || got.Data[0].ID != "moonshotai/Kimi-K2.5" {
		t.Errorf("got %+v", got)
	}
}

func TestAdminToken_BlocksAndAllows(t *testing.T) {
	upstream := startUpstream(t, fixtureUpstream)
	app, _ := newApp(t, config.Config{UpstreamURL: upstream.URL, AdminToken: "sekret"})

	// blocked
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/analytics", nil))
	if w.Code != 401 {
		t.Errorf("status without token = %d, want 401", w.Code)
	}
	// allowed via header
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/analytics", nil)
	r.Header.Set("X-Admin-Token", "sekret")
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("status with token = %d", w.Code)
	}
	// allowed via query
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/analytics?token=sekret", nil))
	if w.Code != 200 {
		t.Errorf("status with query token = %d", w.Code)
	}
}

func TestDashboardServed(t *testing.T) {
	app, _ := newApp(t, config.Config{})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/dashboard", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("commandcode-proxy analytics")) {
		t.Errorf("dashboard html missing title")
	}
}

func TestMethodOnly(t *testing.T) {
	app, _ := newApp(t, config.Config{})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/alpha/generate", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", w.Code)
	}
}

func TestAnalyticsDetail_CapturesAllFourBodies_OpenAIPath(t *testing.T) {
	upstream := startUpstream(t, fixtureUpstream)
	app, _ := newApp(t, config.Config{UpstreamURL: upstream.URL, AdminToken: "sekret"})

	clientBody := `{"model":"moonshotai/Kimi-K2.5","messages":[{"role":"user","content":"Hello from OpenRouter"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(clientBody))
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("OpenAI request status = %d", w.Code)
	}

	// Pull recent to discover the row id, then fetch detail.
	rec := httptest.NewRequest("GET", "/analytics/recent?limit=1", nil)
	rec.Header.Set("X-Admin-Token", "sekret")
	rw := httptest.NewRecorder()
	app.ServeHTTP(rw, rec)
	var recent struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(rw.Body).Decode(&recent); err != nil {
		t.Fatalf("decode recent: %v", err)
	}
	if len(recent.Items) != 1 || recent.Items[0].ID == 0 {
		t.Fatalf("recent = %+v", recent)
	}
	id := recent.Items[0].ID

	dreq := httptest.NewRequest("GET", "/analytics/detail/"+strings.TrimSpace(strconvFmt(id)), nil)
	dreq.Header.Set("X-Admin-Token", "sekret")
	dw := httptest.NewRecorder()
	app.ServeHTTP(dw, dreq)
	if dw.Code != 200 {
		t.Fatalf("detail status = %d, body=%s", dw.Code, dw.Body.String())
	}

	var detail map[string]any
	if err := json.NewDecoder(dw.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}

	// Client request body must contain the OpenAI-shaped payload (proves we capture
	// what OpenRouter / OpenAI SDK sent, not just the translated upstream body).
	clientReq, _ := detail["client_request_body"].(string)
	if !strings.Contains(clientReq, "Hello from OpenRouter") {
		t.Errorf("client_request_body = %q, want it to contain the original OpenAI shape", clientReq)
	}
	if !strings.Contains(clientReq, `"messages"`) {
		t.Errorf("client_request_body missing OpenAI 'messages' shape: %q", clientReq)
	}

	// Upstream request body must be the translated CommandCode shape (params.provider, etc.).
	upstreamReq, _ := detail["upstream_request_body"].(string)
	if !strings.Contains(upstreamReq, `"params"`) || !strings.Contains(upstreamReq, `"command-code"`) {
		t.Errorf("upstream_request_body = %q, want CommandCode-shaped body", upstreamReq)
	}

	// Upstream response body must contain CommandCode's raw msg id.
	upstreamResp, _ := detail["upstream_response_body"].(string)
	if !strings.Contains(upstreamResp, "msg_faf0b2b3-01fc-4c6b-9d47-ddeba29fa0a8") {
		t.Errorf("upstream_response_body missing msg id: %q", upstreamResp)
	}

	// Client response body must contain OpenAI shape (chat.completion).
	clientResp, _ := detail["client_response_body"].(string)
	if !strings.Contains(clientResp, "chat.completion") {
		t.Errorf("client_response_body missing OpenAI shape: %q", clientResp)
	}
}

func TestAnalyticsDetail_NativePath_BodiesEqual(t *testing.T) {
	upstream := startUpstream(t, fixtureUpstream)
	app, _ := newApp(t, config.Config{UpstreamURL: upstream.URL, AdminToken: "sekret"})

	clientBody := `{"params":{"provider":"command-code","model":"moonshotai/Kimi-K2.5","messages":[{"role":"user","content":"hi"}]}}`
	req := httptest.NewRequest("POST", "/alpha/generate", strings.NewReader(clientBody))
	req.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("native status = %d", w.Code)
	}

	rec := httptest.NewRequest("GET", "/analytics/recent?limit=1", nil)
	rec.Header.Set("X-Admin-Token", "sekret")
	rw := httptest.NewRecorder()
	app.ServeHTTP(rw, rec)
	var recent struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	_ = json.NewDecoder(rw.Body).Decode(&recent)
	id := recent.Items[0].ID

	dreq := httptest.NewRequest("GET", "/analytics/detail/"+strconvFmt(id), nil)
	dreq.Header.Set("X-Admin-Token", "sekret")
	dw := httptest.NewRecorder()
	app.ServeHTTP(dw, dreq)
	if dw.Code != 200 {
		t.Fatalf("detail status = %d", dw.Code)
	}

	var detail map[string]any
	_ = json.NewDecoder(dw.Body).Decode(&detail)

	// Native: client request and upstream request are byte-equal.
	if detail["client_request_body"] != detail["upstream_request_body"] {
		t.Errorf("native: client req != upstream req:\n  client=%v\n  upstream=%v",
			detail["client_request_body"], detail["upstream_request_body"])
	}
	// Native: client response and upstream response are byte-equal.
	if detail["client_response_body"] != detail["upstream_response_body"] {
		t.Errorf("native: client resp != upstream resp")
	}
}

func TestAnalyticsDetail_AdminGate_Required(t *testing.T) {
	upstream := startUpstream(t, fixtureUpstream)
	app, _ := newApp(t, config.Config{UpstreamURL: upstream.URL, AdminToken: "sekret"})

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/analytics/detail/1", nil))
	if w.Code != 401 {
		t.Errorf("detail without admin token = %d, want 401", w.Code)
	}
}

func TestAnalyticsDetail_NotFound(t *testing.T) {
	app, _ := newApp(t, config.Config{})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/analytics/detail/9999999", nil))
	if w.Code != 404 {
		t.Errorf("missing id = %d, want 404", w.Code)
	}
}

func TestAnalyticsDetail_BadID(t *testing.T) {
	app, _ := newApp(t, config.Config{})
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/analytics/detail/abc", nil))
	if w.Code != 400 {
		t.Errorf("bad id = %d, want 400", w.Code)
	}
}

// TestModels_DiscoversFromAnalytics: a model not in OPENAI_MODELS but
// actually used through /v1/chat/completions should appear in /v1/models
// after the request lands. Chat completions never gates on the env list.
func TestModels_DiscoversFromAnalytics(t *testing.T) {
	upstream := startUpstream(t, fixtureUpstream)
	app, _ := newApp(t, config.Config{
		UpstreamURL:  upstream.URL,
		OpenAIModels: []string{"moonshotai/Kimi-K2.5"},
	})

	// Initial /v1/models: only the env list.
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	var first struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(w.Body).Decode(&first)
	if len(first.Data) != 1 || first.Data[0].ID != "moonshotai/Kimi-K2.5" {
		t.Fatalf("initial models = %+v", first.Data)
	}

	// Use a model that is NOT in OPENAI_MODELS — should still succeed.
	body := `{"model":"acme/some-custom-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	req.Header.Set("Content-Type", "application/json")
	cw := httptest.NewRecorder()
	app.ServeHTTP(cw, req)
	if cw.Code != 200 {
		t.Fatalf("custom-model chat status = %d, body = %s", cw.Code, cw.Body.String())
	}

	// /v1/models should now include the custom model.
	w2 := httptest.NewRecorder()
	app.ServeHTTP(w2, httptest.NewRequest("GET", "/v1/models", nil))
	var after struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(w2.Body).Decode(&after)
	ids := make([]string, len(after.Data))
	for i, m := range after.Data {
		ids[i] = m.ID
	}
	hasEnv, hasCustom := false, false
	for _, id := range ids {
		if id == "moonshotai/Kimi-K2.5" {
			hasEnv = true
		}
		if id == "acme/some-custom-model" {
			hasCustom = true
		}
	}
	if !hasEnv || !hasCustom {
		t.Errorf("expected both env model and custom model, got %v", ids)
	}
	// Env list should be first (canonical order preserved).
	if ids[0] != "moonshotai/Kimi-K2.5" {
		t.Errorf("env model not first: %v", ids)
	}
}

func strconvFmt(i int64) string {
	// avoid pulling strconv into a test helper just for itoa.
	return jsonNumStr(i)
}

func jsonNumStr(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// silence unused warning for io if not needed:
var _ = io.Discard
