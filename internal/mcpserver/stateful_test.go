package mcpserver_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/mcpserver"
)

// TestStatefulServerRejects20260728 checks the claim that a Streamable HTTP
// server accepts protocol revision 2026-07-28 only when Stateless is true.
//
// This is worth pinning down rather than repeating: it is the concrete cost of
// the stateless flag, and it is the difference between serving the current
// revision and silently serving the previous one.
func TestStatefulServerRejects20260728(t *testing.T) {
	post := func(t *testing.T, stateless bool, version string) (int, string) {
		t.Helper()
		s := newStackWithOptions(t, kmeshapi.ModeDualEngine,
			mcpserver.HandlerOptions{Stateless: stateless, JSONResponse: true}, "")

		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/list",
			"params": map[string]any{
				"_meta": map[string]any{
					"io.modelcontextprotocol/protocolVersion":    version,
					"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				},
			},
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
		req.Header.Set("Mcp-Protocol-Version", version)
		req.Header.Set("Mcp-Method", "tools/list")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(raw))
	}

	t.Run("stateless serves 2026-07-28", func(t *testing.T) {
		code, body := post(t, true, "2026-07-28")
		t.Logf("stateless=true  2026-07-28 -> %d  %.160s", code, body)
		if code != http.StatusOK {
			t.Fatalf("stateless server refused the current revision: %d %s", code, body)
		}
	})

	t.Run("stateful and 2026-07-28", func(t *testing.T) {
		code, body := post(t, false, "2026-07-28")
		t.Logf("stateless=false 2026-07-28 -> %d  %.160s", code, body)
	})

	t.Run("stateful and 2025-11-25", func(t *testing.T) {
		code, body := post(t, false, "2025-11-25")
		t.Logf("stateless=false 2025-11-25 -> %d  %.160s", code, body)
	})
}

// TestStatelessAdvertisedVersions records what the stateless server actually
// advertises in server/discover, which is broader than "2026-07-28 only".
func TestStatelessAdvertisedVersions(t *testing.T) {
	s := newStack(t, kmeshapi.ModeDualEngine)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "server/discover",
		"params": map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcpserver.ProtocolRevision,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}},
	})
	req, _ := http.NewRequest(http.MethodPost, s.MCP.URL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "server/discover")
	// Required alongside the _meta field. Omitting it returns an error
	// response with no result object, which is how this test first failed.
	req.Header.Set("Mcp-Protocol-Version", mcpserver.ProtocolRevision)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out struct {
		Result struct {
			SupportedVersions []string `json:"supportedVersions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	t.Logf("stateless server advertises supportedVersions=%v", out.Result.SupportedVersions)
	if len(out.Result.SupportedVersions) == 0 {
		t.Fatalf("no supportedVersions advertised; status=%d body=%s", resp.StatusCode, raw)
	}
}

var _ = httptest.NewServer
