package main

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
	"github.com/tsic404/rffmpeg/pkg/server/workerhealth"
)

// TestOpenAPIMatchesRoutes keeps api/openapi.yaml in step with the mounted
// route table. The spec drifted because nothing compared the two; walking the
// real router makes any added, removed or renamed endpoint fail here.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.New(filepath.Join(dataDir, "rffmpeg.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	store, err := storage.New(dataDir)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	h := handlers.New(database, store, "test", workerhealth.NewWorkerStateTable(30*time.Second))
	chunkHandler := handlers.NewChunkUploadHandler(database, store, 0)

	r := chi.NewRouter()
	registerRoutes(r, h, chunkHandler, &ratelimit.RuntimeConfig{Enabled: false, Limit: 10})

	routes := map[string]bool{}
	err = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// The root health probe is an alias for /api/v1/health and is not
		// part of the documented API surface.
		if route == "/health" {
			return nil
		}
		routes[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}

	spec := readOpenAPIRoutes(t, filepath.Join("..", "..", "api", "openapi.yaml"))

	var undocumented, unimplemented []string
	for key := range routes {
		if !spec[key] {
			undocumented = append(undocumented, key)
		}
	}
	for key := range spec {
		if !routes[key] {
			unimplemented = append(unimplemented, key)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(unimplemented)
	if len(undocumented) > 0 {
		t.Errorf("routes missing from api/openapi.yaml: %v", undocumented)
	}
	if len(unimplemented) > 0 {
		t.Errorf("documented but not routed: %v", unimplemented)
	}
}

// readOpenAPIRoutes extracts "METHOD /full/path" keys from the spec's paths
// section. The file is hand-maintained and the parser only needs the two-space
// path keys and four-space method keys under `paths:`, so no YAML dependency is
// pulled into the module for one test.
func readOpenAPIRoutes(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open spec: %v", err)
	}
	defer f.Close()

	known := map[string]bool{
		"get": true, "post": true, "put": true, "patch": true, "delete": true,
		"head": true, "options": true, "trace": true,
	}

	type specOperation struct {
		path       string
		method     string
		hasServers bool
	}

	var pending []specOperation
	var current string
	currentHasServers := false
	inPaths := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "paths:" {
			inPaths = true
			continue
		}
		if inPaths && line == "components:" {
			break
		}
		if !inPaths {
			continue
		}
		switch {
		case strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			current = strings.TrimSuffix(strings.TrimSpace(line), ":")
			currentHasServers = false
		case line == "    servers:":
			// A path-level server override means the path is NOT relative to
			// the document's /api/v1 server. The spec marks the /admin family
			// this way; inferring it from a literal path prefix would hide a
			// real mismatch if the prefix changed.
			currentHasServers = true
		case strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") && strings.HasSuffix(line, ":"):
			method := strings.TrimSuffix(strings.TrimSpace(line), ":")
			if current == "" || !known[method] {
				continue
			}
			pending = append(pending, specOperation{path: current, method: strings.ToUpper(method), hasServers: currentHasServers})
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read spec: %v", err)
	}

	routes := map[string]bool{}
	for _, op := range pending {
		full := op.path
		if !op.hasServers {
			full = "/api/v1" + full
		}
		routes[op.method+" "+full] = true
	}
	return routes
}
