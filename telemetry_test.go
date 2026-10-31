package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func testTracing(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { provider.Shutdown(context.Background()); otel.SetTracerProvider(previous) })
	return exporter
}

func TestHTTPToolAndLibraryTrace(t *testing.T) {
	exporter := testTracing(t)
	library := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Traceparent") != "" || r.Header.Get("Baggage") != "" {
			t.Error("trace context sent to third-party library")
		}
		if r.URL.Query().Get("card") != "private-card" {
			t.Error("library query changed")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("private-pin")) {
			t.Error("library form changed")
		}
		io.WriteString(w, "private-library-response")
	}))
	defer library.Close()
	t.Setenv("MCP_AUTH_TOKEN", "private-auth-token")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := newApp(log, openStore(t.TempDir(), log))
	session := newSession(library.URL, "/login", "/login", nil, "", "")
	type probeIn struct {
		Query string `json:"query"`
	}
	type probeOut struct {
		Text string `json:"text"`
	}
	mcp.AddTool(a.server, &mcp.Tool{Name: "tracing_probe", Description: "Tracing test"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in probeIn) (*mcp.CallToolResult, probeOut, error) {
			if in.Query != "private-request" {
				t.Error("tool arguments changed")
			}
			body, err := session.do(ctx, "POST", "/catalog?card=private-card", url.Values{"pin": {"private-pin"}})
			return nil, probeOut{Text: string(body)}, err
		})
	mux := http.NewServeMux()
	mux.Handle("/mcp", a.mcpHandler())
	a.registerConfigAPI(mux)
	srv := httptest.NewServer(traceHTTP(mux))
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tracing_probe","arguments":{"query":"private-request"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"test-client","version":"1.0"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer private-auth-token")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "tracing_probe")
	const traceID = "0123456789abcdef0123456789abcdef"
	req.Header.Set("Traceparent", "00-"+traceID+"-0123456789abcdef-01")
	req.Header.Set("Baggage", "secret=private-baggage")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !bytes.Contains(data, []byte("private-library-response")) {
		t.Fatalf("tool call failed: HTTP %d %s %v", resp.StatusCode, data, err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("want HTTP, MCP and library spans; got %d: %+v", len(spans), spans)
	}
	ids := map[string]trace.SpanID{}
	for _, span := range spans {
		ids[span.Name] = span.SpanContext.SpanID()
		if span.SpanContext.TraceID().String() != traceID {
			t.Fatal("incoming gateway trace ID lost")
		}
		raw, _ := json.Marshal(span)
		for _, secret := range []string{"private-card", "private-pin", "private-request", "private-library-response", "private-auth-token", "private-baggage"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("secret leaked into trace: %s", secret)
			}
		}
	}
	if !ids["POST /mcp"].IsValid() || !ids["tools/call tracing_probe"].IsValid() || !ids["library POST"].IsValid() {
		t.Fatalf("unexpected span names: %+v", ids)
	}
	for _, span := range spans {
		switch span.Name {
		case "POST /mcp":
			if span.SpanKind != trace.SpanKindServer || span.Parent.SpanID().String() != "0123456789abcdef" {
				t.Fatal("gateway parent lost")
			}
		case "tools/call tracing_probe":
			if span.Parent.SpanID() != ids["POST /mcp"] {
				t.Fatal("tool not a child of incoming HTTP")
			}
		case "library POST":
			if span.SpanKind != trace.SpanKindClient || span.Parent.SpanID() != ids["tools/call tracing_probe"] {
				t.Fatal("library not a child of tool")
			}
		}
	}
	health, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, health.Body)
	health.Body.Close()
	if len(exporter.GetSpans()) != len(spans) {
		t.Fatal("liveness check created trace noise")
	}
}

func TestTraceMCPFailures(t *testing.T) {
	exporter := testTracing(t)
	for _, failure := range []bool{false, true} {
		handler := traceMCP(func(context.Context, string, mcp.Request) (mcp.Result, error) {
			if failure {
				return nil, errors.New("private-error-text")
			}
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "private-tool-error"}}}, nil
		})
		handler(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "probe"}})
	}
	for _, span := range exporter.GetSpans() {
		if span.Status.Code != codes.Error {
			t.Fatal("MCP failure not marked as error")
		}
		raw, _ := json.Marshal(span)
		if bytes.Contains(raw, []byte("private-")) {
			t.Fatal("error contents leaked")
		}
	}
}

func TestOTLPExportAndShutdown(t *testing.T) {
	received := make(chan *collectortrace.ExportTraceServiceRequest, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("incorrect OTLP request")
		}
		body, _ := io.ReadAll(r.Body)
		var data collectortrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &data); err != nil {
			t.Error(err)
		}
		received <- &data
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	for _, key := range []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(key, "")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_SDK_DISABLED", "false")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	t.Setenv("OTEL_SERVICE_NAME", "bibliofil-export-test")
	previous := otel.GetTracerProvider()
	defer otel.SetTracerProvider(previous)
	shutdown, err := initTracing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "export-check")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-received:
		if len(data.ResourceSpans) != 1 {
			t.Fatal("trace missing")
		}
		found := false
		for _, attr := range data.ResourceSpans[0].Resource.Attributes {
			if attr.Key == "service.name" && attr.Value.GetStringValue() == "bibliofil-export-test" {
				found = true
			}
		}
		if !found {
			t.Fatal("configured service.name missing")
		}
	case <-ctx.Done():
		t.Fatal("shutdown did not export the span")
	}
}
