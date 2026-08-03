// Command kmesh-mcp serves the kmesh MCP server over Streamable HTTP.
//
// On startup it probes the kmesh daemon once to learn which dataplane mode it
// is running in, then serves three read-only tools against MCP protocol
// revision 2026-07-28.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/mcpserver"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/tokenauth"
)

func main() {
	daemon := flag.String("daemon", kmeshapi.DefaultAddr,
		"kmesh daemon admin address")
	listen := flag.String("listen", ":8080",
		"address to serve MCP on")
	stateless := flag.Bool("stateless", true,
		"serve statelessly, as protocol revision 2026-07-28 requires; set false to compare against session-based behaviour")
	jsonResponse := flag.Bool("json-response", true,
		"reply with application/json instead of text/event-stream")
	token := flag.String("token", "",
		"bearer token the stub verifier accepts; empty disables the auth middleware")
	flag.Parse()

	client := kmeshapi.NewClient(*daemon)

	// The one startup probe. This is a read of the daemon's own state, not a
	// cached protocol session: kmesh answers 400 on the config dump route for
	// the mode it is not running in, so exactly one route answers 200.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mode, err := client.ResolveMode(ctx)
	if err != nil {
		log.Fatalf("resolving dataplane mode from %s: %v", *daemon, err)
	}
	log.Printf("daemon at %s resolved to dataplane mode %q", *daemon, mode)

	srv, err := mcpserver.New(mcpserver.Config{Client: client, ResolvedMode: mode})
	if err != nil {
		log.Fatalf("building MCP server: %v", err)
	}

	var h http.Handler = mcpserver.NewHandler(srv, mcpserver.HandlerOptions{
		Stateless:    *stateless,
		JSONResponse: *jsonResponse,
	})
	if *token != "" {
		h = mcpserver.WithBearerAuth(h, tokenauth.StubVerifier(*token))
		log.Printf("bearer auth enabled (stub verifier, not a real TokenReview)")
	} else {
		log.Printf("bearer auth DISABLED; pass -token to exercise the auth middleware")
	}

	mux := http.NewServeMux()
	mux.Handle(mcpserver.MCPPath, h)

	log.Printf("kmesh-mcp %s serving MCP %s on %s%s (stateless=%v)",
		mcpserver.ServerVersion, mcpserver.ProtocolRevision, *listen, mcpserver.MCPPath, *stateless)
	if err := (&http.Server{Addr: *listen, Handler: mux}).ListenAndServe(); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}
