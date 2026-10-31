package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Settings are the values that can come from the setup page or from
// environment variables. Environment variables always win.
type Settings struct {
	LibraryURL   string `json:"library_url,omitempty"`
	User         string `json:"user,omitempty"`
	PIN          string `json:"pin,omitempty"`
	PortalPickup string `json:"portal_pickup,omitempty"`
	BibsokPickup string `json:"bibsok_pickup,omitempty"`
	BibsokURL    string `json:"bibsok_url,omitempty"`
	ReadOnly     bool   `json:"read_only,omitempty"`
}

// envNames maps each setting to its environment variable.
var envNames = map[string]string{
	"library_url":   "BIBLIOFIL_URL",
	"user":          "BIBLIOFIL_USER",
	"pin":           "BIBLIOFIL_PIN",
	"portal_pickup": "BIBLIOFIL_PICKUP",
	"bibsok_pickup": "BIBSOK_PICKUP",
	"bibsok_url":    "BIBSOK_URL",
	"read_only":     "MCP_READ_ONLY",
}

// fromEnv returns the settings given as environment variables and which
// of them were set.
func fromEnv() (Settings, map[string]bool) {
	set := map[string]bool{}
	get := func(key string) string {
		v := strings.TrimSpace(os.Getenv(envNames[key]))
		if v != "" {
			set[key] = true
		}
		return v
	}
	s := Settings{
		LibraryURL:   get("library_url"),
		User:         get("user"),
		PIN:          get("pin"),
		PortalPickup: get("portal_pickup"),
		BibsokPickup: get("bibsok_pickup"),
		BibsokURL:    get("bibsok_url"),
	}
	if v := get("read_only"); v != "" {
		s.ReadOnly = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	return s, set
}

// merge lays env on top of stored.
func merge(stored, env Settings, set map[string]bool) Settings {
	out := stored
	pick := func(key string, dst *string, v string) {
		if set[key] {
			*dst = v
		}
	}
	pick("library_url", &out.LibraryURL, env.LibraryURL)
	pick("user", &out.User, env.User)
	pick("pin", &out.PIN, env.PIN)
	pick("portal_pickup", &out.PortalPickup, env.PortalPickup)
	pick("bibsok_pickup", &out.BibsokPickup, env.BibsokPickup)
	pick("bibsok_url", &out.BibsokURL, env.BibsokURL)
	if set["read_only"] {
		out.ReadOnly = env.ReadOnly
	}
	if out.BibsokURL == "" {
		out.BibsokURL = "https://bibsok.no"
	}
	out.LibraryURL = strings.TrimRight(out.LibraryURL, "/")
	return out
}

func (s Settings) libraryReady() bool {
	return s.LibraryURL != "" && s.User != "" && s.PIN != ""
}

// ---------- persisted state ----------

type state struct {
	Settings Settings `json:"settings"`
}

// store keeps the state in DATA_DIR/state.json (mode 0600). Without a
// writable directory everything lives in memory until restart.
type store struct {
	mu   sync.Mutex
	path string
	st   state
	log  *slog.Logger
}

func openStore(dir string, log *slog.Logger) *store {
	s := &store{log: log}
	if dir == "" {
		log.Warn("DATA_DIR is empty, settings are kept in memory only")
		return s
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Warn("data dir not usable, settings are kept in memory only", "dir", dir, "err", err)
		return s
	}
	s.path = filepath.Join(dir, "state.json")
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Warn("could not read state", "err", err)
	default:
		if err := json.Unmarshal(data, &s.st); err != nil {
			log.Warn("state file is corrupt, starting empty", "err", err)
		}
	}
	return s
}

// update changes the state under lock and writes it to disk.
func (s *store) update(fn func(*state)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return os.Rename(tmp, s.path)
}

func (s *store) read(fn func(*state)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
}

func constEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
