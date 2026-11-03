package main

import (
	"github.com/go-chi/chi/v5"

	"github.com/tsic404/rffmpeg/pkg/server/handlers"
	"github.com/tsic404/rffmpeg/pkg/server/ratelimit"
)

// registerRoutes mounts the API surface. It is separate from main so the route
// table can be walked by tests and compared with api/openapi.yaml — the
// specification drifted because the routes were only reachable through main.
func registerRoutes(r chi.Router, h *handlers.Handler, chunkHandler *handlers.ChunkUploadHandler, rateLimitCfg *ratelimit.RuntimeConfig) {
	r.Route("/api/v1", func(r chi.Router) {
		// File upload (simple)
		r.Post("/upload", h.Upload)

		// Chunked upload
		r.Post("/upload/init", chunkHandler.InitChunkUpload)
		r.Post("/upload/chunk/{uploadId}/{chunkIndex}", chunkHandler.UploadChunk)
		r.Get("/upload/progress/{uploadId}", chunkHandler.GetUploadProgress)
		r.Post("/upload/complete", chunkHandler.CompleteChunkUpload)
		r.Post("/upload/cancel/{uploadId}", chunkHandler.CancelChunkUpload)
		r.Get("/upload/resume/{uploadId}", chunkHandler.ResumeChunkUpload)

		// Jobs
		r.Group(func(r chi.Router) {
			r.Use(ratelimit.JobSubmitMiddleware(h.GetRateLimiter(), rateLimitCfg))
			r.Post("/jobs", h.SubmitJob)
		})
		r.Get("/jobs", h.ListJobs)
		r.Get("/jobs/{jobId}", h.GetJob)
		r.Delete("/jobs/{jobId}", h.CancelJob)
		r.Patch("/jobs/{jobId}", h.UpdateJob)
		r.Post("/jobs/{jobId}/output", h.UploadJobOutput)
		r.Get("/jobs/{jobId}/log", h.GetJobLogWS) // WebSocket endpoint for real-time logs

		// Output download
		r.Get("/output/{fileId}", h.DownloadOutput)

		// File download (for workers)
		r.Get("/files/{fileId}", h.DownloadFile)

		// Workers
		r.Post("/workers/register", h.RegisterWorker)
		r.Post("/workers/heartbeat", h.WorkerHeartbeat)
		r.Get("/workers/{workerId}/jobs", h.PullWorkerJobs)
		r.Get("/workers", h.ListWorkers)
		r.Get("/workers/{workerId}", h.GetWorker)

		// Encoders (capability query)
		r.Get("/encoders", h.ListAllEncoders)
		r.Get("/encoders/{encoder}/workers", h.ListWorkersByEncoder)

		// Decoders (capability query)
		r.Get("/decoders", h.ListAllDecoders)

		// Info flags (capability query — ffmpeg -codecs/-filters/-pix_fmts/-formats equivalent)
		r.Get("/codecs", h.ListAllCodecs)
		r.Get("/filters", h.ListAllFilters)
		r.Get("/pix_fmts", h.ListAllPixFmts)
		r.Get("/formats", h.ListAllFormats)

		// Hwaccels (P0 info flag aggregation)
		r.Get("/hwaccels", h.ListAllHwaccels)

		// Health check
		r.Get("/health", h.Health)

		// Migration audit events
		r.Get("/migrations", h.ListMigrationEvents)
		r.Get("/migrations/{eventId}", h.GetMigrationEvent)

		// Probe (ffprobe sync endpoint) — behind the job-submission rate
		// limiter: a probe dispatches real work, so an unthrottled client can
		// starve the queue just like unbounded job submissions. The probe
		// variant releases its slot when the handler returns (no job mapping
		// exists for the completion callback), so the reservation lasts exactly
		// one request.
		r.Group(func(r chi.Router) {
			r.Use(ratelimit.ProbeMiddleware(h.GetRateLimiter(), rateLimitCfg))
			r.Post("/probe", h.Probe)
		})
	})

	// Admin routes (outside /api/v1, separate prefix)
	r.Route("/admin", func(r chi.Router) {
		adminHandler := handlers.NewAdminHandler(h, rateLimitCfg)
		r.Get("/config", adminHandler.GetConfig)
		r.Patch("/config", adminHandler.UpdateConfig)
		r.Get("/metrics", adminHandler.GetMetrics)
		// Single-object eviction release: the only exit for a worker whose
		// throughput recovery is unreachable (post-restart, idle).
		r.Post("/workers/eviction/clear", adminHandler.ClearWorkerEviction)
	})

	// Root-level health check for monitoring tools (load balancers, orchestrators)
	r.Get("/health", h.Health)
	// Protocol-consistent JSON 404 for any unmatched path, so a trailing empty
	// segment (e.g. /api/v1/migrations/) returns the same {"code":"not_found"}
	// structure as resource-specific 404s (e.g. GetMigrationEvent).
	r.NotFound(handlers.NotFound)
}
