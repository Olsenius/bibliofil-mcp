package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// Exercise the production handler with modern discovery and the legacy
// handshake. No library account or external requests are needed to list tools.
func TestHTTPDiscoveryAndTools(t *testing.T) {
	t.Setenv("MCP_AUTH_TOKEN", "test-token")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := newApp(log, openStore(t.TempDir(), log))
	srv := httptest.NewServer(a.mcpHandler())
	defer srv.Close()
	call := func(method, version, params string) json.RawMessage {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
		req, err := http.NewRequest("POST", srv.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", version)
		req.Header.Set("Mcp-Method", method)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: HTTP %d: %s", method, resp.StatusCode, data)
		}
		if resp.Header.Get("Mcp-Session-Id") != "" {
			t.Fatal("unexpected HTTP session")
		}
		if i := bytes.Index(data, []byte("data: ")); i >= 0 {
			data = bytes.SplitN(data[i+6:], []byte("\n"), 2)[0]
		}
		var rpc struct {
			Result json.RawMessage
			Error  json.RawMessage
		}
		if err := json.Unmarshal(data, &rpc); err != nil {
			t.Fatalf("%s: %v: %s", method, err, data)
		}
		if len(rpc.Error) > 0 || len(rpc.Result) == 0 {
			t.Fatalf("%s: %s", method, data)
		}
		return rpc.Result
	}
	modernMeta := `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"test-client","version":"1.0"},"io.modelcontextprotocol/clientCapabilities":{}}}`
	var discovery struct {
		SupportedVersions []string
		ResultType        string
	}
	if err := json.Unmarshal(call("server/discover", "2026-07-28", modernMeta), &discovery); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(discovery.SupportedVersions, "2026-07-28") || discovery.ResultType != "complete" {
		t.Fatalf("unexpected discovery: %+v", discovery)
	}
	for _, version := range []string{"2026-07-28", "2025-11-25"} {
		params := modernMeta
		if version == "2025-11-25" {
			call("initialize", version, `{"protocolVersion":"2025-11-25","clientInfo":{"name":"legacy-client","version":"1.0"},"capabilities":{}}`)
			params = `{}`
		}
		var tools struct{ Tools []struct{ Name string } }
		if err := json.Unmarshal(call("tools/list", version, params), &tools); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tool := range tools.Tools {
			if tool.Name == "list_family" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: list_family missing from tools/list", version)
		}
	}
}
