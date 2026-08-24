// Package fixture stands up a stand-in for the kmesh daemon's admin server so
// the MCP server can be run and tested with no cluster, no eBPF and no root.
//
// The payloads below are AUTHORED, not captured. What they are checked against
// is the Go types the daemon actually marshals, read from kmesh-net/kmesh at
// commit c88ef300: status.WorkloadDump, status.Workload, status.Service and
// status.AuthorizationPolicy (pkg/status/api.go:32-91), including their json
// tags, which fields carry omitempty, the enum String() casing, and the sort
// applied before marshalling. So the SHAPE is faithful down to key names.
//
// The VALUES are invented. Nothing here was recorded from a running daemon, so
// anything depending on real data (volumes, timing, protojson quirks in the
// kernel-native dump, whether a real cluster ever produces this exact
// combination of fields) is not demonstrated by this package.
//
// Importing kmesh's own handlers instead is not an option for a separate Go
// module: pkg/status reaches pkg/bpf, which reaches bpf2go-generated packages
// that //go:embed compiled eBPF .o files, and those are build artifacts
// excluded by kmesh's .gitignore. Verified by trying it.
package fixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/tracectx"
)

// Version is the fixture's /version payload, shaped like kmesh's version.Info.
var Version = kmeshapi.VersionInfo{
	GitVersion:   "v1.2.0-fixture",
	GitCommit:    "c88ef300",
	GitTreeState: "clean",
	BuildDate:    "2026-08-02T00:00:00Z",
	GoVersion:    "go1.25.0",
	Compiler:     "gc",
	Platform:     "linux/amd64",
}

// LoggerNames is the fixture's /debug/loggers payload.
//
// The daemon serves logger.GetLoggerNames() with the "bpf" pseudo-logger
// appended (getLoggerNames, pkg/status/status_server.go:359-360), and
// loggerMap holds exactly "default" and "fileOnly"
// (pkg/logger/logger.go:54-57). This list was previously guessed, and a live
// daemon disagreed; see TestLiveLoggerNamesMatchFixture.
//
// Note that GetLoggerNames ranges over a map, so the two real names arrive in
// non-deterministic order and a client must not depend on the ordering. Only
// the trailing "bpf" has a fixed position.
var LoggerNames = []string{"default", "fileOnly", "bpf"}

// LoggerLevels backs /debug/loggers?name=<name>. The names are kmesh's; the
// levels are invented, like the rest of this package's values.
var LoggerLevels = map[string]string{
	"default":  "info",
	"fileOnly": "info",
	"bpf":      "error",
}

// workloadDump is the dual-engine payload.
//
// Every field below is taken from the Go types the daemon actually marshals,
// not guessed: status.WorkloadDump (pkg/status/status_server.go:466-470) over
// status.Workload, status.Service and status.AuthorizationPolicy
// (pkg/status/api.go:32-91). Four details are easy to get wrong and are
// deliberately right here:
//
//   - a Service's addresses serialise under the key "vips", not "addresses"
//     (api.go:79), and each entry is "<network>/<ip>", not a bare IP, because
//     ConvertService joins them (api.go:152) while ConvertWorkload does not
//     (api.go:103);
//   - a Service with NO waypoint still emits {"destination": ""} rather than
//     null, because ConvertService allocates the struct unconditionally
//     (api.go:167) while guarding LoadBalancer three lines below. Both of the
//     above were found by writing this fixture and checking it against the
//     source; the waypoint one is filed upstream as a fix. Update this payload
//     to null if that lands;
//   - workloadType and status are enum String() values, so they are upper case:
//     "POD" and "HEALTHY" (workload.pb.go:146-151, :98-101);
//   - protocol, serviceAccount and status carry no omitempty, so a real daemon
//     emits them even when empty, and locality and applicationTunnel are
//     structs, where omitempty has no effect, so they appear as objects;
//   - printWorkloadDump sorts workloads, services and policies by name before
//     marshalling (status_server.go:566-575), so this payload is pre-sorted.
const workloadDump = `{
  "workloads": [
    {
      "uid": "Kubernetes//Pod/default/productpage-v1-fixture",
      "addresses": ["10.244.0.11"],
      "protocol": "NONE",
      "name": "productpage-v1-fixture",
      "namespace": "default",
      "serviceAccount": "bookinfo-productpage",
      "workloadName": "productpage-v1",
      "workloadType": "POD",
      "canonicalName": "productpage",
      "canonicalRevision": "v1",
      "clusterId": "Kubernetes",
      "locality": {},
      "node": "kmesh-worker",
      "network": "testnetwork",
      "status": "HEALTHY",
      "applicationTunnel": {"protocol": ""},
      "services": ["default/productpage.default.svc.cluster.local"]
    },
    {
      "uid": "Kubernetes//Pod/default/reviews-v2-fixture",
      "addresses": ["10.244.0.12"],
      "waypoint": "testnetwork/192.168.1.10",
      "protocol": "HBONE",
      "name": "reviews-v2-fixture",
      "namespace": "default",
      "serviceAccount": "bookinfo-reviews",
      "workloadName": "reviews-v2",
      "workloadType": "POD",
      "canonicalName": "reviews",
      "canonicalRevision": "v2",
      "clusterId": "Kubernetes",
      "locality": {"region": "us-east-1", "zone": "us-east-1a"},
      "node": "kmesh-worker",
      "network": "testnetwork",
      "status": "HEALTHY",
      "applicationTunnel": {"protocol": ""},
      "services": ["default/reviews.default.svc.cluster.local"],
      "authorizationPolicies": ["default/allow-productpage"]
    }
  ],
  "services": [
    {
      "name": "productpage",
      "namespace": "default",
      "hostname": "productpage.default.svc.cluster.local",
      "vips": ["testnetwork/10.96.0.21"],
      "ports": [{"servicePort": 9080, "targetPort": 9080}],
      "loadBalancer": null,
      "waypoint": {"destination": ""}
    },
    {
      "name": "reviews",
      "namespace": "default",
      "hostname": "reviews.default.svc.cluster.local",
      "vips": ["testnetwork/10.96.0.22"],
      "ports": [{"servicePort": 9080, "targetPort": 9080}],
      "loadBalancer": {"mode": "FAILOVER", "routingPreferences": ["NETWORK", "REGION"]},
      "waypoint": {"destination": "testnetwork/192.168.1.10"}
    }
  ],
  "policies": [
    {
      "name": "allow-productpage",
      "namespace": "default",
      "scope": "NAMESPACE",
      "action": "ALLOW",
      "rules": []
    }
  ]
}`

