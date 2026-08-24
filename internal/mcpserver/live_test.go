//go:build live

// Live tests. These run the same server against a real kmesh-daemon instead of
// the fixture, and are excluded from the default build so `go test ./...` still
// needs no cluster.
//
//	go test -tags live ./internal/mcpserver/ -run TestLive -v
//
// KMESH_DAEMON_ADDR points at a reachable admin API, normally a port-forward of
// the DaemonSet pod's localhost:15200. Without it these tests skip rather than
// fail, so the tag is safe to enable anywhere.
//
// The point of this file is narrow: internal/fixture was hand-shaped field by
// field against kmesh's pkg/status types rather than captured from output, and
// that is a claim which only a real daemon can settle. Everything here either
// confirms it or records exactly where it is wrong.
package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/fixture"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/mcpserver"
)

// liveAddr returns the real daemon address, or skips.
func liveAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("KMESH_DAEMON_ADDR")
	if addr == "" {
		t.Skip("KMESH_DAEMON_ADDR not set; no live daemon to test against")
	}
	return addr
}

// capture writes a body next to the other artifacts when LIVE_CAPTURE_DIR is
// set, so CI can upload exactly what the daemon said.
func capture(t *testing.T, name string, body []byte) {
	t.Helper()
	dir := os.Getenv("LIVE_CAPTURE_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("capture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatalf("capture %s: %v", name, err)
	}
}

// rawGet fetches a route straight off the daemon, bypassing the client, so the
// comparison is against bytes rather than against our own decoding.
func rawGet(t *testing.T, addr, route string) ([]byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+route, nil)
	if err != nil {
		t.Fatalf("new request %s: %v", route, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", route, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", route, err)
	}
	return body, resp.StatusCode
}

// liveStack mirrors newStack, with the fixture daemon replaced by a real one.
// Nothing else about the server changes, which is the claim being tested: the
// fixture stands in for the daemon, it is not a mode of the server.
func liveStack(t *testing.T) (*mcp.ClientSession, kmeshapi.Mode) {
	t.Helper()
	addr := liveAddr(t)

	client := kmeshapi.NewClient(addr)
	resolved, err := client.ResolveMode(context.Background())
	if err != nil {
		t.Fatalf("ResolveMode against live daemon at %s: %v", addr, err)
	}
	t.Logf("live daemon at %s resolved to mode %q", addr, resolved)

	srv, err := mcpserver.New(mcpserver.Config{Client: client, ResolvedMode: resolved})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
	mcpSrv := httptest.NewServer(mcpserver.NewHandler(srv, mcpserver.HandlerOptions{
		Stateless: true, JSONResponse: true,
	}))
	t.Cleanup(mcpSrv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "kmesh-mcp-live", Version: "v0.1.0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             mcpSrv.URL,
			DisableStandaloneSSE: true,
		}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, resolved
}

// --- mode resolution against a real daemon ---------------------------------

// TestLiveResolveModeMatchesDeployment checks the startup probe against real
// kmesh rather than the fixture. checkAdsMode and checkWorkloadMode
// (pkg/status/status_server.go:131-149) answer 400 for the mode the daemon is
// not in, so exactly one config dump route may answer 200.
func TestLiveResolveModeMatchesDeployment(t *testing.T) {
	addr := liveAddr(t)

	ok := 0
	for _, m := range kmeshapi.Modes {
		body, code := rawGet(t, addr, m.Route())
		t.Logf("GET %s -> %d (%d bytes)", m.Route(), code, len(body))
		capture(t, fmt.Sprintf("config_dump_%s.json", m), body)
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusBadRequest:
			if got := string(body); got != kmeshapi.InvalidModeMessage {
				t.Errorf("400 body for %s = %q, want %q (invalidModeErrMessage, status_server.go:68)",
					m, got, kmeshapi.InvalidModeMessage)
			}
		default:
			t.Errorf("GET %s: unexpected status %d", m.Route(), code)
		}
	}
	if ok != 1 {
		t.Fatalf("%d config dump routes answered 200, want exactly 1", ok)
	}
}

// --- the fixture's shape, settled against the daemon -----------------------

// TestLiveVersionShapeMatchesFixture compares the real /version document with
// the fixture's. Values must differ, a real build is not a fixture build, so
// this asserts on the field set only.
func TestLiveVersionShapeMatchesFixture(t *testing.T) {
	addr := liveAddr(t)

	body, code := rawGet(t, addr, kmeshapi.RouteVersion)
	capture(t, "version.json", body)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", kmeshapi.RouteVersion, code, body)
	}
	t.Logf("live /version: %s", body)

	var live map[string]any
	if err := json.Unmarshal(body, &live); err != nil {
		t.Fatalf("live /version is not a JSON object: %v", err)
	}

	fixtureBody, err := json.Marshal(fixture.Version)
	if err != nil {
		t.Fatalf("marshal fixture.Version: %v", err)
	}
	var fixed map[string]any
	if err := json.Unmarshal(fixtureBody, &fixed); err != nil {
		t.Fatalf("unmarshal fixture.Version: %v", err)
	}

	if missing, extra := keyDiff(fixed, live); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("fixture /version field set disagrees with the daemon:\n"+
			"  in fixture but not daemon: %v\n"+
			"  in daemon but not fixture: %v", missing, extra)
	}

	// The client's typed decode is the thing the tools actually rely on.
	var typed kmeshapi.VersionInfo
	if err := json.Unmarshal(body, &typed); err != nil {
		t.Fatalf("live /version does not decode into kmeshapi.VersionInfo: %v", err)
	}
	if typed.GitVersion == "" {
		t.Error("live /version decoded but gitVersion is empty")
	}
}

