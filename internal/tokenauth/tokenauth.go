// Package tokenauth holds the bearer-token seam.
//
// What is real here is the wiring: the SDK's auth middleware sits in front of
// the MCP handler, calls a TokenVerifier per request, and rejects anything it
// does not like before the transport sees it. What is NOT real is the
// verifier. StubVerifier compares against a fixed string. A production build
// would replace exactly this function with a Kubernetes TokenReview call, and
// nothing else in the server would change — kmesh's daemon already
// authenticates with service account tokens, which are bearer JWTs.
package tokenauth

import (
	"context"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Scope is the scope the stub verifier grants. The tools are read-only.
const Scope = "kmesh:read"

// StubVerifier returns a TokenVerifier that accepts exactly one token value.
//
// The returned TokenInfo carries an Expiration because the SDK's
// RequireBearerToken middleware rejects a TokenInfo with a zero Expiration
// unless AllowMissingExpiration is set. A TokenReview-backed verifier would
// populate this from the token's own exp claim.
func StubVerifier(want string) auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if want == "" || token != want {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{
			Scopes:     []string{Scope},
			Expiration: time.Now().Add(time.Hour),
			UserID:     "system:serviceaccount:kmesh-system:kmesh-mcp-fixture",
		}, nil
	}
}
