package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tsix404/rffmpeg/pkg/protocol"
)

// contextKey is the type for context keys used by this package
type contextKey string

// ClientIDKey is the context key for the client ID
const ClientIDKey contextKey = "client_id"

// exemptPaths are paths that don't require authentication
var exemptPaths = map[string]bool{
	"/health":                   true,
	"/api/v1/health":            true,
	"/api/v1/workers/register":  true,
	"/api/v1/workers/heartbeat": true,
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
			// No token configured: reject everything (fail closed)
			if authToken == "" {
				writeUnauthorized(w, "Server has no auth token configured; refusing request")
				return
			}

			// Check if path is exempt from authentication
			if exemptPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}

			// Extract Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				writeUnauthorized(w, "Missing Authorization header")
				return
			}

			// Parse Bearer token
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
				writeUnauthorized(w, "Invalid Authorization header format")
				return
			}

			providedToken := parts[1]

			// Explicitly check for empty token (defense in depth)
			// This ensures requests with "Bearer " (empty token) are rejected
			if providedToken == "" {
				writeUnauthorized(w, "Missing token")
				return
			}

			// Validate token using constant-time comparison to prevent timing attacks
			if subtle.ConstantTimeCompare([]byte(providedToken), []byte(authToken)) != 1 {
				writeUnauthorized(w, "Invalid token")
				return
			}

			// Generate client ID from token (SHA256 first 16 hex chars)
			clientID := GenerateClientID(providedToken)

			// Inject client ID into context
			ctx := context.WithValue(r.Context(), ClientIDKey, clientID)
			r = r.WithContext(ctx)

			next.ServeHTTP(w, r)
		})
	}
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
