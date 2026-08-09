// Command demo runs the whole stack in-process and prints the raw JSON-RPC it
// exchanges, so the README can show wire traffic rather than a description of
// wire traffic.
//
// It starts a fixture kmesh daemon, resolves the dataplane mode from it, serves
// MCP in front of it, and then issues plain HTTP POSTs. Nothing here uses the
// SDK's client, deliberately: the point is to show what actually crosses the
// wire.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/fixture"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/mcpserver"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/tracectx"
)

func main() {
	log.SetFlags(0)

	// A fixture daemon in dual-engine mode. The recorder captures the W3C
	// trace headers the daemon receives, so section 11 can show that trace
	// context really crossed the hop rather than claiming it did.
	daemonHandler, rec := fixture.NewRecording(kmeshapi.ModeDualEngine)
	daemon := httptest.NewServer(daemonHandler)
	defer daemon.Close()

	client := kmeshapi.NewClient(strings.TrimPrefix(daemon.URL, "http://"))
	mode, err := client.ResolveMode(context.Background())
	if err != nil {
		log.Fatalf("resolving mode: %v", err)
	}

	srv, err := mcpserver.New(mcpserver.Config{Client: client, ResolvedMode: mode})
	if err != nil {
		log.Fatalf("building server: %v", err)
	}
	mcpSrv := httptest.NewServer(mcpserver.NewHandler(srv, mcpserver.HandlerOptions{
		Stateless:    true,
		JSONResponse: true,
	}))
	defer mcpSrv.Close()

	section("0. Startup: the server probes the daemon once for its dataplane mode")
	fmt.Printf("fixture kmesh daemon listening, impersonating a %s dataplane\n", mode)
	fmt.Printf("GET %s%s -> 200\n", daemon.URL, kmeshapi.ModeDualEngine.Route())
	fmt.Printf("GET %s%s -> 400 (kmesh answers \"Invalid Client Mode\" for the mode it is not in)\n",
		daemon.URL, kmeshapi.ModeKernelNative.Route())
	fmt.Printf("=> resolved dataplane mode: %q\n", mode)

	section("1. server/discover — mandatory in 2026-07-28; advertises versions, capabilities, identity")
	fmt.Println("// Note params._meta. Under SEP-2575 every request declares its protocol")
	fmt.Println("// version there. Setting only the MCP-Protocol-Version header is rejected")
	fmt.Println("// with -32602 \"missing or invalid _meta field\".")
	exchange(mcpSrv.URL, 1, "server/discover", nil)

	section("2. tools/list — three tools, deterministically ordered, with cache hints")
	exchange(mcpSrv.URL, 2, "tools/list", map[string]any{})

	section("3. tools/call kmesh_version — the no-argument baseline")
	exchange(mcpSrv.URL, 3, "tools/call", map[string]any{
		"name":      "kmesh_version",
		"arguments": map[string]any{},
	})

	section("4. tools/call kmesh_config_dump WITH NO mode ARGUMENT")
	fmt.Println("// The mode argument is omitted entirely. The server answers with dual-engine")
	fmt.Println("// data because it resolved the mode from the daemon at startup, and says so")
	fmt.Println("// in modeSource. No session, no server-minted handle, no extra round trip.")
	exchange(mcpSrv.URL, 4, "tools/call", map[string]any{
		"name":      "kmesh_config_dump",
		"arguments": map[string]any{},
	})

	section("5. tools/call kmesh_get_loggers — optional string that changes the response shape")
	exchange(mcpSrv.URL, 5, "tools/call", map[string]any{
		"name":      "kmesh_get_loggers",
		"arguments": map[string]any{},
	})
	exchange(mcpSrv.URL, 6, "tools/call", map[string]any{
		"name":      "kmesh_get_loggers",
		"arguments": map[string]any{"name": "ads"},
	})

	section("6. Negative: an unknown mode is rejected by the schema, before the handler runs")
	exchange(mcpSrv.URL, 7, "tools/call", map[string]any{
		"name":      "kmesh_config_dump",
		"arguments": map[string]any{"mode": "sidecar"},
	})

	section("7. Negative: a mode the daemon is not running in comes back as a readable tool error")
	exchange(mcpSrv.URL, 8, "tools/call", map[string]any{
		"name":      "kmesh_config_dump",
		"arguments": map[string]any{"mode": "kernel-native"},
	})

	section("8. Version negotiation happens per request, not once per connection")
	fmt.Println("// This is what replaced the initialize handshake. A client declaring an")
	fmt.Println("// older but supported revision is served at that revision — note that")
	fmt.Println("// resultType is absent below, because it is a 2026-07-28 field.")
	exchangeAt(mcpSrv.URL, 9, "tools/call", map[string]any{
		"name": "kmesh_version", "arguments": map[string]any{},
	}, "2025-06-18")

	section("9. Negative: a protocol version the server does not support")
	fmt.Println("// Same request, a revision that does not exist. The error carries the")
	fmt.Println("// versions the server does support, so the client can retry immediately.")
	exchangeAt(mcpSrv.URL, 10, "tools/list", map[string]any{}, "2023-01-01")

	section("10. What Stateless: true costs — GET has no stream to open, so it is 405")
	req, _ := http.NewRequest(http.MethodGet, mcpSrv.URL, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	fmt.Printf("GET /mcp\nAccept: text/event-stream\n\n<- %s\n%s\n",
		resp.Status, strings.TrimSpace(string(body)))

	section("11. Trace context crossing from MCP into the mesh (SEP-414)")
	fmt.Println("// The 2026-07-28 revision reserves traceparent/tracestate/baggage in _meta")
	fmt.Println("// for OpenTelemetry trace context, as an explicit exception to the")
	fmt.Println("// io.modelcontextprotocol/ prefix rule. For a service mesh that is not a")
	fmt.Println("// footnote: unless the trace survives the hop into the daemon, the agent's")
	fmt.Println("// view of a call and the mesh's view of the same call are two unrelated")
	fmt.Println("// traces. Below, the same trace id comes out the other side with a new")
	fmt.Println("// span id, so the daemon call is a child of the tool call.")
	const traceparentIn = "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01"
	exchangeMeta(mcpSrv.URL, 11, "tools/call", map[string]any{
		"name": "kmesh_config_dump", "arguments": map[string]any{},
	}, map[string]any{
		tracectx.KeyTraceparent: traceparentIn,
		tracectx.KeyTracestate:  "kmesh=abc",
		tracectx.KeyBaggage:     "userId=42",
	})
	fmt.Printf("\n// what the kmesh daemon actually received on its own HTTP request:\n")
	seen := rec.Last()
	fmt.Printf("   in  (MCP _meta)      traceparent: %s\n", traceparentIn)
	fmt.Printf("   out (daemon header)  traceparent: %s\n", seen[tracectx.HeaderTraceparent])
	fmt.Printf("   out (daemon header)  tracestate:  %s\n", seen[tracectx.HeaderTracestate])
	fmt.Printf("   out (daemon header)  baggage:     %s\n", seen[tracectx.HeaderBaggage])

	section("12. The same server with Stateless: false, for comparison")
	statefulSrv := httptest.NewServer(mcpserver.NewHandler(srv, mcpserver.HandlerOptions{
		Stateless:    false,
		JSONResponse: true,
	}))
	defer statefulSrv.Close()
	fmt.Println("// Session-based behaviour is one struct field away. Whatever the spec's")
	fmt.Println("// stateless semantics settle into, this is the only line that changes.")
	statusOnly(statefulSrv.URL, http.MethodGet)
}

func section(title string) {
	fmt.Printf("\n%s\n%s\n%s\n", strings.Repeat("=", 78), title, strings.Repeat("=", 78))
}

// MetaProtocolVersion is the _meta key every request must carry under protocol
// revision 2026-07-28 (SEP-2575). The MCP-Protocol-Version HTTP header does not
// substitute for it: the server rejects a request that sets only the header
// with -32602 "missing or invalid _meta field".
const MetaProtocolVersion = "io.modelcontextprotocol/protocolVersion"

// MetaClientCapabilities is the second _meta key every request must carry.
// With the initialize handshake removed there is no other moment at which a
// client could declare what it supports, so it declares it every time.
const MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"

// exchange sends one JSON-RPC request at the current protocol revision.
func exchange(endpoint string, id int, method string, params map[string]any) {
	exchangeFull(endpoint, id, method, params, mcpserver.ProtocolRevision, nil)
}

// exchangeAt sends one JSON-RPC request declaring the given protocol revision.
func exchangeAt(endpoint string, id int, method string, params map[string]any, version string) {
	exchangeFull(endpoint, id, method, params, version, nil)
}

// exchangeMeta sends one request with extra _meta keys merged in, which is how
// trace context travels (SEP-414).
func exchangeMeta(endpoint string, id int, method string, params, extraMeta map[string]any) {
	exchangeFull(endpoint, id, method, params, mcpserver.ProtocolRevision, extraMeta)
}

// exchangeFull sends one JSON-RPC request and prints both sides of the wire.
func exchangeFull(endpoint string, id int, method string, params map[string]any, version string, extraMeta map[string]any) {
	if params == nil {
		params = map[string]any{}
	}
	meta := map[string]any{
		MetaProtocolVersion:    version,
		MetaClientCapabilities: map[string]any{},
	}
	for k, v := range extraMeta {
		meta[k] = v
	}
	params["_meta"] = meta

	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		log.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", version)
	// SEP-2243 requires Mcp-Method on every Streamable HTTP POST, and Mcp-Name
	// where the request names a specific feature. Omitting Mcp-Method is
	// rejected with -32020 before the body is looked at.
	req.Header.Set("Mcp-Method", method)
	headers := fmt.Sprintf("MCP-Protocol-Version: %s   Mcp-Method: %s", version, method)
	if name, ok := params["name"].(string); ok {
		req.Header.Set("Mcp-Name", name)
		headers += "   Mcp-Name: " + name
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	fmt.Printf("\n-> POST /mcp\n   %s\n%s\n", headers, indentJSON(raw))
	fmt.Printf("\n<- %s\n%s\n", resp.Status, indentJSON(body))
}

func statusOnly(endpoint, method string) {
	req, _ := http.NewRequest(method, endpoint, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("%s: %v", method, err)
	}
	_ = resp.Body.Close()
	fmt.Printf("\nGET /mcp (Stateless: false)\n<- %s\n", resp.Status)
}

func indentJSON(b []byte) string {
	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimSpace(b), "", "  "); err != nil {
		return strings.TrimSpace(string(b))
	}
	return out.String()
}
