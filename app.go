package main

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// app holds the live configuration. Saving settings through POST /config
// calls reload, which swaps the library clients without a restart.
type app struct {
	log   *slog.Logger
	store *store

	env    Settings
	envSet map[string]bool

	mcpToken    string // MCP_AUTH_TOKEN: required on /mcp unless MCP_ALLOW_NO_AUTH=1
	configToken string // CONFIG_TOKEN (default MCP_AUTH_TOKEN): required on /config and /healthz?check=1

	mu       sync.RWMutex
	settings Settings
	portal   *portal
	bibsok   *bibsok

	server     *mcp.Server
	toolMu     sync.Mutex
	writeTools []string
	addWrite   func()
	writeOn    bool
}

var errNotConfigured = errors.New("the library account is not set up yet: set BIBLIOFIL_URL, BIBLIOFIL_USER and BIBLIOFIL_PIN (environment or the gateway's settings page)")

func newApp(log *slog.Logger, st *store) *app {
	env, set := fromEnv()
	a := &app{
		log:      log,
		store:    st,
		env:      env,
		envSet:   set,
		mcpToken: strings.TrimSpace(os.Getenv("MCP_AUTH_TOKEN")),
	}
	a.configToken = strings.TrimSpace(os.Getenv("CONFIG_TOKEN"))
	if a.configToken == "" {
		a.configToken = a.mcpToken
	}
	a.server = newServer(a)
	a.reload()
	return a
}

// reload rebuilds the clients from stored settings plus env.
func (a *app) reload() {
	var stored Settings
	a.store.read(func(s *state) { stored = s.Settings })
	s := merge(stored, a.env, a.envSet)

	a.mu.Lock()
	old := a.settings
	if s.LibraryURL != old.LibraryURL || s.User != old.User || s.PIN != old.PIN || s.BibsokURL != old.BibsokURL || a.portal == nil {
		a.portal, a.bibsok = nil, nil
		if s.libraryReady() {
			a.portal = newPortal(s.LibraryURL, s.User, s.PIN, s.PortalPickup)
			a.bibsok = newBibsok(s.BibsokURL, s.User, s.PIN, s.BibsokPickup)
		}
	} else {
		a.portal.setPickup(s.PortalPickup)
		a.bibsok.setPickup(s.BibsokPickup)
	}
	a.settings = s
	a.mu.Unlock()

	// Write tools are removed in read-only mode.
	a.toolMu.Lock()
	defer a.toolMu.Unlock()
	if s.ReadOnly && a.writeOn {
		a.server.RemoveTools(a.writeTools...)
		a.writeOn = false
	} else if !s.ReadOnly && !a.writeOn {
		a.addWrite()
		a.writeOn = true
	}
}

func (a *app) current() Settings {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.settings
}

func (a *app) clients() (*portal, *bibsok, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.portal == nil {
		return nil, nil, errNotConfigured
	}
	return a.portal, a.bibsok, nil
}
