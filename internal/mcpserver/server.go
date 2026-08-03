// Package mcpserver builds the kmesh MCP server: three tools over the kmesh
// daemon's read-only admin API, served on Streamable HTTP against MCP protocol
// revision 2026-07-28.
package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
)

// ProtocolRevision is the MCP revision this server is built against. It is
// Current (not a release candidate) as of 2026-08-02; the spec repo tags
// 2026-07-28 with prerelease=false and tags the earlier candidate separately
// as 2026-07-28-RC.
const ProtocolRevision = "2026-07-28"

// ServerName and ServerVersion identify this implementation in server/discover.
const (
	ServerName    = "kmesh-mcp"
	ServerVersion = "v0.1.0"
)

// Tool argument types. These are the schemas from the design note, unchanged:
// one no-argument tool, one optional enum whose default the server resolves,
// and one optional string that switches the shape of the response.

// VersionArgs takes no arguments.
type VersionArgs struct{}

// ConfigDumpArgs carries the dataplane mode to dump.
//
// Mode is optional by design. The server probes the daemon once at startup and
// uses the resolved value when the argument is absent; an explicit argument
// overrides it. That is the whole answer to "should the server detect the mode
// once at startup and drop mode from the inputs" — it can do the first without
// doing the second, and needs no protocol session to manage it.
type ConfigDumpArgs struct {
	Mode string `json:"mode,omitempty" jsonschema:"kmesh dataplane mode; omit to use the mode this server resolved from the daemon at startup"`
}

// GetLoggersArgs carries an optional logger selector.
type GetLoggersArgs struct {
	Name string `json:"name,omitempty" jsonschema:"logger name; omit to list all logger names"`
}

// Tool result types. Each is a concrete Go type so the SDK derives an output
// schema and populates structuredContent (SEP-2106).

// VersionResult is the kmesh_version response.
type VersionResult struct {
	Address string               `json:"address"`
	Daemon  kmeshapi.VersionInfo `json:"daemon"`
}

// ConfigDumpResult is the kmesh_config_dump response.
//
// ModeSource is reported so a caller can tell whether the mode it got back was
// the one it asked for or the one the server resolved on its behalf.
type ConfigDumpResult struct {
	Mode       string         `json:"mode"`
	ModeSource string         `json:"modeSource"`
	Dump       map[string]any `json:"dump"`
}

// Mode sources reported by ConfigDumpResult.
const (
	ModeSourceArgument = "argument"
	ModeSourceStartup  = "startup-probe"
)

// LoggersResult is the kmesh_get_loggers response. Exactly one field is
// populated: Names when no selector was given, Logger when one was.
type LoggersResult struct {
	Names  []string             `json:"names,omitempty"`
	Logger *kmeshapi.LoggerInfo `json:"logger,omitempty"`
}

// Config configures a server.
type Config struct {
	// Client talks to the kmesh daemon.
	Client *kmeshapi.Client
	// ResolvedMode is the dataplane mode probed from the daemon at startup.
	// It becomes the default for kmesh_config_dump.
	ResolvedMode kmeshapi.Mode
}

// ConfigDumpInputSchema returns the hand-adjusted input schema for
// kmesh_config_dump.
//
// Inference from the Go type gives the object, the property and the
// description; the enum is added on top so the SDK rejects an unknown mode
// before the handler runs, rather than the handler having to check it. The
// inferred schema already sets additionalProperties:false.
func ConfigDumpInputSchema() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[ConfigDumpArgs](nil)
	if err != nil {
		return nil, fmt.Errorf("inferring config dump schema: %w", err)
	}
	prop, ok := s.Properties["mode"]
	if !ok {
		return nil, errors.New("inferred config dump schema has no mode property")
	}
	prop.Enum = make([]any, 0, len(kmeshapi.Modes))
	for _, m := range kmeshapi.Modes {
		prop.Enum = append(prop.Enum, string(m))
	}
	return s, nil
}

