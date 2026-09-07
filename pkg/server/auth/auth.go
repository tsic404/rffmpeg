package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// contextKey is the type for context keys used by this package
type contextKey string

// ClientIDKey is the context key for the client ID
const ClientIDKey contextKey = "client_id"

// exemptPaths are paths that don't require authentication.
// Only health probe endpoints are exempt. Worker registration and heartbeat
// require authentication: an unauthenticated register/heartbeat lets any
// attacker inject ghost workers and poison the scheduler.
var exemptPaths = map[string]bool{
	"/health":        true,
	"/api/v1/health": true,
}

// Middleware creates an authentication middleware for the given PSK token.
// An empty token disables authentication entirely: every request is rejected,
// including those that carry no Authorization header at all. This is a secure
// default — an unauthenticated API must fail closed, never fall back to
// accepting traffic. Deployments that intentionally want an open API must
// configure a token on both sides (server --auth-token, client RFFMPEG_TOKEN).
func Middleware(authToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check if path is exempt from authentication. This runs BEFORE
			// the fail-closed check so tokenless deployments can still serve
			// load-balancer health probes on /health.
			if exemptPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}

			// Validate Bearer token (single shared implementation)
			clientID, ok := ValidateBearer(w, r, authToken)
			if !ok {
				// ValidateBearer already wrote the response
				return
			}

			// Inject client ID into context
			ctx := context.WithValue(r.Context(), ClientIDKey, clientID)
			r = r.WithContext(ctx)

			next.ServeHTTP(w, r)
		})
	}
}

// ValidateBearer extracts and validates the Authorization header against the
// configured PSK token. Returns the client ID derived from the token and true
// when valid. When validation fails it writes a 401 response and returns false.
// An empty authToken rejects every request (fail closed).
func ValidateBearer(w http.ResponseWriter, r *http.Request, authToken string) (string, bool) {
	// No token configured: reject everything (fail closed)
	if authToken == "" {
		writeUnauthorized(w, "Server has no auth token configured; refusing request")
		return "", false
	}

	// Extract Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		writeUnauthorized(w, "Missing Authorization header")
		return "", false
	}

	// Parse Bearer token
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		writeUnauthorized(w, "Invalid Authorization header format")
		return "", false
	}

	providedToken := parts[1]

	// Explicitly check for empty token (defense in depth)
	// This ensures requests with "Bearer " (empty token) are rejected
	if providedToken == "" {
		writeUnauthorized(w, "Missing token")
		return "", false
	}

	// Validate token using constant-time comparison to prevent timing attacks
	if subtle.ConstantTimeCompare([]byte(providedToken), []byte(authToken)) != 1 {
		writeUnauthorized(w, "Invalid token")
		return "", false
	}

	return GenerateClientID(providedToken), true
}

// GenerateClientID generates a client ID from a token using SHA256
func GenerateClientID(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])[:16]
}

// writeUnauthorized writes a 401 Unauthorized response
func writeUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// Use protocol error format for consistency
	err := protocol.NewProtocolError(protocol.ErrCodeUnauthorized, message, nil)
	json.NewEncoder(w).Encode(err.ToResponse())
}

// GetClientID extracts the client ID from the request context.
// Returns an empty string if not found.
func GetClientID(r *http.Request) string {
	if clientID, ok := r.Context().Value(ClientIDKey).(string); ok {
		return clientID
	}
	return ""
}

// maxJSONBodyBytes is the global request-body cap for JSON endpoints. It
// bounds memory/CPU spent parsing unauthenticated input; multipart upload
// endpoints apply their own (larger) MaxBytesReader before this matters.
const maxJSONBodyBytes int64 = 10 * 1024 * 1024

// MaxBodyBytesMiddleware caps JSON request bodies at 10MB via MaxBytesReader.
// Multipart requests are exempt: upload handlers apply their own larger
// MaxBytesReader sized to the actual payload limits.
func MaxBodyBytesMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}
