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
	"net/http"
	"os"
	"reflect"
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
// node's own eBPF maps through Processor.GetBpfCache(), so it can only ever
// describe the node it was asked on.
//
// The comparison is per section and by multiset, not by digest. That matters:
// these arrays come out of Go maps, so their order is not stable, and a digest
// comparison reports "different" for two documents holding identical contents.
// Only a difference in what the sections contain says anything about scope.
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

	secA, secB := sections(t, bodyA), sections(t, bodyB)
	names := map[string]bool{}
	for k := range secA {
		names[k] = true
	}
	for k := range secB {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	contentDiffs := 0
	orderOnly := 0
	for _, name := range sorted {
		countA, countB := tally(secA[name]), tally(secB[name])
		if reflect.DeepEqual(countA, countB) {
			if reflect.DeepEqual(secA[name], secB[name]) {
				t.Logf("  %-16s identical (%d entries)", name, len(secA[name]))
			} else {
				orderOnly++
				t.Logf("  %-16s same contents, different order (%d entries)", name, len(secA[name]))
			}
			continue
		}
		contentDiffs++
		t.Logf("  %-16s CONTENT DIFFERS", name)
		for entry, nA := range countA {
			if nB := countB[entry]; nB != nA {
				t.Logf("      A x%d  B x%d   %s", nA, nB, truncate(entry, 120))
			}
		}
		for entry, nB := range countB {
			if _, seen := countA[entry]; !seen {
				t.Logf("      A x0  B x%d   %s", nB, truncate(entry, 120))
			}
		}
	}

	t.Logf("sections differing in content: %d, differing only in order: %d", contentDiffs, orderOnly)
	if contentDiffs > 0 {
		t.Log("=> the two nodes' eBPF state is genuinely different, so a get_bpf_maps tool")
		t.Log("   answering without a pod_name would be describing one node and not saying which")
	} else {
		t.Log("=> on this cluster the two nodes' eBPF state happens to match; the route is still")
		t.Log("   node-scoped by construction, this cluster is just too small to show it")
	}
	t.Log("=> either way the arrays are not order-stable, so a client must not diff raw responses")
	t.Log("=> note: the backend map is keyed by BackendUid (bpfcache/backend.go:29-31) but")
	t.Log("   WithBackends serialises values only (pkg/status/api.go:266-291), so two distinct")
	t.Log("   entries sharing an ip render identically and cannot be told apart by a client,")
	t.Log("   nor correlated with endpoints, which do carry backendUid")
}

// sections splits a bpf dump into its top-level arrays, each entry rendered as
// canonical JSON so entries can be compared and counted.
func sections(t *testing.T, body []byte) map[string][]string {
	t.Helper()
	var raw map[string][]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("bpf dump is not an object of arrays: %v", err)
	}
	out := map[string][]string{}
	for name, entries := range raw {
		for _, e := range entries {
			var v any
			if err := json.Unmarshal(e, &v); err != nil {
				t.Fatalf("entry in %s: %v", name, err)
			}
			canonical, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("re-marshal entry in %s: %v", name, err)
			}
			out[name] = append(out[name], string(canonical))
		}
	}
	return out
}

// tally counts each distinct entry. Duplicates are meaningful here: the maps
// really do hold the same value more than once.
func tally(entries []string) map[string]int {
	c := map[string]int{}
	for _, e := range entries {
		c[e]++
	}
	return c
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