// New builds the MCP server and registers the three tools.
func New(cfg Config) (*mcp.Server, error) {
	if cfg.Client == nil {
		return nil, errors.New("mcpserver: Config.Client is required")
	}
	if !cfg.ResolvedMode.Valid() {
		return nil, fmt.Errorf("mcpserver: resolved mode %q is not a kmesh mode", cfg.ResolvedMode)
	}

	// Instructions are returned by server/discover, so the mode the server
	// resolved at startup is discoverable in one request, before any tool call.
	instructions := fmt.Sprintf(
		"Read-only access to a kmesh daemon at %s. The daemon reported dataplane mode %q at startup; "+
			"kmesh_config_dump uses that mode when its mode argument is omitted.",
		cfg.Client.Addr(), cfg.ResolvedMode)

	srv := mcp.NewServer(&mcp.Implementation{
		Name:        ServerName,
		Title:       "Kmesh",
		Description: "Read-only tools over the kmesh daemon admin API.",
		Version:     ServerVersion,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "kmesh_version",
		Title:       "Kmesh version",
		Description: "Build and version information reported by the kmesh daemon.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handleVersion(cfg))

	dumpSchema, err := ConfigDumpInputSchema()
	if err != nil {
		return nil, err
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "kmesh_config_dump",
		Title: "Kmesh config dump",
		Description: fmt.Sprintf(
			"Dump the kmesh daemon's xDS configuration. Omit mode to use %q, which this server "+
				"resolved from the daemon at startup.", cfg.ResolvedMode),
		InputSchema: dumpSchema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handleConfigDump(cfg))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "kmesh_get_loggers",
		Title:       "Kmesh loggers",
		Description: "List the kmesh daemon's logger names, or report the level of one named logger.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, handleGetLoggers(cfg))

	return srv, nil
}

func handleVersion(cfg Config) mcp.ToolHandlerFor[VersionArgs, VersionResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ VersionArgs) (*mcp.CallToolResult, VersionResult, error) {
		info, err := cfg.Client.Version(ctx)
		if err != nil {
			return nil, VersionResult{}, err
		}
		return nil, VersionResult{Address: cfg.Client.Addr(), Daemon: *info}, nil
	}
}

func handleConfigDump(cfg Config) mcp.ToolHandlerFor[ConfigDumpArgs, ConfigDumpResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args ConfigDumpArgs) (*mcp.CallToolResult, ConfigDumpResult, error) {
		mode, source := cfg.ResolvedMode, ModeSourceStartup
		if args.Mode != "" {
			mode, source = kmeshapi.Mode(args.Mode), ModeSourceArgument
		}
		dump, err := cfg.Client.ConfigDump(ctx, mode)
		if err != nil {
			// A mode mismatch is the caller's problem, not a protocol fault, so
			// it comes back as a tool error the model can read and correct.
			var modeErr *kmeshapi.ModeError
			if errors.As(err, &modeErr) {
				return nil, ConfigDumpResult{}, fmt.Errorf(
					"%w; this daemon resolved to %q at startup, so omit the mode argument or pass that value",
					modeErr, cfg.ResolvedMode)
			}
			return nil, ConfigDumpResult{}, err
		}
		return nil, ConfigDumpResult{Mode: string(mode), ModeSource: source, Dump: dump}, nil
	}
}

func handleGetLoggers(cfg Config) mcp.ToolHandlerFor[GetLoggersArgs, LoggersResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args GetLoggersArgs) (*mcp.CallToolResult, LoggersResult, error) {
		if args.Name == "" {
			names, err := cfg.Client.LoggerNames(ctx)
			if err != nil {
				return nil, LoggersResult{}, err
			}
			return nil, LoggersResult{Names: names}, nil
		}
		info, err := cfg.Client.LoggerLevel(ctx, args.Name)
		if err != nil {
			return nil, LoggersResult{}, err
		}
		return nil, LoggersResult{Logger: info}, nil
	}
}
