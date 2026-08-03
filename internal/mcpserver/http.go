package mcpserver

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is where the Streamable HTTP endpoint is mounted.
const MCPPath = "/mcp"

// HandlerOptions configures the HTTP transport.
type HandlerOptions struct {
	// Stateless is the flag that makes this a 2026-07-28 server: no
	// Mcp-Session-Id is read or set, every request stands alone, and GET and
	// DELETE on the endpoint answer 405 Method Not Allowed.
	//
	// It is one field. If the spec's stateless semantics move — the SDK notes
	// they are still settling — reverting to session-based behaviour is a
	// one-line change here and nowhere else in this repo.
	Stateless bool

	// JSONResponse makes replies application/json instead of text/event-stream.
	// The PoC turns this on so the demo transcripts in the README show plain
	// JSON-RPC rather than SSE framing. It has no bearing on statelessness.
	JSONResponse bool
}

// NewHandler wraps srv in a Streamable HTTP handler.
func NewHandler(srv *mcp.Server, opts HandlerOptions) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Stateless:    opts.Stateless,
			JSONResponse: opts.JSONResponse,
		},
	)
}

// WithBearerAuth puts the SDK's bearer-token middleware in front of h.
//
// auth.RequireBearerToken is a middleware factory: it takes the verifier and
// returns the wrapper, rather than taking the handler directly.
func WithBearerAuth(h http.Handler, verifier auth.TokenVerifier) http.Handler {
	return auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{
		Scopes: []string{"kmesh:read"},
	})(h)
}
