package websocket

import (
	"log"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// AllowedOrigins holds the list of origins that are allowed to connect via WebSocket
// If empty, all origins are allowed (development mode)
var AllowedOrigins []string

// SetAllowedOrigins sets the allowed origins for WebSocket connections
func SetAllowedOrigins(origins []string) {
	AllowedOrigins = origins
}

// checkOrigin validates the Origin header against allowed origins
func checkOrigin(r *http.Request) bool {
	// If no allowed origins configured, allow all (development mode)
	if len(AllowedOrigins) == 0 {
		log.Printf("WARNING: WebSocket allowing all origins (development mode)")
		return true
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin header, allow the connection (e.g., non-browser clients)
		return true
	}

	// Check against allowed origins
	for _, allowed := range AllowedOrigins {
		// Support wildcard subdomains like "*.example.com"
		if strings.HasPrefix(allowed, "*.") {
			domain := allowed[2:]
			// Check if origin ends with the domain (after the scheme)
			originHost := origin
			if idx := strings.Index(origin, "://"); idx != -1 {
				originHost = origin[idx+3:]
			}
			if originHost == domain || strings.HasSuffix(originHost, "."+domain) {
				return true
			}
		}
		if origin == allowed {
			return true
		}
	}

	log.Printf("WebSocket connection rejected from origin: %s", origin)
	return false
}

// upgrader is the WebSocket upgrader
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     checkOrigin,
}

// GetUpgrader returns the websocket upgrader for testing purposes
func GetUpgrader() *websocket.Upgrader {
	return &upgrader
}
