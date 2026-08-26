//go:build live

// Multi-daemon tests. kmesh runs as a DaemonSet, one daemon per node, and eight
// of the fifteen tools in kmesh-net/kmesh#1800 take an optional pod_name. That
// leaves an open contract question: what does a tool with no pod_name mean on a
// cluster with N nodes?
//
// This file answers it by measurement rather than by argument. Two routes on the
// same daemon have different scope:
//
//	/debug/config_dump/dual-engine       xDS view, pushed by istiod
//	/debug/config_dump/bpf/dual-engine   this node's live eBPF maps, via
//	                                     Processor.GetBpfCache()
//	                                     (pkg/status/status_server.go:151-157)
//
// If every daemon converges on the same xDS view, a tool built on the first
// route does not need pod targeting. The second route can only ever describe the
// node it was asked on.
//
// KMESH_DAEMON_ADDR and KMESH_DAEMON_ADDR_2 must point at two different daemons.
// Without the second, these skip.
package mcpserver_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"
)

// Routes this proof of concept does not expose as tools. Declared here rather
// than in internal/kmeshapi so the package keeps its property of touching no
// eBPF-backed route; these are read only to measure their scope.
const (
	routeBpfDumpWorkload = "/debug/config_dump/bpf/dual-engine"   // patternBpfWorkloadMaps
	routeBpfDumpAds      = "/debug/config_dump/bpf/kernel-native" // patternBpfAdsMaps
)

// secondAddr returns the other daemon, or skips.
func secondAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("KMESH_DAEMON_ADDR_2")
	if addr == "" {
		t.Skip("KMESH_DAEMON_ADDR_2 not set; need a second daemon to compare against")
	}
	return addr
}

// digest is a stable fingerprint of a JSON document, independent of key order.
func digest(t *testing.T, body []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	canonical, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:8])
}

// meshView pulls the identifying sets out of a dual-engine config dump: every
// workload uid and every service hostname. Those are what a caller would act on,
// so they are what has to agree.
func meshView(t *testing.T, body []byte) (workloads, services []string, byNode map[string]int) {
	t.Helper()
	var dump struct {
		Workloads []struct {
			UID  string `json:"uid"`
			Node string `json:"node"`
		} `json:"workloads"`
		Services []struct {
			Hostname string `json:"hostname"`
		} `json:"services"`
	}
	if err := json.Unmarshal(body, &dump); err != nil {
		t.Fatalf("decode config dump: %v", err)
	}
	byNode = map[string]int{}
	for _, w := range dump.Workloads {
		workloads = append(workloads, w.UID)
		n := w.Node
		if n == "" {
			n = "<none>"
		}
		byNode[n]++
	}
	for _, s := range dump.Services {
		services = append(services, s.Hostname)
	}
	sort.Strings(workloads)
	sort.Strings(services)
	return workloads, services, byNode
}

