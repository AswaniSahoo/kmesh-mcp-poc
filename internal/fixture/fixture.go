// Package fixture stands up a stand-in for the kmesh daemon's admin server so
// the MCP server can be run and tested with no cluster, no eBPF and no root.
//
// The payloads below are HAND-AUTHORED from the exported struct shapes read in
// kmesh-net/kmesh at commit c88ef300. They are not captured from a live
// daemon. Field names and the mode-mismatch behaviour are faithful to the
// source; the values are invented. Anything that depends on the values being
// real (data volumes, timing, protojson quirks in the kernel-native dump)
// is not demonstrated by this package.
package fixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/kmeshapi"
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

// LoggerNames is the fixture's /debug/loggers payload. kmesh appends the
// "bpf" pseudo-logger to the real logger names (getLoggerNames,
// pkg/status/status_server.go), so the fixture does too.
var LoggerNames = []string{"default", "controller", "ads", "workload", "bpf"}

// LoggerLevels backs /debug/loggers?name=<name>.
var LoggerLevels = map[string]string{
	"default":    "info",
	"controller": "info",
	"ads":        "debug",
	"workload":   "info",
	"bpf":        "error",
}

// workloadDump is the dual-engine payload, shaped like status.WorkloadDump
// (pkg/status/status_server.go:466-470).
const workloadDump = `{
  "workloads": [
    {
      "uid": "Kubernetes//Pod/default/productpage-v1-fixture",
      "name": "productpage-v1-fixture",
      "namespace": "default",
      "addresses": ["10.244.0.11"],
      "node": "kmesh-worker",
      "network": "",
      "canonicalName": "productpage",
      "canonicalRevision": "v1",
      "workloadType": "Pod",
      "workloadName": "productpage-v1",
      "clusterId": "Kubernetes"
    },
    {
      "uid": "Kubernetes//Pod/default/reviews-v2-fixture",
      "name": "reviews-v2-fixture",
      "namespace": "default",
      "addresses": ["10.244.0.12"],
      "node": "kmesh-worker",
      "network": "",
      "canonicalName": "reviews",
      "canonicalRevision": "v2",
      "workloadType": "Pod",
      "workloadName": "reviews-v2",
      "clusterId": "Kubernetes"
    }
  ],
  "services": [
    {
      "name": "productpage",
      "namespace": "default",
      "hostname": "productpage.default.svc.cluster.local",
      "addresses": ["10.96.0.21"],
      "ports": [{"servicePort": 9080, "targetPort": 9080}]
    }
  ],
  "policies": [
    {
      "name": "allow-productpage",
      "namespace": "default",
      "action": "ALLOW",
      "scope": "NAMESPACE"
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

// New returns a handler serving the kmesh admin routes this PoC uses, acting
// as a daemon running in the given mode.
//
// The config dump route for the other mode answers HTTP 400 with kmesh's exact
// invalidModeErrMessage body, which is what makes Client.ResolveMode work.
func New(mode kmeshapi.Mode) http.Handler {
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

	return mux
}

// NewTestServer starts a fixture daemon on an ephemeral port and returns it.
// The caller must Close it.
func NewTestServer(mode kmeshapi.Mode) *httptest.Server {
	return httptest.NewServer(New(mode))
}
