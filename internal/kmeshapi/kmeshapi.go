// Package kmeshapi is a thin HTTP client for the subset of the kmesh daemon's
// admin API that this proof of concept exposes over MCP.
//
// Route names, response shapes and error behaviour were read from
// kmesh-net/kmesh at commit c88ef300, pkg/status/status_server.go. Line
// references appear on the declarations below so they can be re-checked
// against upstream.
package kmeshapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/tracectx"
)

// DefaultAddr is the kmesh daemon admin address. It matches adminAddr in
// pkg/status/status_server.go:48.
const DefaultAddr = "localhost:15200"

// Admin routes, from the pattern constants at pkg/status/status_server.go:50-57.
// Note that /debug/config_dump/bpf/* are separate routes backed by live eBPF
// maps; this package deliberately does not touch them.
const (
	RouteVersion            = "/version"                         // patternVersion
	RouteConfigDumpAds      = "/debug/config_dump/kernel-native" // patternConfigDumpAds
	RouteConfigDumpWorkload = "/debug/config_dump/dual-engine"   // patternConfigDumpWorkload
	RouteLoggers            = "/debug/loggers"                   // patternLoggers
)

// InvalidModeMessage is the body kmesh returns with HTTP 400 when a config
// dump is requested for a mode the daemon is not running in. Byte-for-byte
// from invalidModeErrMessage at pkg/status/status_server.go:68.
const InvalidModeMessage = "\tInvalid Client Mode\n"

// Mode is the kmesh dataplane mode.
type Mode string

const (
	ModeKernelNative Mode = "kernel-native"
	ModeDualEngine   Mode = "dual-engine"
)

// Modes lists every valid mode, in the order the enum is advertised.
var Modes = []Mode{ModeKernelNative, ModeDualEngine}

// Valid reports whether m is a mode kmesh recognises.
func (m Mode) Valid() bool {
	for _, v := range Modes {
		if m == v {
			return true
		}
	}
	return false
}

// Route returns the config dump route serving this mode.
func (m Mode) Route() string {
	if m == ModeKernelNative {
		return RouteConfigDumpAds
	}
	return RouteConfigDumpWorkload
}

// VersionInfo mirrors kmesh's version.Info (pkg/version/version.go:33-41),
// which /version serves as indented JSON.
type VersionInfo struct {
	GitVersion   string `json:"gitVersion"`
	GitCommit    string `json:"gitCommit"`
	GitTreeState string `json:"gitTreeState"`
	BuildDate    string `json:"buildDate"`
	GoVersion    string `json:"goVersion"`
	Compiler     string `json:"compiler"`
	Platform     string `json:"platform"`
}

// LoggerInfo mirrors kmesh's status.LoggerInfo (pkg/status/status_server.go:206-209).
type LoggerInfo struct {
	Name  string `json:"name,omitempty"`
	Level string `json:"level,omitempty"`
}

// ModeError reports that the daemon is not running in the requested mode.
// kmesh signals this with HTTP 400 and InvalidModeMessage; see checkAdsMode
// and checkWorkloadMode at pkg/status/status_server.go:131-149.
type ModeError struct {
	Mode Mode
}

func (e *ModeError) Error() string {
	return fmt.Sprintf("daemon is not running in %s mode", e.Mode)
}

// Client talks to a kmesh daemon admin server.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Client for a daemon at addr (host:port).
func NewClient(addr string) *Client {
	return &Client{
		baseURL: "http://" + addr,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Addr returns the daemon address this client targets.
func (c *Client) Addr() string { return strings.TrimPrefix(c.baseURL, "http://") }

func (c *Client) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	// If the caller's MCP request carried W3C trace context, continue it on
	// the hop into the daemon. Same trace, new span. This is the point at
	// which an agent's tool call and the mesh's own view of that call become
	// the same trace rather than two unrelated ones.
	if tc := tracectx.FromContext(ctx); tc != nil {
		for k, v := range tc.Headers() {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// Version fetches /version.
func (c *Client) Version(ctx context.Context) (*VersionInfo, error) {
	body, code, err := c.get(ctx, RouteVersion)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %d: %s", RouteVersion, code, strings.TrimSpace(string(body)))
	}
	var info VersionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", RouteVersion, err)
	}
	return &info, nil
}

// ConfigDump fetches the config dump for mode.
//
// The two routes return structurally different documents: kernel-native serves
// a protojson-encoded Envoy admin ConfigDump (dynamicResources), dual-engine
// serves kmesh's own WorkloadDump (workloads/services/policies). Both are JSON
// objects, so both decode into a map.
func (c *Client) ConfigDump(ctx context.Context, mode Mode) (map[string]any, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
	route := mode.Route()
	body, code, err := c.get(ctx, route)
	if err != nil {
		return nil, err
	}
	if code == http.StatusBadRequest {
		return nil, &ModeError{Mode: mode}
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %d: %s", route, code, strings.TrimSpace(string(body)))
	}
	var dump map[string]any
	if err := json.Unmarshal(body, &dump); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", route, err)
	}
	return dump, nil
}

// LoggerNames fetches /debug/loggers with no name, which serves a JSON array
// of logger names (getLoggerNames, pkg/status/status_server.go).
func (c *Client) LoggerNames(ctx context.Context) ([]string, error) {
	body, code, err := c.get(ctx, RouteLoggers)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %d: %s", RouteLoggers, code, strings.TrimSpace(string(body)))
	}
	var names []string
	if err := json.Unmarshal(body, &names); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", RouteLoggers, err)
	}
	return names, nil
}

// LoggerLevel fetches /debug/loggers?name=<name>, which serves a single
// LoggerInfo (getLoggerLevel, pkg/status/status_server.go:376-388).
func (c *Client) LoggerLevel(ctx context.Context, name string) (*LoggerInfo, error) {
	route := RouteLoggers + "?name=" + url.QueryEscape(name)
	body, code, err := c.get(ctx, route)
	if err != nil {
		return nil, err
	}
	if code == http.StatusBadRequest {
		return nil, fmt.Errorf("unknown logger %q: %s", name, strings.TrimSpace(string(body)))
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %d: %s", route, code, strings.TrimSpace(string(body)))
	}
	var info LoggerInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", route, err)
	}
	return &info, nil
}

// ResolveMode asks the daemon which dataplane mode it is running in.
//
// This is a read, not a heuristic. kmesh's config dump handlers gate on
// whether the corresponding xDS controller exists and answer HTTP 400 when it
// does not (checkAdsMode / checkWorkloadMode, pkg/status/status_server.go:131-149),
// so exactly one of the two routes answers 200 on a healthy daemon. Calling
// this once at startup is what lets the mode argument be optional on the
// kmesh_config_dump tool without holding any protocol session.
func (c *Client) ResolveMode(ctx context.Context) (Mode, error) {
	var errs []string
	for _, m := range []Mode{ModeDualEngine, ModeKernelNative} {
		_, code, err := c.get(ctx, m.Route())
		if err != nil {
			return "", fmt.Errorf("probing %s: %w", m.Route(), err)
		}
		if code == http.StatusOK {
			return m, nil
		}
		errs = append(errs, fmt.Sprintf("%s=%d", m, code))
	}
	return "", fmt.Errorf("daemon at %s reports neither dataplane mode (%s)", c.Addr(), strings.Join(errs, " "))
}
