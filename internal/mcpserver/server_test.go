package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/fixture"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/mcpserver"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/tokenauth"
)

// stack is a fixture daemon plus an MCP server in front of it.
type stack struct {
	Daemon   *httptest.Server
	MCP      *httptest.Server
	Resolved kmeshapi.Mode
}

// newStack starts a fixture daemon impersonating mode, resolves the mode the
// way the real binary does, and serves MCP over Streamable HTTP in front of it.
func newStack(t *testing.T, mode kmeshapi.Mode) *stack {
	t.Helper()
	return newStackWithOptions(t, mode, mcpserver.HandlerOptions{Stateless: true, JSONResponse: true}, "")
}

func newStackWithOptions(t *testing.T, mode kmeshapi.Mode, opts mcpserver.HandlerOptions, token string) *stack {
	t.Helper()
	daemon := fixture.NewTestServer(mode)
	t.Cleanup(daemon.Close)

	client := kmeshapi.NewClient(strings.TrimPrefix(daemon.URL, "http://"))
	resolved, err := client.ResolveMode(context.Background())
	if err != nil {
		t.Fatalf("ResolveMode: %v", err)
	}

	srv, err := mcpserver.New(mcpserver.Config{Client: client, ResolvedMode: resolved})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}

	var h http.Handler = mcpserver.NewHandler(srv, opts)
	if token != "" {
		h = mcpserver.WithBearerAuth(h, tokenauth.StubVerifier(token))
	}
	mcpSrv := httptest.NewServer(h)
	t.Cleanup(mcpSrv.Close)

	return &stack{Daemon: daemon, MCP: mcpSrv, Resolved: resolved}
}

func (s *stack) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "kmesh-mcp-test", Version: "v0.1.0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             s.MCP.URL,
			DisableStandaloneSSE: true,
		}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// decode pulls a tool's structured output into v.
func decode(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool returned an error result: %s", textOf(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal structured content %s: %v", raw, err)
	}
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// --- mode resolution -------------------------------------------------------

// TestResolveModeReadsDaemon checks that the startup probe reads the mode off
// the daemon rather than guessing. kmesh answers 400 on the config dump route
// for the mode it is not in (checkAdsMode / checkWorkloadMode), so exactly one
// route answers 200.
func TestResolveModeReadsDaemon(t *testing.T) {
	for _, want := range kmeshapi.Modes {
		t.Run(string(want), func(t *testing.T) {
			daemon := fixture.NewTestServer(want)
			defer daemon.Close()
			got, err := kmeshapi.NewClient(strings.TrimPrefix(daemon.URL, "http://")).
				ResolveMode(context.Background())
			if err != nil {
				t.Fatalf("ResolveMode: %v", err)
			}
			if got != want {
				t.Fatalf("resolved %q, want %q", got, want)
			}
		})
	}
}

