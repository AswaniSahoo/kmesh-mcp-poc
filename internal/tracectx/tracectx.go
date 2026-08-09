// Package tracectx carries W3C trace context from an MCP request's _meta
// through to the kmesh daemon.
//
// Why this exists. Protocol revision 2026-07-28 documents OpenTelemetry trace
// context propagation over MCP (SEP-414, Final): the keys traceparent,
// tracestate and baggage are reserved in _meta, as an explicit exception to
// the io.modelcontextprotocol/ prefix rule, and their values MUST follow
// W3C Trace Context and W3C Baggage. The same revision deprecates MCP Logging
// and points implementations at OpenTelemetry instead (SEP-2577).
//
// For most servers that is a footnote. For a service mesh it is the whole
// point. kmesh exists to make service-to-service calls observable; if an agent
// calls kmesh_config_dump and then some other tool, those calls should land in
// the same trace as the traffic they describe. That only happens if the trace
// context survives the hop from MCP into the daemon's own HTTP surface, which
// is what this package does.
//
// The go-sdk has no built-in helper for these keys as of v1.7.0, so the
// parsing and the propagation are both implemented here.
package tracectx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Reserved _meta keys, from the reserved-keys table in the 2026-07-28
// specification (basic/index). These three deliberately carry no vendor
// prefix; the spec calls that out as an exception made to stay compatible
// with the OpenTelemetry semantic conventions for MCP.
const (
	KeyTraceparent = "traceparent"
	KeyTracestate  = "tracestate"
	KeyBaggage     = "baggage"
)

// HeaderTraceparent and friends are the W3C header names used on the outbound
// hop to the kmesh daemon. Same values, different carrier.
const (
	HeaderTraceparent = "traceparent"
	HeaderTracestate  = "tracestate"
	HeaderBaggage     = "baggage"
)

// SupportedVersion is the only traceparent version W3C currently defines.
const SupportedVersion = "00"

var (
	// ErrAbsent reports that the request carried no trace context at all. This
	// is not a failure: trace context is optional.
	ErrAbsent = errors.New("no trace context present")

	errMalformed = errors.New("malformed traceparent")
)

// Context is a parsed W3C trace context.
type Context struct {
	// Version is the traceparent version field, "00" today.
	Version string
	// TraceID is the 32 hex digit trace identifier, shared by every span in
	// the trace. This is the field that must survive the hop into kmesh.
	TraceID string
	// SpanID is the 16 hex digit identifier of the caller's span, which
	// becomes the parent of anything this server does.
	SpanID string
	// Flags is the 2 hex digit trace-flags field.
	Flags string
	// Tracestate and Baggage are carried verbatim. Their internal structure is
	// vendor-defined and this package does not interpret it.
	Tracestate string
	Baggage    string
}

// Sampled reports whether the caller set the sampled bit (flags & 0x01).
func (c *Context) Sampled() bool {
	b, err := hex.DecodeString(c.Flags)
	if err != nil || len(b) != 1 {
		return false
	}
	return b[0]&0x01 == 1
}

// Traceparent renders the context back into a traceparent value.
func (c *Context) Traceparent() string {
	return strings.Join([]string{c.Version, c.TraceID, c.SpanID, c.Flags}, "-")
}

// ParseTraceparent parses a W3C traceparent value.
//
// Validation follows the W3C Trace Context rules the spec points at: four
// hyphen-separated fields, all lowercase hex, with an all-zero trace-id or
// span-id rejected as invalid.
func ParseTraceparent(s string) (*Context, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 4 {
		return nil, fmt.Errorf("%w: want 4 hyphen-separated fields, got %d", errMalformed, len(parts))
	}
	version, traceID, spanID, flags := parts[0], parts[1], parts[2], parts[3]

	if err := checkHex("version", version, 2); err != nil {
		return nil, err
	}
	if version != SupportedVersion {
		return nil, fmt.Errorf("%w: unsupported version %q", errMalformed, version)
	}
	if err := checkHex("trace-id", traceID, 32); err != nil {
		return nil, err
	}
	if isAllZero(traceID) {
		return nil, fmt.Errorf("%w: trace-id is all zeros", errMalformed)
	}
	if err := checkHex("parent-id", spanID, 16); err != nil {
		return nil, err
	}
	if isAllZero(spanID) {
		return nil, fmt.Errorf("%w: parent-id is all zeros", errMalformed)
	}
	if err := checkHex("trace-flags", flags, 2); err != nil {
		return nil, err
	}

	return &Context{Version: version, TraceID: traceID, SpanID: spanID, Flags: flags}, nil
}

func checkHex(field, s string, want int) error {
	if len(s) != want {
		return fmt.Errorf("%w: %s must be %d hex digits, got %d", errMalformed, field, want, len(s))
	}
	if s != strings.ToLower(s) {
		return fmt.Errorf("%w: %s must be lowercase hex", errMalformed, field)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return fmt.Errorf("%w: %s is not hex", errMalformed, field)
	}
	return nil
}

func isAllZero(s string) bool { return strings.Trim(s, "0") == "" }

// FromMeta extracts trace context from an MCP request's _meta map.
//
// Returns ErrAbsent when no traceparent key is present, which callers should
// treat as "this request is not traced" rather than as an error. A traceparent
// that is present but malformed is a real error: silently dropping it would
// break the trace without anyone noticing.
func FromMeta(meta map[string]any) (*Context, error) {
	if meta == nil {
		return nil, ErrAbsent
	}
	raw, ok := meta[KeyTraceparent]
	if !ok {
		return nil, ErrAbsent
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("%w: traceparent must be a string, got %T", errMalformed, raw)
	}
	tc, err := ParseTraceparent(s)
	if err != nil {
		return nil, err
	}
	if v, ok := meta[KeyTracestate].(string); ok {
		tc.Tracestate = v
	}
	if v, ok := meta[KeyBaggage].(string); ok {
		tc.Baggage = v
	}
	return tc, nil
}

// Child returns the trace context this server should send onward to the kmesh
// daemon: same trace, same flags, same tracestate and baggage, but a fresh
// span id so the daemon call is a child of the tool call rather than a
// duplicate of it.
//
// newSpanID is injectable so tests are deterministic. Pass nil for a
// crypto/rand span id.
func (c *Context) Child(newSpanID func() (string, error)) (*Context, error) {
	if newSpanID == nil {
		newSpanID = RandomSpanID
	}
	span, err := newSpanID()
	if err != nil {
		return nil, fmt.Errorf("minting span id: %w", err)
	}
	if err := checkHex("parent-id", span, 16); err != nil {
		return nil, err
	}
	child := *c
	child.SpanID = span
	return &child, nil
}

// RandomSpanID returns a random 16 hex digit span id.
func RandomSpanID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Header names and values to set on an outbound HTTP request.
func (c *Context) Headers() map[string]string {
	h := map[string]string{HeaderTraceparent: c.Traceparent()}
	if c.Tracestate != "" {
		h[HeaderTracestate] = c.Tracestate
	}
	if c.Baggage != "" {
		h[HeaderBaggage] = c.Baggage
	}
	return h
}

type ctxKey struct{}

// NewContext returns ctx carrying tc.
func NewContext(ctx context.Context, tc *Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, tc)
}

// FromContext returns the trace context carried by ctx, or nil.
func FromContext(ctx context.Context) *Context {
	tc, _ := ctx.Value(ctxKey{}).(*Context)
	return tc
}
