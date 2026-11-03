package handlers

import (
	"net/http"
	"strings"
)

// maxJSONBodyBytes is the request-body cap for JSON endpoints. It bounds the
// memory/CPU a client can make the server spend parsing JSON.
const maxJSONBodyBytes int64 = 10 * 1024 * 1024

// MaxBodyBytesMiddleware caps JSON request bodies at maxJSONBodyBytes via
// MaxBytesReader.
//
// Which requests are exempt is decided from the route, never from the request
// headers: an earlier variant skipped the cap whenever Content-Type started
// with "multipart/", which let any token holder send an arbitrarily large body
// to a JSON endpoint just by labelling it multipart. The only routes that may
// exceed the cap are the upload handlers, which install their own (larger)
// MaxBytesReader before parsing, so the JSON cap must not wrap them first.
func MaxBodyBytesMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && !hasOwnBodyLimit(r) {
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// hasOwnBodyLimit reports whether the request targets an endpoint that reads a
// multipart body under its own limit. The paths mirror the routes registered
// for the three multipart endpoints:
//   - POST /api/v1/upload
//   - POST /api/v1/upload/chunk/{uploadId}/{chunkIndex}
//   - POST /api/v1/jobs/{jobId}/output
func hasOwnBodyLimit(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}

	switch {
	case r.URL.Path == "/api/v1/upload":
		return true
	case strings.HasPrefix(r.URL.Path, "/api/v1/upload/chunk/"):
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/upload/chunk/")
		parts := strings.Split(rest, "/")
		return len(parts) == 2 && parts[0] != "" && parts[1] != ""
	case strings.HasPrefix(r.URL.Path, "/api/v1/jobs/") && strings.HasSuffix(r.URL.Path, "/output"):
		jobID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/"), "/output")
		return jobID != "" && !strings.Contains(jobID, "/")
	}
	return false
}
