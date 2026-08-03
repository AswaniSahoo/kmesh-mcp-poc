// Command fixture runs a stand-in for the kmesh daemon's admin server.
//
// It exists so the MCP server can be demonstrated with no Kubernetes cluster,
// no eBPF datapath and no root. It serves hand-authored payloads shaped like
// the real daemon's; see package fixture for what that does and does not
// prove.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/fixture"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
)

func main() {
	addr := flag.String("addr", kmeshapi.DefaultAddr,
		"address to listen on; the default matches kmesh's own adminAddr")
	mode := flag.String("mode", string(kmeshapi.ModeDualEngine),
		"dataplane mode to impersonate: kernel-native or dual-engine")
	flag.Parse()

	m := kmeshapi.Mode(*mode)
	if !m.Valid() {
		fmt.Fprintf(os.Stderr, "unknown mode %q (want kernel-native or dual-engine)\n", *mode)
		os.Exit(2)
	}

	log.Printf("fixture kmesh daemon listening on %s, impersonating mode %q", *addr, m)
	log.Printf("  %s", kmeshapi.RouteVersion)
	log.Printf("  %s", m.Route())
	log.Printf("  %s   (other mode answers 400 Invalid Client Mode, as kmesh does)", kmeshapi.RouteLoggers)

	srv := &http.Server{Addr: *addr, Handler: fixture.New(m)}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("fixture server: %v", err)
	}
}