// TestLiveLoggerNamesMatchFixture compares the daemon's logger set with the
// fixture's. Unlike /version this is a fixed enumeration in kmesh rather than
// build metadata, so a difference here is a real drift in the fixture and is
// reported as one.
func TestLiveLoggerNamesMatchFixture(t *testing.T) {
	addr := liveAddr(t)

	body, code := rawGet(t, addr, kmeshapi.RouteLoggers)
	capture(t, "loggers.json", body)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", kmeshapi.RouteLoggers, code, body)
	}

	var live []string
	if err := json.Unmarshal(body, &live); err != nil {
		t.Fatalf("live /debug/loggers is not a JSON array of strings: %v", err)
	}
	sort.Strings(live)

	fixed := append([]string(nil), fixture.LoggerNames...)
	sort.Strings(fixed)

	t.Logf("daemon loggers:  %v", live)
	t.Logf("fixture loggers: %v", fixed)

	if !reflect.DeepEqual(live, fixed) {
		t.Errorf("fixture logger names drifted from the daemon\n  daemon:  %v\n  fixture: %v", live, fixed)
	}
}

// --- the whole stack, end to end, over MCP ---------------------------------

// TestLiveVersionThroughMCP drives the real daemon through the full protocol
// path: tools/call over stateless Streamable HTTP, structured content decoded
// on the client side. Asserts the value reaching an agent is the value the
// daemon reported, not something the server invented.
func TestLiveVersionThroughMCP(t *testing.T) {
	addr := liveAddr(t)
	cs, _ := liveStack(t)

	raw, code := rawGet(t, addr, kmeshapi.RouteVersion)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d", kmeshapi.RouteVersion, code)
	}
	var direct kmeshapi.VersionInfo
	if err := json.Unmarshal(raw, &direct); err != nil {
		t.Fatalf("decode direct /version: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "kmesh_version"})
	if err != nil {
		t.Fatalf("tools/call kmesh_version: %v", err)
	}
	var got mcpserver.VersionResult
	decode(t, res, &got)

	t.Logf("through MCP: %+v", got)

	if got.Daemon.GitVersion != direct.GitVersion {
		t.Errorf("gitVersion through MCP = %q, daemon says %q", got.Daemon.GitVersion, direct.GitVersion)
	}
	if got.Daemon.GitCommit != direct.GitCommit {
		t.Errorf("gitCommit through MCP = %q, daemon says %q", got.Daemon.GitCommit, direct.GitCommit)
	}
}

// TestLiveConfigDumpThroughMCP calls the config dump tool with no arguments at
// all, which is the case the startup mode probe exists to serve, and then
// checks the other mode is refused with the daemon's own 400.
func TestLiveConfigDumpThroughMCP(t *testing.T) {
	cs, resolved := liveStack(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "kmesh_config_dump"})
	if err != nil {
		t.Fatalf("tools/call kmesh_config_dump with no arguments: %v", err)
	}
	var got mcpserver.ConfigDumpResult
	decode(t, res, &got)
	if got.Mode != string(resolved) {
		t.Errorf("config dump reported mode %q, daemon resolved to %q", got.Mode, resolved)
	}
	if got.ModeSource != mcpserver.ModeSourceStartup {
		t.Errorf("modeSource = %q, want %q: a call with no arguments must be answered from the startup probe",
			got.ModeSource, mcpserver.ModeSourceStartup)
	}

	other := kmeshapi.ModeKernelNative
	if resolved == kmeshapi.ModeKernelNative {
		other = kmeshapi.ModeDualEngine
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "kmesh_config_dump",
		Arguments: map[string]any{"mode": string(other)},
	})
	if err != nil {
		t.Fatalf("tools/call kmesh_config_dump mode=%s: %v", other, err)
	}
	if !res.IsError {
		t.Errorf("asking a %s daemon for a %s dump succeeded; expected the daemon's 400 to surface as a tool error",
			resolved, other)
	} else {
		t.Logf("wrong-mode dump correctly refused: %s", textOf(res))
	}
}

// keyDiff reports top-level keys present in a but not b, and vice versa.
func keyDiff(a, b map[string]any) (missing, extra []string) {
	for k := range a {
		if _, ok := b[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}
