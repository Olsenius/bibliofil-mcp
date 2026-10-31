// bibliofil-mcp is an MCP server for a Norwegian public library running
// Bibliofil and the national search bibsok.no.
//
// Settings come from environment variables, or from a gateway through
// POST /config (stored in DATA_DIR). Environment variables always win.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "0.3.0"

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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
	transport := env("MCP_TRANSPORT", "http")
	a := newApp(log, openStore(env("DATA_DIR", "/data"), log))
	if !a.current().libraryReady() {
		log.Warn("library account not set up yet; tools will answer with an error until BIBLIOFIL_URL, BIBLIOFIL_USER and BIBLIOFIL_PIN are set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if transport == "stdio" {
		if err := a.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
			log.Error("stdio", "err", err)
			os.Exit(1)
		}
		return
	}

	if a.mcpToken == "" && env("MCP_ALLOW_NO_AUTH", "") != "1" {
		log.Error("MCP_AUTH_TOKEN must be set for HTTP (or MCP_ALLOW_NO_AUTH=1 on a trusted network)")
		os.Exit(2)
	}

	mux := http.NewServeMux()
	a.registerConfigAPI(mux)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return a.server }, &mcp.StreamableHTTPOptions{
		SessionTimeout: 30 * time.Minute,
	})
	mux.Handle("/mcp", requireToken(a.mcpToken, mcpHandler))

	srv := &http.Server{Addr: ":" + env("PORT", "8080"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", srv.Addr, "path", "/mcp", "config_api", a.configToken != "", "version", version)
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !constEq(r.Header.Get("Authorization"), "Bearer "+token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bibliofil-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
