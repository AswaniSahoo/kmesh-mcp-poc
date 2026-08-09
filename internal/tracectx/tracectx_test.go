package tracectx_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/AswaniSahoo/kmesh-mcp-poc/internal/tracectx"
)

// The example from the 2026-07-28 specification's own non-normative sample of
// trace context in _meta.
const specExample = "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01"

func TestParseSpecExample(t *testing.T) {
	tc, err := tracectx.ParseTraceparent(specExample)
	if err != nil {
		t.Fatalf("the spec's own example failed to parse: %v", err)
	}
	if tc.Version != "00" {
		t.Errorf("version = %q, want 00", tc.Version)
	}
	if tc.TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("traceID = %q", tc.TraceID)
	}
	if tc.SpanID != "00f067aa0ba902b7" {
		t.Errorf("spanID = %q", tc.SpanID)
	}
	if !tc.Sampled() {
		t.Error("flags 01 should report sampled")
	}
	if got := tc.Traceparent(); got != specExample {
		t.Errorf("round trip = %q, want %q", got, specExample)
	}
}

func TestParseRejects(t *testing.T) {
	for name, in := range map[string]string{
		"empty":              "",
		"three fields":       "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7",
		"five fields":        specExample + "-extra",
		"short trace id":     "00-0af7651916cd43dd8448eb211c8031-00f067aa0ba902b7-01",
		"short span id":      "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902-01",
		"uppercase trace id": "00-0AF7651916CD43DD8448EB211C80319C-00f067aa0ba902b7-01",
		"non hex":            "00-0af7651916cd43dd8448eb211c80319z-00f067aa0ba902b7-01",
		"all zero trace id":  "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"all zero span id":   "00-0af7651916cd43dd8448eb211c80319c-0000000000000000-01",
		"unsupported ver":    "01-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tracectx.ParseTraceparent(in); err == nil {
				t.Fatalf("%q was accepted, want rejection", in)
			}
		})
	}
}

func TestSampledBit(t *testing.T) {
	for flags, want := range map[string]bool{"01": true, "00": false, "03": true, "02": false} {
		tc, err := tracectx.ParseTraceparent("00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-" + flags)
		if err != nil {
			t.Fatalf("flags %s: %v", flags, err)
		}
		if got := tc.Sampled(); got != want {
			t.Errorf("flags %s: Sampled() = %v, want %v", flags, got, want)
		}
	}
}

func TestFromMeta(t *testing.T) {
	t.Run("absent is not an error condition", func(t *testing.T) {
		_, err := tracectx.FromMeta(map[string]any{"other": "value"})
		if !errors.Is(err, tracectx.ErrAbsent) {
			t.Fatalf("want ErrAbsent, got %v", err)
		}
		if _, err := tracectx.FromMeta(nil); !errors.Is(err, tracectx.ErrAbsent) {
			t.Fatalf("nil meta: want ErrAbsent, got %v", err)
		}
	})

	t.Run("carries tracestate and baggage verbatim", func(t *testing.T) {
		tc, err := tracectx.FromMeta(map[string]any{
			tracectx.KeyTraceparent: specExample,
			tracectx.KeyTracestate:  "kmesh=abc,vendor=xyz",
			tracectx.KeyBaggage:     "userId=42,region=eu",
		})
		if err != nil {
			t.Fatal(err)
		}
		if tc.Tracestate != "kmesh=abc,vendor=xyz" {
			t.Errorf("tracestate = %q", tc.Tracestate)
		}
		if tc.Baggage != "userId=42,region=eu" {
			t.Errorf("baggage = %q", tc.Baggage)
		}
	})

	t.Run("malformed traceparent is an error", func(t *testing.T) {
		_, err := tracectx.FromMeta(map[string]any{tracectx.KeyTraceparent: "not-a-traceparent"})
		if err == nil || errors.Is(err, tracectx.ErrAbsent) {
			t.Fatalf("want a parse error, got %v", err)
		}
	})

	t.Run("non-string traceparent is an error", func(t *testing.T) {
		if _, err := tracectx.FromMeta(map[string]any{tracectx.KeyTraceparent: 42}); err == nil {
			t.Fatal("a numeric traceparent was accepted")
		}
	})
}

// TestChildKeepsTraceDropsSpan pins the propagation rule: the downstream hop
// belongs to the same trace but is a distinct span.
func TestChildKeepsTraceDropsSpan(t *testing.T) {
	parent, err := tracectx.FromMeta(map[string]any{
		tracectx.KeyTraceparent: specExample,
		tracectx.KeyTracestate:  "kmesh=abc",
		tracectx.KeyBaggage:     "userId=42",
	})
	if err != nil {
		t.Fatal(err)
	}

	child, err := parent.Child(func() (string, error) { return "aaaaaaaaaaaaaaaa", nil })
	if err != nil {
		t.Fatal(err)
	}

	if child.TraceID != parent.TraceID {
		t.Errorf("trace id changed: %q -> %q", parent.TraceID, child.TraceID)
	}
	if child.SpanID == parent.SpanID {
		t.Error("span id was not replaced; the daemon hop would look like the same span")
	}
	if child.Tracestate != parent.Tracestate || child.Baggage != parent.Baggage {
		t.Error("tracestate/baggage should be carried through unchanged")
	}
	if child.Flags != parent.Flags {
		t.Error("sampling decision should be carried through unchanged")
	}
}

func TestRandomSpanIDIsUsableAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		id, err := tracectx.RandomSpanID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 16 {
			t.Fatalf("span id %q is %d chars, want 16", id, len(id))
		}
		if strings.Trim(id, "0") == "" {
			t.Fatal("generated an all-zero span id, which W3C forbids")
		}
		if seen[id] {
			t.Fatalf("duplicate span id %q", id)
		}
		seen[id] = true
	}
}

func TestHeaders(t *testing.T) {
	tc, err := tracectx.FromMeta(map[string]any{
		tracectx.KeyTraceparent: specExample,
		tracectx.KeyBaggage:     "userId=42",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := tc.Headers()
	if h[tracectx.HeaderTraceparent] != specExample {
		t.Errorf("traceparent header = %q", h[tracectx.HeaderTraceparent])
	}
	if h[tracectx.HeaderBaggage] != "userId=42" {
		t.Errorf("baggage header = %q", h[tracectx.HeaderBaggage])
	}
	if _, ok := h[tracectx.HeaderTracestate]; ok {
		t.Error("empty tracestate should be omitted rather than sent blank")
	}
}
