package main

// The gateway contract: endpoints a gateway (github.com/Olsenius/mcp-gateway)
// uses to show a settings form and a test button for this server.
//
//   GET  /healthz            liveness, no auth
//   GET  /healthz?check=1    runs real checks with the current settings
//   GET  /config             the settings: schema, values (never secrets), lock state
//   POST /config             {"values": {"ENV_NAME": "value"}, "clear": ["ENV_NAME"]}
//
// All but plain /healthz need "Authorization: Bearer <CONFIG_TOKEN>".

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type fieldOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type configField struct {
	Key      string        `json:"key"`
	Label    string        `json:"label"`
	Type     string        `json:"type"` // text, url, secret, bool, select
	Required bool          `json:"required,omitempty"`
	Help     string        `json:"help,omitempty"`
	Value    string        `json:"value"`
	Set      bool          `json:"set"`
	Locked   bool          `json:"locked"` // set by an environment variable
	Options  []fieldOption `json:"options,omitempty"`
	Default  string        `json:"default,omitempty"`
}

type configDoc struct {
	Name       string        `json:"name"`
	Version    string        `json:"version"`
	Configured bool          `json:"configured"`
	Fields     []configField `json:"fields"`
}

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type healthDoc struct {
	OK     bool          `json:"ok"`
	Status string        `json:"status"`
	Checks []checkResult `json:"checks,omitempty"`
}

// fieldDefs describes each setting. key is the internal name used in
// envNames.
var fieldDefs = []struct {
	key, label, typ, help, def string
	required                   bool
}{
	{"library_url", "Library \"Min side\" URL", "url", "Your library's Bibliofil site, e.g. https://mitt.<library>.no (the one serving /cgi-bin/m2).", "", true},
	{"user", "Library card number or e-mail", "text", "The card to use. Its family folder comes along.", "", true},
	{"pin", "PIN", "secret", "Stored on the server and never shown again.", "", true},
	{"portal_pickup", "Pickup branch for reservations", "select", "Empty = the card's default.", "", false},
	{"bibsok_pickup", "Pickup library for interlibrary loans (Biblioteksøk)", "select", "Empty = the card's home library (often the main library).", "", false},
	{"read_only", "Read-only", "bool", "Hides the tools that reserve, order, renew and cancel.", "", false},
	{"bibsok_url", "Biblioteksøk URL", "url", "Normally no need to change.", "https://bibsok.no", false},
}

func settingValue(s Settings, key string) string {
	switch key {
	case "library_url":
		return s.LibraryURL
	case "user":
		return s.User
	case "pin":
		return s.PIN
	case "portal_pickup":
		return s.PortalPickup
	case "bibsok_pickup":
		return s.BibsokPickup
	case "bibsok_url":
		return s.BibsokURL
	case "read_only":
		if s.ReadOnly {
			return "true"
		}
		return "false"
	}
	return ""
}

func setSetting(s *Settings, key, v string) error {
	v = strings.TrimSpace(v)
	switch key {
	case "library_url", "bibsok_url":
		if v != "" && !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://") {
			return fmt.Errorf("%s must start with https://", envNames[key])
		}
		if key == "library_url" {
			s.LibraryURL = strings.TrimRight(v, "/")
		} else {
			s.BibsokURL = strings.TrimRight(v, "/")
		}
	case "user":
		s.User = v
	case "pin":
		s.PIN = v
	case "portal_pickup":
		s.PortalPickup = v
	case "bibsok_pickup":
		s.BibsokPickup = v
	case "read_only":
		s.ReadOnly = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	return nil
}

func keyForEnv(env string) string {
	for k, e := range envNames {
		if strings.EqualFold(e, env) {
			return k
		}
	}
	return ""
}

// ---------- pickup options cache ----------

type optionsCache struct {
	mu      sync.Mutex
	key     string
	at      time.Time
	local   []fieldOption
	natl    []fieldOption
	lastErr error
}

func (a *app) pickupOptions(ctx context.Context, c *optionsCache) ([]fieldOption, []fieldOption) {
	s := a.current()
	key := s.LibraryURL + "|" + s.User + "|" + s.PIN
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == key && time.Since(c.at) < 15*time.Minute && (c.local != nil || c.natl != nil) {
		return c.local, c.natl
	}
	p, b, err := a.clients()
	if err != nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	var local, natl []fieldOption
	if l, err := p.PickupLocations(ctx); err == nil {
		for _, o := range l {
			label := o.Name
			if o.CardDefault {
				label += " (card default)"
			}
			local = append(local, fieldOption{Value: o.Code, Label: label})
		}
	} else {
		c.lastErr = err
	}
	if l, err := b.PickupLocations(ctx, ""); err == nil {
		for _, o := range l {
			label := o.Name
			if o.CardDefault {
				label += " (card default)"
			}
			natl = append(natl, fieldOption{Value: o.Code, Label: label})
		}
	} else {
		c.lastErr = err
	}
	c.key, c.at, c.local, c.natl = key, time.Now(), local, natl
	return local, natl
}

// ---------- handlers ----------