// TestResolveModeFailsWhenDaemonHasNeitherMode is the negative case: a daemon
// with no xDS controller at all answers 400 on both routes.
func TestResolveModeFailsWhenDaemonHasNeitherMode(t *testing.T) {
	daemon := httptest.NewServer(fixture.New(kmeshapi.Mode("none")))
	defer daemon.Close()
	_, err := kmeshapi.NewClient(strings.TrimPrefix(daemon.URL, "http://")).
		ResolveMode(context.Background())
	if err == nil {
		t.Fatal("expected an error when neither mode answers 200")
	}
	if !strings.Contains(err.Error(), "neither dataplane mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- tools/list ------------------------------------------------------------

// TestToolsListIsDeterministic covers the revision's guidance that list results
// SHOULD be deterministically ordered, so clients and prompt caches can hit.
func TestToolsListIsDeterministic(t *testing.T) {
	cs := newStack(t, kmeshapi.ModeDualEngine).connect(t)
	ctx := context.Background()

	var runs [][]string
	for range 3 {
		res, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		names := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		runs = append(runs, names)
	}

	want := []string{"kmesh_config_dump", "kmesh_get_loggers", "kmesh_version"}
	for i, got := range runs {
		if len(got) != len(want) {
			t.Fatalf("run %d: got %d tools %v, want %d", i, len(got), got, len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("run %d: tool order %v, want %v", i, got, want)
			}
		}
	}
}

// TestConfigDumpToolAdvertisesEnum checks that the hand-adjusted schema really
// reaches the client, so the enum is machine-readable and not only prose in
// the description.
func TestConfigDumpToolAdvertisesEnum(t *testing.T) {
	cs := newStack(t, kmeshapi.ModeDualEngine).connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema []byte
	for _, tool := range res.Tools {
		if tool.Name == "kmesh_config_dump" {
			schema, _ = json.Marshal(tool.InputSchema)
		}
	}
	if schema == nil {
		t.Fatal("kmesh_config_dump not present in tools/list")
	}
	for _, want := range []string{`"enum"`, `"kernel-native"`, `"dual-engine"`} {
		if !strings.Contains(string(schema), want) {
			t.Fatalf("input schema %s does not contain %s", schema, want)
		}
	}
}

// --- kmesh_version ---------------------------------------------------------

func TestVersionTool(t *testing.T) {
	cs := newStack(t, kmeshapi.ModeDualEngine).connect(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "kmesh_version"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out mcpserver.VersionResult
	decode(t, res, &out)
	if out.Daemon.GitVersion != fixture.Version.GitVersion {
		t.Fatalf("gitVersion %q, want %q", out.Daemon.GitVersion, fixture.Version.GitVersion)
	}
	if out.Daemon.GitCommit != fixture.Version.GitCommit {
		t.Fatalf("gitCommit %q, want %q", out.Daemon.GitCommit, fixture.Version.GitCommit)
	}
}

// --- kmesh_config_dump -----------------------------------------------------

// TestConfigDumpOmittedModeUsesStartupProbe is the centrepiece. The tool is
// called with no arguments at all and still returns the right dataplane's data,
// because the server resolved the mode from the daemon at startup. No protocol
// session, no server-minted handle, no extra round trip.
func TestConfigDumpOmittedModeUsesStartupProbe(t *testing.T) {
	s := newStack(t, kmeshapi.ModeDualEngine)
	cs := s.connect(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "kmesh_config_dump"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out mcpserver.ConfigDumpResult
	decode(t, res, &out)

	if out.Mode != string(kmeshapi.ModeDualEngine) {
		t.Fatalf("mode %q, want %q", out.Mode, kmeshapi.ModeDualEngine)
	}
	if out.ModeSource != mcpserver.ModeSourceStartup {
		t.Fatalf("modeSource %q, want %q", out.ModeSource, mcpserver.ModeSourceStartup)
	}
	if _, ok := out.Dump["workloads"]; !ok {
		t.Fatalf("dual-engine dump has no workloads key: %v", out.Dump)
	}
}

// TestConfigDumpExplicitModeOverrides shows the argument still wins when given.
func TestConfigDumpExplicitModeOverrides(t *testing.T) {
	s := newStack(t, kmeshapi.ModeKernelNative)
	cs := s.connect(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "kmesh_config_dump",
		Arguments: map[string]any{"mode": string(kmeshapi.ModeKernelNative)},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out mcpserver.ConfigDumpResult
	decode(t, res, &out)

	if out.ModeSource != mcpserver.ModeSourceArgument {
		t.Fatalf("modeSource %q, want %q", out.ModeSource, mcpserver.ModeSourceArgument)
	}
	if _, ok := out.Dump["dynamicResources"]; !ok {
		t.Fatalf("kernel-native dump has no dynamicResources key: %v", out.Dump)
	}
}

// TestConfigDumpRejectsUnknownMode settles empirically whether the SDK enforces
// the enum keyword before the handler runs, which the design note recorded as
// unverified. The handler never checks the value itself, so if this rejects,
// the enforcement came from schema validation.
func TestConfigDumpRejectsUnknownMode(t *testing.T) {
	cs := newStack(t, kmeshapi.ModeDualEngine).connect(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "kmesh_config_dump",
		Arguments: map[string]any{"mode": "sidecar"},
	})
	if err != nil {
		t.Logf("rejected as a PROTOCOL error: %v", err)
		return
	}
	if !res.IsError {
		t.Fatalf("mode %q was accepted; schema enum is not enforced", "sidecar")
	}
	t.Logf("rejected as a TOOL error: %s", textOf(res))
}

// TestConfigDumpWrongModeIsToolError checks that asking a dual-engine daemon
// for a kernel-native dump comes back as a readable tool error rather than a
// protocol fault, so a model can correct itself.
func TestConfigDumpWrongModeIsToolError(t *testing.T) {
	cs := newStack(t, kmeshapi.ModeDualEngine).connect(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "kmesh_config_dump",
		Arguments: map[string]any{"mode": string(kmeshapi.ModeKernelNative)},
	})
	if err != nil {
		t.Fatalf("expected a tool error, got a protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError on a mode mismatch")
	}
	if !strings.Contains(textOf(res), "dual-engine") {
		t.Fatalf("error text should name the resolved mode, got: %s", textOf(res))
	}
}

// --- kmesh_get_loggers -----------------------------------------------------

func TestGetLoggersListsNames(t *testing.T) {
	cs := newStack(t, kmeshapi.ModeDualEngine).connect(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "kmesh_get_loggers"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out mcpserver.LoggersResult
	decode(t, res, &out)
	if len(out.Names) != len(fixture.LoggerNames) {
		t.Fatalf("got %d logger names %v, want %d", len(out.Names), out.Names, len(fixture.LoggerNames))
	}
	if out.Logger != nil {
		t.Fatalf("logger should be absent when listing names, got %+v", out.Logger)
	}
}

func TestGetLoggersReturnsOneLevel(t *testing.T) {
	cs := newStack(t, kmeshapi.ModeDualEngine).connect(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "kmesh_get_loggers",
		Arguments: map[string]any{"name": "ads"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var out mcpserver.LoggersResult
	decode(t, res, &out)
	if out.Logger == nil {
		t.Fatal("expected a single logger")
	}
	if out.Logger.Name != "ads" || out.Logger.Level != fixture.LoggerLevels["ads"] {
		t.Fatalf("got %+v, want ads/%s", out.Logger, fixture.LoggerLevels["ads"])
	}
	if len(out.Names) != 0 {
		t.Fatalf("names should be absent when selecting one logger, got %v", out.Names)
	}
}

// --- transport and auth ----------------------------------------------------

// TestStatelessRejectsGET pins the observable consequence of Stateless: true.
// A session-based server would open an SSE stream on GET; a stateless one has
// nothing to stream, so the SDK answers 405.
func TestStatelessRejectsGET(t *testing.T) {
	s := newStack(t, kmeshapi.ModeDualEngine)
	req, err := http.NewRequest(http.MethodGet, s.MCP.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET returned %d, want 405", resp.StatusCode)
	}
}

// TestBearerAuthRejectsBadToken proves the auth middleware is actually in the
// request path. The verifier behind it is a stub, not a real TokenReview.
func TestBearerAuthRejectsBadToken(t *testing.T) {
	const good = "fixture-token"
	s := newStackWithOptions(t, kmeshapi.ModeDualEngine,
		mcpserver.HandlerOptions{Stateless: true, JSONResponse: true}, good)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	call := func(token string) int {
		req, err := http.NewRequest(http.MethodPost, s.MCP.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := call(""); code != http.StatusUnauthorized {
		t.Fatalf("no token returned %d, want 401", code)
	}
	if code := call("wrong-token"); code != http.StatusUnauthorized {
		t.Fatalf("bad token returned %d, want 401", code)
	}
	if code := call(good); code != http.StatusOK {
		t.Fatalf("good token returned %d, want 200", code)
	}
}
