// bibliofil-mcp is an MCP server for a Norwegian public library running
// Bibliofil and the national search bibsok.no. All configuration comes
// from environment variables.
package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "0.2.0"

type config struct {
	portalURL    string
	bibsokURL    string
	user, pin    string
	portalPickup string
	bibsokPickup string
	token        string
	addr         string
	transport    string
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	c := config{
		portalURL:    env("BIBLIOFIL_URL", ""),
		bibsokURL:    env("BIBSOK_URL", "https://bibsok.no"),
		user:         env("BIBLIOFIL_USER", ""),
		pin:          env("BIBLIOFIL_PIN", ""),
		portalPickup: env("BIBLIOFIL_PICKUP", ""),
		bibsokPickup: env("BIBSOK_PICKUP", ""),
		token:        env("MCP_AUTH_TOKEN", ""),
		addr:         ":" + env("PORT", "8080"),
		transport:    env("MCP_TRANSPORT", "http"),
	}
	if c.portalURL == "" || c.user == "" || c.pin == "" {
		return c, fmt.Errorf("BIBLIOFIL_URL, BIBLIOFIL_USER and BIBLIOFIL_PIN must be set")
	}
	if c.transport == "http" && c.token == "" && env("MCP_ALLOW_NO_AUTH", "") != "1" {
		return c, fmt.Errorf("MCP_AUTH_TOKEN must be set for HTTP (or MCP_ALLOW_NO_AUTH=1 on a trusted network)")
	}
	return c, nil
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "check that the HTTP server answers, for Docker HEALTHCHECK")
	flag.Parse()
	if *healthcheck {
		resp, err := http.Get("http://127.0.0.1:" + env("PORT", "8080") + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := loadConfig()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(2)
	}
	p := newPortal(cfg.portalURL, cfg.user, cfg.pin, cfg.portalPickup)
	b := newBibsok(cfg.bibsokURL, cfg.user, cfg.pin, cfg.bibsokPickup)
	server := newServer(p, b)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.transport == "stdio" {
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
			log.Error("stdio", "err", err)
			os.Exit(1)
		}
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute,
	})
	mux.Handle("/mcp", requireToken(cfg.token, mcpHandler))

	srv := &http.Server{Addr: cfg.addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", cfg.addr, "path", "/mcp", "library", cfg.portalURL, "version", version)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http", "err", err)
		os.Exit(1)
	}
}

// requireToken accepts "Authorization: Bearer <token>". An empty token
// disables the check (only allowed with MCP_ALLOW_NO_AUTH=1).
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bibliofil-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