func (a *app) configDoc(ctx context.Context, c *optionsCache) configDoc {
	s := a.current()
	var stored Settings
	a.store.read(func(st *state) { stored = st.Settings })
	local, natl := a.pickupOptions(ctx, c)
	doc := configDoc{Name: "bibliofil-mcp", Version: version, Configured: s.libraryReady()}
	for _, d := range fieldDefs {
		f := configField{Key: envNames[d.key], Label: d.label, Type: d.typ, Help: d.help, Required: d.required, Default: d.def, Locked: a.envSet[d.key]}
		v := settingValue(s, d.key)
		f.Set = v != ""
		if d.typ == "bool" {
			f.Set = a.envSet[d.key] || stored.ReadOnly
		}
		if d.typ == "secret" {
			f.Set = v != ""
			v = ""
		}
		f.Value = v
		switch d.key {
		case "portal_pickup":
			f.Options = local
		case "bibsok_pickup":
			f.Options = natl
		}
		doc.Fields = append(doc.Fields, f)
	}
	return doc
}

type configUpdate struct {
	Values map[string]string `json:"values"`
	Clear  []string          `json:"clear"`
}

func (a *app) applyConfig(u configUpdate) error {
	// Validate first, then store.
	var stored Settings
	a.store.read(func(st *state) { stored = st.Settings })
	next := stored
	for env, v := range u.Values {
		key := keyForEnv(env)
		if key == "" {
			return fmt.Errorf("unknown setting %q", env)
		}
		if a.envSet[key] {
			return fmt.Errorf("%s is set by an environment variable and cannot be changed here", env)
		}
		if key == "pin" && v == "" {
			continue // empty secret = keep
		}
		if err := setSetting(&next, key, v); err != nil {
			return err
		}
	}
	for _, env := range u.Clear {
		key := keyForEnv(env)
		if key == "" {
			return fmt.Errorf("unknown setting %q", env)
		}
		if a.envSet[key] {
			return fmt.Errorf("%s is set by an environment variable and cannot be changed here", env)
		}
		setSetting(&next, key, "")
	}
	if err := a.store.update(func(st *state) { st.Settings = next }); err != nil {
		return err
	}
	a.reload()
	return nil
}

func (a *app) runChecks(ctx context.Context) healthDoc {
	h := healthDoc{OK: true, Status: "ok"}
	add := func(name string, err error, detail string) {
		c := checkResult{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			c.Detail = err.Error()
			h.OK = false
		}
		h.Checks = append(h.Checks, c)
	}
	p, b, err := a.clients()
	if err != nil {
		add("Settings", err, "")
		h.Status = "not configured"
		return h
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	fam, err := p.Family(ctx)
	detail := ""
	if err == nil {
		detail = fmt.Sprintf("logged in, %d in the family folder", len(fam))
	}
	add("Login to Min side", err, detail)

	if err == nil {
		loans, lerr := p.Loans(ctx, "")
		add("Loans", lerr, fmt.Sprintf("%d loans", len(loans)))
	}
	if pick := p.defaultPickup(); pick != "" {
		opts, perr := p.PickupLocations(ctx)
		found := ""
		for _, o := range opts {
			if strings.EqualFold(o.Code, pick) {
				found = o.Name
			}
		}
		if perr == nil && found == "" {
			perr = fmt.Errorf("pickup branch %q does not exist", pick)
		}
		add("Pickup branch for reservations", perr, found)
	}
	_, hits, serr := b.Search(ctx, "norge", 1)
	if serr == nil && len(hits) == 0 {
		serr = fmt.Errorf("the search returned nothing")
	}
	add("Login and search in Biblioteksøk", serr, "")
	if pick := b.defaultPickup(); pick != "" && serr == nil {
		opts, perr := b.PickupLocations(ctx, pick)
		found := ""
		for _, o := range opts {
			if o.Code == pick {
				found = o.Name
			}
		}
		if perr == nil && found == "" {
			perr = fmt.Errorf("library number %q does not exist", pick)
		}
		add("Pickup library for interlibrary loans", perr, found)
	}
	if !h.OK {
		h.Status = "failing"
	}
	return h
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (a *app) configAuthorized(r *http.Request) bool {
	if a.configToken == "" {
		return false
	}
	return constEq(r.Header.Get("Authorization"), "Bearer "+a.configToken)
}

func (a *app) registerConfigAPI(mux *http.ServeMux) {
	cache := &optionsCache{}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("check") == "" {
			w.Write([]byte("ok"))
			return
		}
		if !a.configAuthorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h := a.runChecks(r.Context())
		code := http.StatusOK
		if !h.OK {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, h)
	})

	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		if a.configToken == "" {
			http.NotFound(w, r)
			return
		}
		if !a.configAuthorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, a.configDoc(r.Context(), cache))
		case http.MethodPost:
			var u configUpdate
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&u); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
				return
			}
			if err := a.applyConfig(u); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			a.log.Info("settings updated via /config")
			writeJSON(w, http.StatusOK, a.configDoc(r.Context(), cache))
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
