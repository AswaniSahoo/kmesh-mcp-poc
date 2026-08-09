package mcpserver_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/fixture"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/mcpserver"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/tracectx"
)

const traceparentIn = "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01"

// tracedStack is a fixture daemon that records the trace headers it receives,
// with an MCP server in front of it.
type tracedStack struct {
	MCP *httptest.Server
	Rec *fixture.Recorder
}

func newTracedStack(t *testing.T) *tracedStack {
	t.Helper()
	handler, rec := fixture.NewRecording(kmeshapi.ModeDualEngine)
	daemon := httptest.NewServer(handler)
	t.Cleanup(daemon.Close)

	client := kmeshapi.NewClient(strings.TrimPrefix(daemon.URL, "http://"))
	srv, err := mcpserver.New(mcpserver.Config{
		Client:       client,
		ResolvedMode: kmeshapi.ModeDualEngine,
	})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	mcpSrv := httptest.NewServer(mcpserver.NewHandler(srv,
		mcpserver.HandlerOptions{Stateless: true, JSONResponse: true}))
	t.Cleanup(mcpSrv.Close)

	return &tracedStack{MCP: mcpSrv, Rec: rec}
}

// callTool issues a tools/call, merging extraMeta into params._meta.
func (s *tracedStack) callTool(t *testing.T, tool string, extraMeta map[string]any) *http.Response {
	t.Helper()
	meta := map[string]any{
		"io.modelcontextprotocol/protocolVersion":    mcpserver.ProtocolRevision,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	for k, v := range extraMeta {
		meta[k] = v
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{}, "_meta": meta},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, s.MCP.URL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", mcpserver.ProtocolRevision)
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", tool)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("tools/call returned %d: %s", resp.StatusCode, raw)
	}
	return resp
}

// TestTraceContextReachesTheDaemon is the whole point of the tracectx package.
//
// An agent's trace context arrives in the MCP request's _meta (SEP-414). It has
// to continue as a W3C traceparent header on the hop into the kmesh daemon,
// otherwise the agent's view of the call and the mesh's view of the same call
// are two unrelated traces.
func TestTraceContextReachesTheDaemon(t *testing.T) {
	s := newTracedStack(t)
	s.callTool(t, "kmesh_config_dump", map[string]any{
		tracectx.KeyTraceparent: traceparentIn,
		tracectx.KeyTracestate:  "kmesh=abc",
		tracectx.KeyBaggage:     "userId=42",
	})

	got := s.Rec.Last()
	raw := got[tracectx.HeaderTraceparent]
	if raw == "" {
		t.Fatalf("daemon received no traceparent header; trace context was dropped. headers=%v", got)
	}
	t.Logf("in  (MCP _meta):     %s", traceparentIn)
	t.Logf("out (daemon header): %s", raw)

	out, err := tracectx.ParseTraceparent(raw)
	if err != nil {
		t.Fatalf("daemon received an invalid traceparent %q: %v", raw, err)
	}
	in, err := tracectx.ParseTraceparent(traceparentIn)
	if err != nil {
		t.Fatal(err)
	}

	if out.TraceID != in.TraceID {
		t.Errorf("trace id was not preserved: %s -> %s", in.TraceID, out.TraceID)
	}
	if out.SpanID == in.SpanID {
		t.Error("span id was reused; the daemon call should be a child span, not the same span")
	}
	if out.Flags != in.Flags {
		t.Errorf("sampling decision changed: %s -> %s", in.Flags, out.Flags)
	}
	if got[tracectx.HeaderTracestate] != "kmesh=abc" {
		t.Errorf("tracestate = %q, want kmesh=abc", got[tracectx.HeaderTracestate])
	}
	if got[tracectx.HeaderBaggage] != "userId=42" {
		t.Errorf("baggage = %q, want userId=42", got[tracectx.HeaderBaggage])
	}
}

// TestTraceContextOnEveryTool checks propagation is a property of the server
// rather than of one handler.
func TestTraceContextOnEveryTool(t *testing.T) {
	for _, tool := range []string{"kmesh_version", "kmesh_config_dump", "kmesh_get_loggers"} {
		t.Run(tool, func(t *testing.T) {
			s := newTracedStack(t)
			s.callTool(t, tool, map[string]any{tracectx.KeyTraceparent: traceparentIn})

			raw := s.Rec.Last()[tracectx.HeaderTraceparent]
			if raw == "" {
				t.Fatalf("%s dropped the trace context", tool)
			}
			out, err := tracectx.ParseTraceparent(raw)
			if err != nil {
				t.Fatalf("%s emitted an invalid traceparent %q: %v", tool, raw, err)
			}
			if out.TraceID != "0af7651916cd43dd8448eb211c80319c" {
				t.Errorf("%s changed the trace id to %s", tool, out.TraceID)
			}
		})
	}
}

// TestUntracedRequestSendsNoHeader checks we do not invent trace context for
// clients that never asked for it.
func TestUntracedRequestSendsNoHeader(t *testing.T) {
	s := newTracedStack(t)
	s.callTool(t, "kmesh_version", nil)

	if got := s.Rec.Last(); len(got) != 0 {
		t.Fatalf("an untraced request produced trace headers: %v", got)
	}
}

// TestMalformedTraceparentDoesNotFailTheCall pins the W3C behaviour: a
// receiver that cannot parse traceparent restarts the trace rather than
// propagating something invalid. The tool still works; the daemon hop goes out
// untraced.
func TestMalformedTraceparentDoesNotFailTheCall(t *testing.T) {
	s := newTracedStack(t)
	s.callTool(t, "kmesh_version", map[string]any{
		tracectx.KeyTraceparent: "00-not-a-valid-trace-id-01",
	})

	if got := s.Rec.Last()[tracectx.HeaderTraceparent]; got != "" {
		t.Fatalf("an invalid traceparent was propagated onward as %q", got)
	}
}
