// Command proxy is the commandcode-proxy entry point.
//
// It reads configuration from the environment (or a .env file in the
// current directory if godotenv-style behavior is desired — we keep
// that out of stdlib by simply expecting `set -a; . .env; set +a`
// before launch), opens the SQLite analytics database, and serves
// HTTP traffic on PORT (default 3000).
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/analytics"
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/config"
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/proxy"
	"github.com/maystri21store-pixel/memory-md-generator/commandcode-proxy/internal/server"
)

func main() {
	loadDotEnv(".env")
	cfg := config.Load()

	db, err := analytics.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open analytics db: %v", err)
	}
	defer db.Close()

	p := proxy.New(cfg, db, &http.Client{Timeout: 120 * time.Second})
	handler := server.Build(cfg, db, p)

	addr := ":" + strconv.Itoa(cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	log.Printf("commandcode-proxy listening on http://0.0.0.0%s -> %s", addr, cfg.UpstreamURL)
	log.Printf("dashboard: http://0.0.0.0%s/dashboard", addr)

	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
		close(idle)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
	<-idle
}

// loadDotEnv reads a minimal `.env` file (KEY=VALUE per line, # comments)
// and sets each variable in the process environment if it isn't already
// set. We avoid pulling in a third-party library for this.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := f.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	for _, line := range splitLines(string(buf)) {
		line = trimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := splitOnce(line, '=')
		if !ok {
			continue
		}
		k, v = trimSpace(k), trimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	_ = fmt.Sprint("loaded ", path)
}

func splitLines(s string) []string {
	var out []string
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[last:i])
			last = i + 1
		}
	}
	if last < len(s) {
		out = append(out, s[last:])
	}
	return out
}

func splitOnce(s string, sep byte) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
