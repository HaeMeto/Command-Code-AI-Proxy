package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/analytics"
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/proxy"
)

// Handler returns the http.Handler that implements POST /v1/chat/completions.
func Handler(p *proxy.Proxy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		defer r.Body.Close()

		var req ChatRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		ccBody, err := ToCommandCode(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		body, err := json.Marshal(ccBody)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}

		result := p.Forward(r, proxy.ForwardOptions{
			UpstreamPath:      "/alpha/generate",
			Body:              body,
			ClientRequestBody: raw,
			RecordMeta:        proxy.RecordMeta{Provider: ccBody.Params.Provider, Model: req.Model},
		})

		db := p.Analytics()
		recordClientResponse := func(b []byte) {
			if db != nil && result.EntryID > 0 {
				_ = db.UpdateClientResponse(result.EntryID, analytics.CapBody(b))
			}
		}

		if result.AuthMissing {
			writeErrorAndCapture(w, http.StatusUnauthorized, "authentication_error", result.ErrorMessage, recordClientResponse)
			return
		}
		if result.ErrorMessage != "" {
			writeErrorAndCapture(w, http.StatusBadGateway, "upstream_error", result.ErrorMessage, recordClientResponse)
			return
		}
		if result.StatusCode >= 400 {
			msg := "upstream returned " + strconv.Itoa(result.StatusCode)
			if result.ParsedBody != nil {
				if e, ok := result.ParsedBody["error"].(string); ok && e != "" {
					msg = e
				}
			}
			writeErrorAndCapture(w, result.StatusCode, "upstream_error", msg, recordClientResponse)
			return
		}

		if !req.Stream {
			oa := ToOpenAI(result.ParsedBody, req.Model)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Proxy-Duration-Ms", strconv.FormatInt(result.DurationMs, 10))
			w.WriteHeader(http.StatusOK)
			var buf bytes.Buffer
			mw := io.MultiWriter(w, &buf)
			_ = json.NewEncoder(mw).Encode(oa)
			recordClientResponse(buf.Bytes())
			return
		}

		// Streaming: emit role chunk -> content chunk -> finish chunk -> [DONE].
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Proxy-Duration-Ms", strconv.FormatInt(result.DurationMs, 10))
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		chunks := BuildStreamChunks(result.ParsedBody, req.Model)
		var capture bytes.Buffer
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n\n"))
			capture.WriteString("data: ")
			capture.Write(b)
			capture.WriteString("\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		capture.WriteString("data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		recordClientResponse(capture.Bytes())
	})
}

// ModelsHandler returns the http.Handler for GET /v1/models.
//
// The advertised set is the union of:
//   - envIDs: the static OPENAI_MODELS list (canonical, listed first)
//   - extras(): a dynamic source — typically models discovered from
//     analytics history, so any model the proxy has actually been used
//     with shows up in the advertised list even if it wasn't pre-added
//     to OPENAI_MODELS. Pass nil if no dynamic source is wanted.
//   - upstream: fetched from the CommandCode /provider/v1/models endpoint,
//     cached for 5 minutes to avoid hammering upstream.
//
// Note: /v1/chat/completions never gates on this list — any model name
// the client sends is forwarded verbatim to upstream as `params.model`.
// /v1/models is purely advertising.
func ModelsHandler(envIDs []string, extras func() []string, upstreamURL, defaultToken string) http.Handler {
	cache := &modelsCache{
		upstreamURL: upstreamURL,
		token:       defaultToken,
		ttl:         5 * time.Minute,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ids := envIDs
		if extras != nil {
			ids = MergeModelLists(ids, extras())
		}
		if upstream := cache.get(); len(upstream) > 0 {
			ids = MergeModelLists(ids, upstream)
		}
		out := BuildModelsResponse(ids)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

type modelsCache struct {
	mu          sync.Mutex
	upstreamURL string
	token       string
	ttl         time.Duration
	models      []string
	expiresAt   time.Time
}

type upstreamModel struct {
	ID string `json:"id"`
}

type upstreamModelsResponse struct {
	Data []upstreamModel `json:"data"`
}

func (c *modelsCache) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.models != nil && time.Now().Before(c.expiresAt) {
		return c.models
	}
	c.models = c.fetch()
	c.expiresAt = time.Now().Add(c.ttl)
	return c.models
}

func (c *modelsCache) fetch() []string {
	url := c.upstreamURL + "/provider/v1/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed upstreamModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"param":   nil,
			"code":    nil,
		},
	})
}

// writeErrorAndCapture writes an error response and also passes the
// rendered body to capture so the analytics row's client_response_body
// is filled in (otherwise debugging a 4xx via the dashboard modal would
// show an empty client response panel).
func writeErrorAndCapture(w http.ResponseWriter, status int, kind, message string, capture func([]byte)) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"param":   nil,
			"code":    nil,
		},
	}
	var buf bytes.Buffer
	mw := io.MultiWriter(w, &buf)
	_ = json.NewEncoder(mw).Encode(payload)
	if capture != nil {
		capture(buf.Bytes())
	}
}