func symmetricDiff(a, b []string) (onlyA, onlyB []string) {
	inB := map[string]bool{}
	for _, s := range b {
		inB[s] = true
	}
	inA := map[string]bool{}
	for _, s := range a {
		inA[s] = true
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return onlyA, onlyB
}

// TestLiveMeshViewAgreesAcrossDaemons checks whether every daemon reports the
// same mesh, which is what decides if config_dump needs pod targeting at all.
//
// xDS is push-based, so two daemons can be momentarily out of step. The retry
// window separates "these converge" from "these are scoped differently"; a
// scope difference will never converge no matter how long it is given.
func TestLiveMeshViewAgreesAcrossDaemons(t *testing.T) {
	a, b := liveAddr(t), secondAddr(t)
	route := "/debug/config_dump/dual-engine"

	deadline := time.Now().Add(90 * time.Second)
	for attempt := 1; ; attempt++ {
		bodyA, codeA := rawGet(t, a, route)
		bodyB, codeB := rawGet(t, b, route)
		if codeA != http.StatusOK || codeB != http.StatusOK {
			t.Fatalf("config dump: daemon A %d, daemon B %d", codeA, codeB)
		}

		wA, sA, nA := meshView(t, bodyA)
		wB, sB, nB := meshView(t, bodyB)

		wOnlyA, wOnlyB := symmetricDiff(wA, wB)
		sOnlyA, sOnlyB := symmetricDiff(sA, sB)

		if len(wOnlyA) == 0 && len(wOnlyB) == 0 && len(sOnlyA) == 0 && len(sOnlyB) == 0 {
			capture(t, "meshview_daemon_a.json", bodyA)
			capture(t, "meshview_daemon_b.json", bodyB)
			t.Logf("attempt %d: the two daemons report the SAME mesh view", attempt)
			t.Logf("  workloads: %d   services: %d", len(wA), len(sA))
			t.Logf("  daemon A groups those workloads by node as %v", nA)
			t.Logf("  daemon B groups those workloads by node as %v", nB)
			t.Logf("  document digests: A=%s B=%s", digest(t, bodyA), digest(t, bodyB))
			t.Log("  => a config_dump tool called with no pod_name can be answered by any daemon")
			return
		}

		if time.Now().After(deadline) {
			capture(t, "meshview_daemon_a.json", bodyA)
			capture(t, "meshview_daemon_b.json", bodyB)
			t.Errorf("the two daemons never converged on the same mesh view after %d attempts", attempt)
			t.Errorf("  workloads only on A (%d): %v", len(wOnlyA), wOnlyA)
			t.Errorf("  workloads only on B (%d): %v", len(wOnlyB), wOnlyB)
			t.Errorf("  services only on A (%d): %v", len(sOnlyA), sOnlyA)
			t.Errorf("  services only on B (%d): %v", len(sOnlyB), sOnlyB)
			t.Errorf("  => config_dump IS node-scoped, so a tool without pod_name is ambiguous")
			return
		}
		time.Sleep(3 * time.Second)
	}
}

// TestLiveBpfViewIsPerDaemon measures the other route. This one reads the
// node's own eBPF maps, so it describes one node by construction. The test
// asserts only that both daemons answer; whether the documents differ is the
// measurement, and it is reported either way rather than assumed.
func TestLiveBpfViewIsPerDaemon(t *testing.T) {
	a, b := liveAddr(t), secondAddr(t)

	bodyA, codeA := rawGet(t, a, routeBpfDumpWorkload)
	bodyB, codeB := rawGet(t, b, routeBpfDumpWorkload)
	capture(t, "bpfview_daemon_a.json", bodyA)
	capture(t, "bpfview_daemon_b.json", bodyB)

	t.Logf("GET %s -> A:%d (%d bytes)  B:%d (%d bytes)",
		routeBpfDumpWorkload, codeA, len(bodyA), codeB, len(bodyB))

	if codeA != http.StatusOK || codeB != http.StatusOK {
		t.Fatalf("both daemons must serve the bpf dump in dual-engine mode; got A=%d B=%d", codeA, codeB)
	}

	dA, dB := digest(t, bodyA), digest(t, bodyB)
	t.Logf("digests: A=%s  B=%s", dA, dB)

	summarise := func(tag string, body []byte) {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Logf("  %s: not a JSON object (%v)", tag, err)
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if arr, ok := m[k].([]any); ok {
				t.Logf("  %s %s: %d entries", tag, k, len(arr))
			} else {
				t.Logf("  %s %s: %s", tag, k, fmt.Sprintf("%T", m[k]))
			}
		}
	}
	summarise("A", bodyA)
	summarise("B", bodyB)

	if dA == dB {
		t.Log("=> the two nodes' eBPF maps happen to be identical on this cluster")
		t.Log("   (a two node kind cluster is small; this does not make the route node-independent)")
	} else {
		t.Log("=> the two nodes' eBPF maps differ, so a get_bpf_maps tool must name its node")
	}
}
