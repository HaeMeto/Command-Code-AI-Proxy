// Package proxy forwards requests to the upstream CommandCode API and
// records analytics. It is the shared core used by both the native
// passthrough handler and the OpenAI-compatible adapter.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/analytics"
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/config"
)

// HTTPClient is the subset of *http.Client used by Proxy. Tests stub it.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Proxy is the long-lived component that owns the upstream client,
// analytics handle, and config.
type Proxy struct {
	cfg       config.Config
	analytics *analytics.DB
	client    HTTPClient
}

// New constructs a Proxy.
func New(cfg config.Config, db *analytics.DB, client HTTPClient) *Proxy {
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	return &Proxy{cfg: cfg, analytics: db, client: client}
}

// Result is the outcome of a single forward call.
type Result struct {
	StatusCode    int
	DurationMs    int64
	Body          []byte
	ContentType   string
	ErrorMessage  string
	AuthMissing   bool
	ParsedBody    map[string]any
	ResponseBytes int64
	EntryID       int64 // analytics row id, so handlers can update client_response_body
}

// ForwardOptions controls Forward.
type ForwardOptions struct {
	UpstreamPath string
	// Body is what we send upstream to CommandCode. For the native
	// passthrough this equals the client's body verbatim; for the
	// OpenAI adapter it's the translated CommandCode-shaped body.
	Body []byte
	// ClientRequestBody is what the client originally sent. If empty,
	// Forward defaults it to Body. The dashboard's modal shows it
	// alongside the upstream-bound body so users can debug translations.
	ClientRequestBody []byte
	RecordMeta        RecordMeta
}

// RecordMeta lets callers override the model/provider stored in
// analytics (used by the OpenAI adapter, which knows the OpenAI-side
// model name even after translation).
type RecordMeta struct {
	Provider string
	Model    string
}

// requestBodyMeta is what we extract from the outbound request body.
type requestBodyMeta struct {
	Provider string
	Model    string
}

// responseBodyMeta is what we extract from the upstream response body.
type responseBodyMeta struct {
	MessageID     string
	StopReason    string
	InputTokens   int64
	OutputTokens  int64
	CacheRead     int64
	CacheCreation int64
}

func extractRequestMeta(body []byte) requestBodyMeta {
	var parsed struct {
		Params struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &parsed)
	return requestBodyMeta{Provider: parsed.Params.Provider, Model: parsed.Params.Model}
}

func extractResponseMeta(body []byte) (responseBodyMeta, map[string]any) {
	if len(body) == 0 {
		return responseBodyMeta{}, nil
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err == nil {
		meta := extractMetaFromObject(generic)
		return meta, generic
	}
	if syn, meta := parseNDJSONResponse(body); syn != nil {
		return meta, syn
	}
	return responseBodyMeta{}, nil
}

func extractMetaFromObject(generic map[string]any) responseBodyMeta {
	meta := responseBodyMeta{}
	if id, ok := generic["id"].(string); ok {
		meta.MessageID = id
	}
	if sr, ok := generic["stop_reason"].(string); ok {
		meta.StopReason = sr
	}
	if usage, ok := generic["usage"].(map[string]any); ok {
		meta.InputTokens = numAsInt(usage["input_tokens"])
		meta.OutputTokens = numAsInt(usage["output_tokens"])
		meta.CacheRead = numAsInt(usage["cache_read_input_tokens"])
		meta.CacheCreation = numAsInt(usage["cache_creation_input_tokens"])
	}
	return meta
}

func parseNDJSONResponse(body []byte) (map[string]any, responseBodyMeta) {
	lines := splitBodyLines(body)
	if len(lines) == 0 {
		return nil, responseBodyMeta{}
	}
	var firstType string
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if t, ok := ev["type"].(string); ok && t != "" {
			firstType = t
			break
		}
	}
	if firstType == "" {
		return nil, responseBodyMeta{}
	}

	var contentParts []CommandCodeContentPart
	var messageID string
	var stopReason string
	var inputTokens, outputTokens int64

	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		evType, _ := ev["type"].(string)
		switch evType {
		case "text-delta":
			if t, ok := ev["text"].(string); ok {
				contentParts = append(contentParts, CommandCodeContentPart{Type: "text", Text: t})
			}
			if id, ok := ev["id"].(string); ok && messageID == "" {
				messageID = id
			}
		case "text-start":
			if id, ok := ev["id"].(string); ok && messageID == "" {
				messageID = id
			}
		case "finish-step":
			if fr, ok := ev["finishReason"].(string); ok {
				stopReason = fr
			}
			if usage, ok := ev["usage"].(map[string]any); ok {
				inputTokens = numAsInt(usage["inputTokens"])
				outputTokens = numAsInt(usage["outputTokens"])
			}
		case "finish":
			if fr, ok := ev["finishReason"].(string); ok && stopReason == "" {
				stopReason = fr
			}
			if tu, ok := ev["totalUsage"].(map[string]any); ok {
				if inputTokens == 0 {
					inputTokens = numAsInt(tu["inputTokens"])
				}
				if outputTokens == 0 {
					outputTokens = numAsInt(tu["outputTokens"])
				}
			}
		}
	}

	if len(contentParts) == 0 {
		return nil, responseBodyMeta{}
	}

	content := make([]any, len(contentParts))
	for i, p := range contentParts {
		content[i] = map[string]any{"type": p.Type, "text": p.Text}
	}

	synthetic := map[string]any{
		"id":          messageID,
		"role":        "assistant",
		"content":     content,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  float64(inputTokens),
			"output_tokens": float64(outputTokens),
		},
	}

	meta := responseBodyMeta{
		MessageID:    messageID,
		StopReason:   stopReason,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}
	return synthetic, meta
}

type CommandCodeContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func splitBodyLines(body []byte) [][]byte {
	var lines [][]byte
	last := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' {
			line := bytes.TrimSpace(body[last:i])
			if len(line) > 0 {
				lines = append(lines, line)
			}
			last = i + 1
		}
	}
	if last < len(body) {
		line := bytes.TrimSpace(body[last:])
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

func numAsInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

// BuildHeaders returns the headers that should be sent upstream for an
// inbound request. Auth precedence: client header > config default token.
func (p *Proxy) BuildHeaders(in http.Header) http.Header {
	h := http.Header{}

	auth := in.Get("Authorization")
	if auth == "" && p.cfg.DefaultToken != "" {
		auth = "Bearer " + p.cfg.DefaultToken
	}
	if auth != "" {
		h.Set("Authorization", auth)
	}

	v := in.Get("X-Command-Code-Version")
	if v == "" {
		v = p.cfg.DefaultVersion
	}
	if v != "" {
		h.Set("X-Command-Code-Version", v)
	}

	ct := in.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	h.Set("Content-Type", ct)

	if accept := in.Get("Accept"); accept != "" {
		h.Set("Accept", accept)
	}
	return h
}

// Forward sends an upstream request, records analytics, and returns the
// raw response (or an error result). It never panics, never returns nil.
func (p *Proxy) Forward(r *http.Request, opt ForwardOptions) Result {
	startedAt := time.Now()

	upstreamPath := opt.UpstreamPath
	if upstreamPath == "" {
		upstreamPath = "/alpha/generate"
	}
	urlStr := strings.TrimRight(p.cfg.UpstreamURL, "/") + upstreamPath

	body := opt.Body
	if body == nil {
		body = []byte("{}")
	}
	reqMeta := extractRequestMeta(body)

	clientBody := opt.ClientRequestBody
	if clientBody == nil {
		clientBody = body
	}

	headers := p.BuildHeaders(r.Header)
	if headers.Get("Authorization") == "" {
		dur := time.Since(startedAt).Milliseconds()
		id, _ := p.analytics.Record(analytics.Entry{
			Method:              r.Method,
			Path:                r.URL.RequestURI(),
			StatusCode:          401,
			DurationMs:          dur,
			Provider:            pickStr(opt.RecordMeta.Provider, reqMeta.Provider),
			Model:               pickStr(opt.RecordMeta.Model, reqMeta.Model),
			IP:                  pickClientIP(r),
			UserAgent:           r.Header.Get("User-Agent"),
			VersionHeader:       r.Header.Get("X-Command-Code-Version"),
			ErrorMessage:        "missing Authorization header and no COMMAND_CODE_TOKEN configured",
			RequestBytes:        int64(len(body)),
			ClientRequestBody:   analytics.CapBody(clientBody),
			UpstreamRequestBody: analytics.CapBody(body),
		})
		return Result{
			StatusCode:   401,
			DurationMs:   dur,
			AuthMissing:  true,
			ErrorMessage: "No Authorization header was provided and the proxy has no COMMAND_CODE_TOKEN configured.",
			EntryID:      id,
		}
	}

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, urlStr, bytes.NewReader(body))
	if err != nil {
		return p.recordFailure(r, opt, reqMeta, body, clientBody, startedAt, fmt.Errorf("build upstream request: %w", err))
	}
	for k, vv := range headers {
		for _, v := range vv {
			upReq.Header.Add(k, v)
		}
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		return p.recordFailure(r, opt, reqMeta, body, clientBody, startedAt, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	dur := time.Since(startedAt).Milliseconds()
	respMeta, parsed := extractResponseMeta(respBody)

	id, _ := p.analytics.Record(analytics.Entry{
		Method:               r.Method,
		Path:                 r.URL.RequestURI(),
		StatusCode:           resp.StatusCode,
		DurationMs:           dur,
		Provider:             pickStr(opt.RecordMeta.Provider, reqMeta.Provider),
		Model:                pickStr(opt.RecordMeta.Model, reqMeta.Model),
		InputTokens:          respMeta.InputTokens,
		OutputTokens:         respMeta.OutputTokens,
		CacheRead:            respMeta.CacheRead,
		CacheCreation:        respMeta.CacheCreation,
		MessageID:            respMeta.MessageID,
		StopReason:           respMeta.StopReason,
		IP:                   pickClientIP(r),
		UserAgent:            r.Header.Get("User-Agent"),
		VersionHeader:        r.Header.Get("X-Command-Code-Version"),
		RequestBytes:         int64(len(body)),
		ResponseBytes:        int64(len(respBody)),
		ClientRequestBody:    analytics.CapBody(clientBody),
		UpstreamRequestBody:  analytics.CapBody(body),
		UpstreamResponseBody: analytics.CapBody(respBody),
	})

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	return Result{
		StatusCode:    resp.StatusCode,
		DurationMs:    dur,
		Body:          respBody,
		ContentType:   ct,
		ParsedBody:    parsed,
		ResponseBytes: int64(len(respBody)),
		EntryID:       id,
	}
}

func (p *Proxy) recordFailure(r *http.Request, opt ForwardOptions, reqMeta requestBodyMeta, body, clientBody []byte, startedAt time.Time, err error) Result {
	dur := time.Since(startedAt).Milliseconds()
	msg := err.Error()
	id, _ := p.analytics.Record(analytics.Entry{
		Method:              r.Method,
		Path:                r.URL.RequestURI(),
		StatusCode:          502,
		DurationMs:          dur,
		Provider:            pickStr(opt.RecordMeta.Provider, reqMeta.Provider),
		Model:               pickStr(opt.RecordMeta.Model, reqMeta.Model),
		IP:                  pickClientIP(r),
		UserAgent:           r.Header.Get("User-Agent"),
		VersionHeader:       r.Header.Get("X-Command-Code-Version"),
		ErrorMessage:        msg,
		RequestBytes:        int64(len(body)),
		ClientRequestBody:   analytics.CapBody(clientBody),
		UpstreamRequestBody: analytics.CapBody(body),
	})
	return Result{
		StatusCode:   502,
		DurationMs:   dur,
		ErrorMessage: msg,
		EntryID:      id,
	}
}

func pickStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func pickClientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.Index(v, ","); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}

// Analytics returns the underlying analytics handle. Used by handlers
// that need to update body fields after Forward returns.
func (p *Proxy) Analytics() *analytics.DB { return p.analytics }

// Handler returns the http.Handler for the native /alpha/generate path.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": err.Error()})
			return
		}
		defer r.Body.Close()

		result := p.Forward(r, ForwardOptions{Body: body})
		if result.AuthMissing {
			payload := map[string]string{"error": "unauthorized", "message": result.ErrorMessage}
			if result.EntryID > 0 {
				if buf, mErr := json.Marshal(payload); mErr == nil {
					_ = p.analytics.UpdateClientResponse(result.EntryID, analytics.CapBody(buf))
				}
			}
			writeJSON(w, http.StatusUnauthorized, payload)
			return
		}
		if result.ErrorMessage != "" {
			payload := map[string]string{"error": "upstream_unreachable", "message": result.ErrorMessage}
			if result.EntryID > 0 {
				if buf, mErr := json.Marshal(payload); mErr == nil {
					_ = p.analytics.UpdateClientResponse(result.EntryID, analytics.CapBody(buf))
				}
			}
			writeJSON(w, http.StatusBadGateway, payload)
			return
		}
		// Native passthrough: the client response is the upstream response verbatim.
		if result.EntryID > 0 {
			_ = p.analytics.UpdateClientResponse(result.EntryID, analytics.CapBody(result.Body))
		}
		w.Header().Set("Content-Type", result.ContentType)
		w.Header().Set("X-Proxy-Duration-Ms", strconv.FormatInt(result.DurationMs, 10))
		w.WriteHeader(result.StatusCode)
		_, _ = w.Write(result.Body)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
