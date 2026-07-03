// Package server wires HTTP routing, middleware, and embedded assets.
package server

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/analytics"
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/config"
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/openai"
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/proxy"
)

//go:embed dashboard_assets/*
var dashboardFS embed.FS

// Build constructs the http.Handler tree.
func Build(cfg config.Config, db *analytics.DB, p *proxy.Proxy) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", healthz)

	mux.Handle("/alpha/generate", methodOnly(http.MethodPost, p.Handler()))

	mux.Handle("/v1/chat/completions", methodOnly(http.MethodPost, openai.Handler(p)))
	// /v1/models advertises the env list AND any model the proxy has
	// actually been used with (auto-discovered from analytics history).
	// /v1/chat/completions itself accepts any model verbatim — see
	// ModelsHandler doc.
	mux.Handle("/v1/models", methodOnly(http.MethodGet, openai.ModelsHandler(
		cfg.OpenAIModels,
		func() []string {
			if db == nil {
				return nil
			}
			ids, _ := db.DistinctModels(50)
			return ids
		},
		cfg.UpstreamURL,
		cfg.DefaultToken,
	)))

	mux.Handle("/analytics", admin(cfg, methodOnly(http.MethodGet, analyticsHandler(db))))
	mux.Handle("/analytics/recent", admin(cfg, methodOnly(http.MethodGet, analyticsRecentHandler(db))))
	mux.Handle("/analytics/detail/", admin(cfg, methodOnly(http.MethodGet, analyticsDetailHandler(db))))
	mux.Handle("/dashboard", admin(cfg, methodOnly(http.MethodGet, dashboardHandler())))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			writeJSON(w, 200, map[string]any{
				"name":     "commandcode-proxy",
				"version":  "0.2.0",
				"upstream": cfg.UpstreamURL,
				"endpoints": []string{
					"POST /alpha/generate",
					"POST /v1/chat/completions",
					"GET  /v1/models",
					"GET  /analytics",
					"GET  /analytics/recent",
					"GET  /analytics/detail/{id}",
					"GET  /dashboard",
					"GET  /healthz",
				},
			})
			return
		}
		http.NotFound(w, r)
	})

	return cors(cfg, requestID(mux))
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true})
}

func analyticsHandler(db *analytics.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s, err := db.Summary()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, s)
	})
}

func analyticsRecentHandler(db *analytics.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		items, err := db.Recent(limit)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"items": items, "count": len(items)})
	})
}

// analyticsDetailHandler serves GET /analytics/detail/{id} and returns
// the full row including captured request/response bodies, used by the
// dashboard's request-detail modal.
func analyticsDetailHandler(db *analytics.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/analytics/detail/")
		if rest == "" || strings.Contains(rest, "/") {
			writeJSON(w, 400, map[string]string{"error": "bad_request", "message": "path must be /analytics/detail/{id}"})
			return
		}
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || id <= 0 {
			writeJSON(w, 400, map[string]string{"error": "bad_request", "message": "id must be a positive integer"})
			return
		}
		detail, err := db.GetDetail(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSON(w, 404, map[string]string{"error": "not_found", "message": fmt.Sprintf("no request with id %d", id)})
				return
			}
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, detail)
	})
}

func dashboardHandler() http.Handler {
	sub, err := fs.Sub(dashboardFS, "dashboard_assets")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, fmt.Sprintf("dashboard assets unavailable: %v", err), 500)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := sub.Open("dashboard.html")
		if err != nil {
			http.Error(w, "dashboard unavailable", 500)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, f)
	})
}

// admin gates the wrapped handler with the configured ADMIN_TOKEN. If
// no token is configured, the handler is reachable without auth.
func admin(cfg config.Config, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.AdminToken == "" {
			h.ServeHTTP(w, r)
			return
		}
		token := r.Header.Get("X-Admin-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token != cfg.AdminToken {
			writeJSON(w, 401, map[string]string{"error": "unauthorized", "message": "ADMIN_TOKEN required"})
			return
		}
		h.ServeHTTP(w, r)
	})
}

func cors(cfg config.Config, h http.Handler) http.Handler {
	allow := strings.Join(cfg.CORSOrigins, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if allow == "*" || strings.Contains(allow, origin) {
				w.Header().Set("Access-Control-Allow-Origin", originOrStar(allow, origin))
				w.Header().Set("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Command-Code-Version, X-Admin-Token, Accept")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func originOrStar(allow, origin string) string {
	if allow == "*" {
		return "*"
	}
	return origin
}

func requestID(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Powered-By", "commandcode-proxy/go")
		h.ServeHTTP(w, r)
	})
}

func methodOnly(method string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method && r.Method != http.MethodOptions {
			w.Header().Set("Allow", method)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed", "allow": method})
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
