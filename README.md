# kmesh-mcp-poc

A working MCP server for [kmesh](https://github.com/kmesh-net/kmesh), built against MCP
protocol revision **2026-07-28**, that runs with **no Kubernetes cluster, no eBPF datapath
and no root**.

It exists to answer three questions in code rather than in prose:

1. Does an MCP server for kmesh work under the 2026-07-28 revision, where the `initialize`
   handshake and `Mcp-Session-Id` are gone?
2. If there is no protocol session, can the `mode` argument on a config-dump tool still be
   optional — or must every caller now pass it explicitly?
3. How much of this can be demonstrated without a cluster?

Short answers: yes; yes, and the mechanism is already in kmesh's own source; more than
expected, but well short of everything — see [What this does not cover](#what-this-does-not-cover).

This is a proof of concept in a personal repository, not a proposed change to kmesh.

---

## Run it

Needs Go 1.25.0 or later, and nothing else.

```
git clone https://github.com/AswaniSahoo/kmesh-mcp-poc
cd kmesh-mcp-poc
go run ./cmd/demo
```

That starts a fixture kmesh daemon, resolves the dataplane mode from it, serves MCP in
front of it, and prints every JSON-RPC exchange. One process, no cluster, no root, no ports
to free. The full transcript is committed at [docs/demo-output.txt](docs/demo-output.txt).

```
go test ./...        # the test suite
go vet ./...
go run ./cmd/fixture # two-terminal mode: fixture daemon on :15200
go run ./cmd/kmesh-mcp -daemon localhost:15200 -listen :8080
```

A [Makefile](Makefile) wraps these as `make run` / `make test` / `make check` / `make serve`.
The `go` commands above are the verified path — `make` was not available on the machine this
was built on, so the Makefile itself is untested.

---

## The three tools

Three tools, chosen so that each exercises a different input shape rather than three
variations of the same one.

| Tool | Input shape | kmesh route |
|---|---|---|
| `kmesh_version` | no arguments | `GET /version` |
| `kmesh_config_dump` | optional enum, server-resolved default | `GET /debug/config_dump/{kernel-native,dual-engine}` |
| `kmesh_get_loggers` | optional string that changes the response shape | `GET /debug/loggers[?name=]` |

Route names, response shapes and error behaviour were read from `kmesh-net/kmesh` at commit
`c88ef300`, `pkg/status/status_server.go`. The listen address `:15200` is kmesh's own
`adminAddr` (`status_server.go:48`), not an invented port.

---

## The mode question, answered from kmesh's source

An open question in [kmesh-net/kmesh#1800](https://github.com/kmesh-net/kmesh/issues/1800):
should the server detect the dataplane mode once at startup and drop `mode` from the tool
inputs entirely?

Under 2026-07-28 this looks harder than it is, because the revision removed sessions —
"servers that need cross-call state use explicit, server-minted handles passed as ordinary
tool arguments" (SEP-2567).

But the mode is not cross-call state. It is a fact about the daemon, and kmesh already
publishes it. Both config-dump handlers gate on whether the matching xDS controller exists
and answer **HTTP 400** when it does not:

```go
func (s *Server) checkAdsMode(w http.ResponseWriter) bool {
	client := s.xdsClient
	if client == nil || client.AdsController == nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, invalidModeErrMessage)   // "\tInvalid Client Mode\n"
		return false
	}
	return true
}
```
<sub>kmesh `pkg/status/status_server.go:131-149`</sub>

So exactly one of the two routes answers 200 on a healthy daemon. One probe at startup
reads the mode off the daemon itself. That is not a cached session; it is the server
knowing a fact about what it is connected to.

The result: `mode` stays **optional**. Omit it and the server uses what it resolved;
pass it and the argument wins. Here is that call with no arguments at all:

```jsonc
-> POST /mcp
   MCP-Protocol-Version: 2026-07-28   Mcp-Method: tools/call   Mcp-Name: kmesh_config_dump
{
  "id": 4,
  "jsonrpc": "2.0",
  "method": "tools/call",
  "params": {
    "_meta": {
      "io.modelcontextprotocol/clientCapabilities": {},
      "io.modelcontextprotocol/protocolVersion": "2026-07-28"
    },
    "arguments": {},
    "name": "kmesh_config_dump"
  }
}

<- 200 OK
{
  "jsonrpc": "2.0",
  "id": 4,
  "result": {
    "structuredContent": {
      "mode": "dual-engine",
      "modeSource": "startup-probe",
      "dump": { "workloads": [ ... ], "services": [ ... ], "policies": [ ... ] }
    },
    "resultType": "complete"
  }
}
```

`modeSource` is reported so a caller can tell whether it got the mode it asked for or the
one the server resolved for it. Full untruncated response: [docs/demo-output.txt](docs/demo-output.txt).

The resolved mode is also discoverable *before* any tool call, in the mandatory
`server/discover` response:

```json
"instructions": "Read-only access to a kmesh daemon at 127.0.0.1:60925. The daemon reported dataplane mode \"dual-engine\" at startup; kmesh_config_dump uses that mode when its mode argument is omitted."
```

---

## Three things that only showed up by running it

These were not obvious from the changelog, and each one is a 400 until you fix it.

**1. The `MCP-Protocol-Version` HTTP header is not sufficient.** A request that sets only
the header is rejected:

```json
{"code": -32602, "message": "missing or invalid _meta field \"io.modelcontextprotocol/protocolVersion\""}
```

The version must be in `params._meta`. Then `clientCapabilities` is required there too —
with `initialize` gone, there is no other moment at which a client could declare what it
supports, so it declares it on every request.

**2. `Mcp-Method` is enforced, with its own error code.** Omitting it fails before the body
is parsed:

```json
{"code": -32020, "message": "missing required Mcp-Method header"}
```

**3. Version negotiation is genuinely per-request.** A client declaring `2025-06-18` is
served at that revision by the same server — and the response comes back *without*
`resultType`, because that field belongs to 2026-07-28. An unsupported revision is rejected
with the supported set attached, so the client can retry immediately:

```
<- 400 Bad Request
Bad Request: Unsupported protocol version (supported versions: 2026-07-28,2025-11-25,2025-06-18,2025-03-26,2024-11-05)
```

---

## Schema validation is real, not documentation

`kmesh_config_dump`'s `mode` property carries a hand-added `enum` on top of the inferred
schema. The handler never checks the value. An invalid mode is still rejected:

```
"text": "validating \"arguments\": validating root: validating /properties/mode: enum: sidecar does not equal any of: [kernel-native dual-engine]"
"isError": true
```

It comes back as a **tool** error rather than a protocol error, which is the useful
behaviour: a model can read it and correct itself. Enforcement is
`github.com/google/jsonschema-go v0.4.3` via the SDK's SEP-2106 validation path. This was an
open question when the design was written; `TestConfigDumpRejectsUnknownMode` settles it.

---

## Statelessness is one struct field

```go
mcp.NewStreamableHTTPHandler(
	func(*http.Request) *mcp.Server { return srv },
	&mcp.StreamableHTTPOptions{Stateless: true},
)
```

Its observable cost, shown in the transcript: `GET /mcp` returns **405 Method Not Allowed**,
because a stateless server has no stream to open. The same server built with
`Stateless: false` answers 400 instead.

The SDK notes that the spec's stateless semantics are still settling. If they move,
reverting is a one-line change in [internal/mcpserver/http.go](internal/mcpserver/http.go)
and nowhere else in this repo.

---

## Go version

The SDK's `go.mod` declares `go 1.25.0`. kmesh declares `go 1.24.2`
(`kmesh/go.mod`, commit `c88ef300`). This module is standalone and declares `go 1.25.0`,
so nothing here is blocked by that.

It is not free, though: **vendoring an MCP server built on this SDK into kmesh itself would
require kmesh to raise its own go directive from 1.24.2 to at least 1.25.0.** That is a
decision for the maintainers, not a detail to discover during implementation. Anyone
proposing this work inside `kmesh/mcp/` should raise it up front.

---

## Tests

```
$ go test ./...
ok  	github.com/AswaniSahoo/kmesh-mcp-poc/internal/mcpserver	3.844s
```

```
$ go test ./... -v
=== RUN   TestResolveModeReadsDaemon
=== RUN   TestResolveModeReadsDaemon/kernel-native
=== RUN   TestResolveModeReadsDaemon/dual-engine
--- PASS: TestResolveModeReadsDaemon (0.01s)
=== RUN   TestResolveModeFailsWhenDaemonHasNeitherMode
--- PASS: TestResolveModeFailsWhenDaemonHasNeitherMode (0.00s)
=== RUN   TestToolsListIsDeterministic
--- PASS: TestToolsListIsDeterministic (0.01s)
=== RUN   TestConfigDumpToolAdvertisesEnum
--- PASS: TestConfigDumpToolAdvertisesEnum (0.00s)
=== RUN   TestVersionTool
--- PASS: TestVersionTool (0.01s)
=== RUN   TestConfigDumpOmittedModeUsesStartupProbe
--- PASS: TestConfigDumpOmittedModeUsesStartupProbe (0.01s)
=== RUN   TestConfigDumpExplicitModeOverrides
--- PASS: TestConfigDumpExplicitModeOverrides (0.01s)
=== RUN   TestConfigDumpRejectsUnknownMode
--- PASS: TestConfigDumpRejectsUnknownMode (0.01s)
=== RUN   TestConfigDumpWrongModeIsToolError
--- PASS: TestConfigDumpWrongModeIsToolError (0.01s)
=== RUN   TestGetLoggersListsNames
--- PASS: TestGetLoggersListsNames (0.01s)
=== RUN   TestGetLoggersReturnsOneLevel
--- PASS: TestGetLoggersReturnsOneLevel (0.01s)
=== RUN   TestStatelessRejectsGET
--- PASS: TestStatelessRejectsGET (0.00s)
=== RUN   TestBearerAuthRejectsBadToken
--- PASS: TestBearerAuthRejectsBadToken (0.00s)
PASS
```

13 tests. The coverage is of the MCP layer and the daemon client, not of kmesh.

---

## What this does not cover

The point of this section is that the list above is small and this list is not.

**The fixtures are hand-authored, not captured.** Payloads in
[internal/fixture](internal/fixture) were written from the exported struct shapes read in
kmesh's source. They are not a recording of a live daemon. Field names and the
mode-mismatch behaviour are faithful; **the values are invented, and shape fidelity against
a real daemon is unproven.** Anything that depends on real data — volumes, protojson
encoding quirks in the kernel-native dump, timing — is not demonstrated here.

**No eBPF, and therefore no `get_bpf_maps`.** kmesh's `/debug/config_dump/bpf/*` routes read
live eBPF maps through `BackendLookupAll`, `EndpointLookupAll`, `FrontendLookupAll`,
`ServiceLookupAll`, `WorkloadPolicyLookupAll`, `ClusterLookupAll`, `ListenerLookupAll` and
`RouteConfigLookupAll`. No fixture substitutes for a loaded datapath. Those routes are
deliberately absent.

**No waypoint tools.** They need a live Kubernetes API with Gateway API CRDs installed.
`waypoint generate` is pure templating and would have been cheap, but it is excluded to keep
the boundary of this PoC unambiguous.

**Authentication is a seam, not an implementation.** The SDK's bearer middleware really is in
the request path and really does reject bad tokens — `TestBearerAuthRejectsBadToken` proves
that. The verifier behind it compares against a fixed string. A real deployment replaces
[internal/tokenauth](internal/tokenauth) with a Kubernetes TokenReview call and changes
nothing else. **mTLS is not addressed at all.**

**Read-only.** No `log set`, no `authz enable/disable`, no monitoring toggles. Every tool is
a GET.

**Not built inside kmesh.** No `go build` or `go get` was run against the kmesh tree, so
whether this SDK coexists cleanly with kmesh's dependency graph — `cilium/ebpf`,
`envoyproxy/go-control-plane` — is **untested**. See [Go version](#go-version).

**Three tools, not ten.** #1800 lists ten core tools and five stretch tools. This covers
three, chosen for schema variety. Nothing here says the other seven are easy.

**No kmesh CI, no e2e, no container image, no `kmeshctl mcp serve`.**

---

## Licence

Apache-2.0. See [LICENSE](LICENSE).