// adsDump is the kernel-native payload. kmesh serves this route with
// protojson over an Envoy admin ConfigDump (configDumpAds,
// pkg/status/status_server.go), so the shape is dynamicResources rather than
// kmesh's own types.
const adsDump = `{
  "dynamicResources": {
    "clusterConfigs": [
      {
        "cluster": {
          "name": "outbound|9080||productpage.default.svc.cluster.local",
          "type": "EDS",
          "connectTimeout": "10s"
        }
      }
    ],
    "listenerConfigs": [
      {
        "listener": {
          "name": "0.0.0.0_9080",
          "address": {"socketAddress": {"address": "0.0.0.0", "portValue": 9080}}
        }
      }
    ],
    "routeConfigs": [
      {
        "routeConfig": {
          "name": "9080",
          "virtualHosts": [
            {
              "name": "productpage.default.svc.cluster.local:9080",
              "domains": ["productpage.default.svc.cluster.local"]
            }
          ]
        }
      }
    ]
  }
}`

// Recorder captures the W3C trace headers the fixture daemon received on its
// most recent request. The real kmesh daemon does not do this; it exists so a
// test can assert that trace context actually crossed the hop from MCP into
// the daemon call, rather than assuming it did.
type Recorder struct {
	mu   sync.Mutex
	last map[string]string
}

// Last returns the trace headers seen on the most recent request.
func (r *Recorder) Last() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.last))
	for k, v := range r.last {
		out[k] = v
	}
	return out
}

func (r *Recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = map[string]string{}
	for _, h := range []string{tracectx.HeaderTraceparent, tracectx.HeaderTracestate, tracectx.HeaderBaggage} {
		if v := req.Header.Get(h); v != "" {
			r.last[h] = v
		}
	}
}

// New returns a handler serving the kmesh admin routes this PoC uses, acting
// as a daemon running in the given mode.
//
// The config dump route for the other mode answers HTTP 400 with kmesh's exact
// invalidModeErrMessage body, which is what makes Client.ResolveMode work.
func New(mode kmeshapi.Mode) http.Handler {
	h, _ := NewRecording(mode)
	return h
}

// NewRecording is New plus a Recorder over the trace headers received.
func NewRecording(mode kmeshapi.Mode) (http.Handler, *Recorder) {
	rec := &Recorder{}
	mux := http.NewServeMux()

	mux.HandleFunc(kmeshapi.RouteVersion, func(w http.ResponseWriter, r *http.Request) {
		data, err := json.MarshalIndent(&Version, "", "  ")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	configDump := func(route kmeshapi.Mode, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if mode != route {
				// checkAdsMode / checkWorkloadMode, pkg/status/status_server.go:131-149.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, kmeshapi.InvalidModeMessage)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, body)
		}
	}
	mux.HandleFunc(kmeshapi.RouteConfigDumpAds, configDump(kmeshapi.ModeKernelNative, adsDump))
	mux.HandleFunc(kmeshapi.RouteConfigDumpWorkload, configDump(kmeshapi.ModeDualEngine, workloadDump))

	mux.HandleFunc(kmeshapi.RouteLoggers, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			// The PoC is read-only; kmesh's POST branch sets log levels.
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			data, _ := json.MarshalIndent(&LoggerNames, "", "    ")
			_, _ = w.Write(data)
			return
		}
		level, ok := LoggerLevels[name]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, "\tlogger %s not found\n", name)
			return
		}
		data, _ := json.MarshalIndent(&kmeshapi.LoggerInfo{Name: name, Level: level}, "", "    ")
		_, _ = w.Write(data)
	})

	// One middleware over every route, so the recorder sees trace headers
	// regardless of which endpoint was hit.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		mux.ServeHTTP(w, r)
	}), rec
}

// NewTestServer starts a fixture daemon on an ephemeral port and returns it.
// The caller must Close it.
func NewTestServer(mode kmeshapi.Mode) *httptest.Server {
	return httptest.NewServer(New(mode))
}
