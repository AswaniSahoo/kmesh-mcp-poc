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
)

func main() {
	log.SetFlags(0)

	// A fixture daemon in dual-engine mode.
	daemon := httptest.NewServer(fixture.New(kmeshapi.ModeDualEngine))
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

	section("11. The same server with Stateless: false, for comparison")
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
	exchangeAt(endpoint, id, method, params, mcpserver.ProtocolRevision)
}

// exchangeAt sends one JSON-RPC request declaring the given protocol revision,
// and prints both sides of the wire.
func exchangeAt(endpoint string, id int, method string, params map[string]any, version string) {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		MetaProtocolVersion:    version,
		MetaClientCapabilities: map[string]any{},
	}

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
