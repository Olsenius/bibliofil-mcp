package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// session is a logged-in web session against one Bibliofil site
// (the library's "Min side" or bibsok.no). All requests are serialized,
// because the site keeps state (session cookie, one-time form ids).
type session struct {
	base     string // e.g. https://mitt.<library>.no
	loginURL string // page that sets the first cookies
	postURL  string // where the login form is posted
	form     func(user, pin string) url.Values
	user     string
	pin      string

	mu       sync.Mutex
	client   *http.Client
	loggedIn bool
}

const userAgent = "bibliofil-mcp/0.2 (+https://github.com/Olsenius/bibliofil-mcp)"

func newSession(base, loginURL, postURL string, form func(user, pin string) url.Values, user, pin string) *session {
	jar, _ := cookiejar.New(nil)
	return &session{
		base:     strings.TrimRight(base, "/"),
		loginURL: loginURL,
		postURL:  postURL,
		form:     form,
		user:     user,
		pin:      pin,
		client:   &http.Client{Jar: jar, Timeout: 45 * time.Second},
	}
}

// isLoginPage reports whether a page is the login form, which means the
// session has expired.
func isLoginPage(body []byte) bool {
	return bytes.Contains(body, []byte(`id="logginn"`)) && bytes.Contains(body, []byte(`name="passord"`))
}

func (s *session) abs(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return s.base + path
}

func (s *session) do(ctx context.Context, method, path string, vals url.Values) ([]byte, error) {
	var body io.Reader
	u := s.abs(path)
	if method == http.MethodGet && len(vals) > 0 {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + vals.Encode()
	} else if vals != nil {
		body = strings.NewReader(vals.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: HTTP %d", method, redact(u), resp.StatusCode)
	}
	return data, nil
}

// redact strips the query string, so errors never leak form values.
func redact(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

func (s *session) login(ctx context.Context) error {
	s.loggedIn = false
	if _, err := s.do(ctx, http.MethodGet, s.loginURL, nil); err != nil {
		return fmt.Errorf("login page: %w", err)
	}
	body, err := s.do(ctx, http.MethodPost, s.postURL, s.form(s.user, s.pin))
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if isLoginPage(body) {
		return errors.New("login failed: check card number and PIN")
	}
	s.loggedIn = true
	return nil
}

func (s *session) ensureLogin(ctx context.Context) error {
	if s.loggedIn {
		return nil
	}
	return s.login(ctx)
}

// request runs one request, logging in first if needed and once more if
// the site answers with the login form.
func (s *session) request(ctx context.Context, method, path string, vals url.Values) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLogin(ctx); err != nil {
		return nil, err
	}
	body, err := s.do(ctx, method, path, vals)
	if err != nil {
		return nil, err
	}
	if isLoginPage(body) {
		if err := s.login(ctx); err != nil {
			return nil, err
		}
		body, err = s.do(ctx, method, path, vals)
		if err != nil {
			return nil, err
		}
		if isLoginPage(body) {
			return nil, errors.New("still logged out after a fresh login")
		}
	}
	return body, nil
}

func (s *session) page(ctx context.Context, path string, vals url.Values) (*goquery.Document, error) {
	body, err := s.request(ctx, http.MethodGet, path, vals)
	if err != nil {
		return nil, err
	}
	return goquery.NewDocumentFromReader(bytes.NewReader(body))
}

// jsonCall runs a request that should answer with JSON. A non-JSON answer
// usually means the session expired. With retry set it logs in again and
// retries once; never set retry for actions that must not run twice.
func (s *session) jsonCall(ctx context.Context, method, path string, vals url.Values, retry bool, out any) error {
	attempts := 1
	if retry {
		attempts = 2
	}
	for attempt := 0; attempt < attempts; attempt++ {
		body, err := s.request(ctx, method, path, vals)
		if err != nil {
			return err
		}
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			return json.Unmarshal(trimmed, out)
		}
		s.mu.Lock()
		s.loggedIn = false
		s.mu.Unlock()
	}
	return errors.New("the library answered with something other than JSON")
}
