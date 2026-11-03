package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tsic404/rffmpeg/pkg/pathutil"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/storage"
)

// inputExists reports whether a probe input resolves to a registered storage
// file. Only registered inputs are probed: a bare path or arbitrary string must
// not turn the endpoint into a filesystem existence oracle on the server host.
// Remote URLs and shared-FS paths return true and are resolved by the Worker.
func inputExists(store *storage.Storage, input string) bool {
	if storage.ValidateFileID(input) {
		return store.FileExists(input)
	}

	if pathutil.ContainsPathTraversal(input) {
		return false
	}
	return true
}

// Probe handles ffprobe requests via job dispatch to workers.
// POST /api/v1/probe
//
// The rate-limit slot this request occupies is reserved and released by
// ratelimit.ProbeMiddleware around this handler, so the handler keeps no
// counter bookkeeping of its own.
func (h *Handler) Probe(w http.ResponseWriter, r *http.Request) {

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req protocol.ProbeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "Invalid JSON body", err,
		))
		return
	}
	if req.Input == "" {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeInvalidRequest, "input is required", nil,
		))
		return
	}

	if !inputExists(h.storage, req.Input) {
		writeError(w, http.StatusBadRequest, protocol.NewProtocolError(
			protocol.ErrCodeNotFound, "Input file not found: "+req.Input, nil,
		))
		return
	}

	liveWorkers, err := h.db.GetLiveSchedulableWorkers(h.heartbeatTimeout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to check worker availability", err,
		))
		return
	}
	if len(liveWorkers) == 0 {
		writeError(w, http.StatusServiceUnavailable, protocol.NewProtocolError(
			protocol.ErrCodeWorkerUnavailable,
			"No workers available. Please ensure at least one worker is registered and online.",
			nil,
		))
		return
	}

	inputFilesJSON, err := json.Marshal([]string{req.Input})
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to marshal input files", err,
		))
		return
	}
	argsJSON, err := json.Marshal([]string{"__rffmpeg_probe__", req.Input})
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to marshal args", err,
		))
		return
	}

	directPathsJSON := "[]"
	if !strings.Contains(req.Input, "://") && !storage.ValidateFileID(req.Input) {
		dp, err := json.Marshal([]string{req.Input})
		if err != nil {
			writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
				protocol.ErrCodeInternalError, "Failed to marshal direct path", err,
			))
			return
		}
		directPathsJSON = string(dp)
	}

	timeout := 60 * time.Second
	job, err := h.db.CreateJobWithStreaming(string(inputFilesJSON), string(argsJSON), "probe_result.json", false, false, &timeout, directPathsJSON)
	if err != nil {
		writeCreateJobError(w, err)
		return
	}

	// Wait for the probe job to reach a terminal state. A per-job notifier
	// (woken by every terminal-status DB write) replaces the old 2-second
	// poll loop, so terminal-state propagation is immediate instead of up to
	// 2s late. The overall budget mirrors the previous 60x2s loop
	// (~2 minutes), but the probe job itself times out after 60s, so the
	// extra headroom only matters for a late report racing the deadline.
	const probeWaitBudget = 2 * time.Minute
	finalJob, err := h.waitForProbeTerminal(r.Context(), job.ID, probeWaitBudget)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get job status", err,
		))
		return
	}
	if finalJob == nil {

		writeJSON(w, http.StatusGatewayTimeout, protocol.ProbeResponse{
			Error:   "timeout",
			Message: "Probe job did not complete within the timeout period",
		})
		return
	}

	if finalJob.Status == protocol.JobStatusFailed {
		errMsg := ""
		if finalJob.Error.Valid {
			errMsg = finalJob.Error.String
		}
		writeJSON(w, http.StatusInternalServerError, protocol.ProbeResponse{
			Error:   "probe_failed",
			Message: errMsg,
		})
		return
	}
	if finalJob.Status == protocol.JobStatusTimeout {
		writeJSON(w, http.StatusGatewayTimeout, protocol.ProbeResponse{
			Error:   "timeout",
			Message: "Probe job timed out",
		})
		return
	}
	if finalJob.Status == protocol.JobStatusCancelled {
		writeJSON(w, http.StatusOK, protocol.ProbeResponse{
			Error:   "cancelled",
			Message: "Probe job was cancelled",
		})
		return
	}

	// Read probe result from output file
	var outputFiles []string
	if err := json.Unmarshal([]byte(finalJob.OutputFiles), &outputFiles); err != nil || len(outputFiles) == 0 {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "No output file from probe job", err,
		))
		return
	}

	file, err := h.storage.OpenOutput(outputFiles[0])
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to open probe output", err,
		))
		return
	}
	defer file.Close()

	var ffprobeResult struct {
		Format  map[string]interface{}   `json:"format,omitempty"`
		Streams []map[string]interface{} `json:"streams,omitempty"`
	}
	if err := json.NewDecoder(file).Decode(&ffprobeResult); err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to decode probe result", err,
		))
		return
	}

	// Populate _rffmpeg metadata: workers[] stays lightweight and lists only
	// live workers (fresh heartbeat, not offline, not evicted), so stale rows
	// retained within the offline-threshold window don't masquerade as online
	// nodes. The executing worker's encoder list is emitted once as the shared
	// list, and only workers whose list differs appear in
	// worker_encoder_overrides — so a homogeneous cluster emits a single copy
	// while a heterogeneous one keeps every per-worker capability.
	var rffmpegMeta *protocol.RffmpegMeta
	liveWorkers, workersErr := h.db.GetLiveSchedulableWorkers(h.heartbeatTimeout)

	// The shared baseline is the encoder list of the worker that executed the
	// probe; it also drives the suggestion.
	var sharedEncoders []string
	if finalJob.WorkerID.Valid {
		if encoders, err := h.db.GetWorkerEncoders(finalJob.WorkerID.String); err == nil {
			sharedEncoders = encoders
		}
	}

	var workerSummaries []protocol.WorkerSummary
	var overrides map[string][]string
	if workersErr == nil {
		workerSummaries, overrides = buildWorkerSummariesAndOverrides(liveWorkers, sharedEncoders)
	}

	if len(sharedEncoders) > 0 {
		rffmpegMeta = &protocol.RffmpegMeta{
			WorkerEncoders:         sharedEncoders,
			WorkerEncoderOverrides: overrides,
			Suggestion:             buildEncoderSuggestion(ffprobeResult.Streams, sharedEncoders),
			Workers:                workerSummaries,
		}
	} else if len(workerSummaries) > 0 {

		rffmpegMeta = &protocol.RffmpegMeta{
			Workers: workerSummaries,
		}
	}

	writeJSON(w, http.StatusOK, protocol.ProbeResponse{
		Format:  ffprobeResult.Format,
		Streams: ffprobeResult.Streams,
		Rffmpeg: rffmpegMeta,
	})
}

// buildWorkerSummariesAndOverrides reduces the worker list to lightweight
// summaries and returns sparse per-worker encoder overrides relative to the
// shared (executing-worker) list. Workers whose list matches the shared list
// are elided, so a homogeneous cluster emits no overrides.
func buildWorkerSummariesAndOverrides(allWorkers []*db.Worker, sharedEncoders []string) ([]protocol.WorkerSummary, map[string][]string) {
	workerSummaries := make([]protocol.WorkerSummary, 0, len(allWorkers))
	overrides := make(map[string][]string)
	for _, w := range allWorkers {
		workerSummaries = append(workerSummaries, protocol.WorkerSummary{
			ID:            w.ID,
			Name:          w.Name,
			Status:        string(w.Status),
			FFmpegVersion: w.FFmpegVersion,
			MaxConcurrent: w.MaxConcurrent,
		})
		var encoders []string
		if err := json.Unmarshal([]byte(w.Encoders), &encoders); err != nil {
			encoders = []string{}
		}
		if !slices.Equal(encoders, sharedEncoders) {
			overrides[w.ID] = encoders
		}
	}
	return workerSummaries, overrides
}

// waitForProbeTerminal blocks until the probe job reaches a terminal state,
// the request context is cancelled, or the overall budget elapses. It returns
// (nil, nil) on budget exhaustion — the caller maps that to a 504 — and a
// non-nil error only on a database failure or a cancelled request (whose job
// is cancelled in flight, mirroring the old poll loop's client-gone path).
func (h *Handler) waitForProbeTerminal(ctx context.Context, jobID string, waitTimeout time.Duration) (*db.Job, error) {
	var notifyCh <-chan struct{}
	var unsubscribe func()
	if n := h.db.JobNotifier(); n != nil {

		notifyCh, unsubscribe = n.Subscribe(jobID)
		defer unsubscribe()
	}

	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()

	for {
		j, err := h.db.GetJob(jobID)
		if err != nil {
			return nil, err
		}
		if protocol.IsTerminalStatus(j.Status) {
			return j, nil
		}

		select {
		case <-ctx.Done():

			_ = h.db.CancelJob(jobID)
			return nil, ctx.Err()
		case <-timer.C:
			return nil, nil
		case <-notifyCh:

		}
	}
}

// codecFormatPriority defines the preferred encoders (HW first, then SW) for each codec format.
// Hardware encoders are listed first (preferred), software encoders as fallback.
var codecFormatPriority = map[string][]string{
	"h264":   {"h264_nvenc", "h264_qsv", "h264_vaapi", "h264_amf", "h264_videotoolbox", "libx264", "libx264rgb"},
	"avc":    {"h264_nvenc", "h264_qsv", "h264_vaapi", "h264_amf", "h264_videotoolbox", "libx264", "libx264rgb"},
	"hevc":   {"hevc_nvenc", "hevc_qsv", "hevc_vaapi", "hevc_amf", "hevc_videotoolbox", "libx265"},
	"h265":   {"hevc_nvenc", "hevc_qsv", "hevc_vaapi", "hevc_amf", "hevc_videotoolbox", "libx265"},
	"vp9":    {"vp9_vaapi", "vp9_qsv", "vp9_videotoolbox", "libvpx-vp9"},
	"av1":    {"av1_nvenc", "av1_qsv", "av1_vaapi", "av1_amf", "libaom-av1", "libsvtav1"},
	"vp8":    {"libvpx"},
	"mpeg2":  {"mpeg2video"},
	"mpeg4":  {"mpeg4"},
	"mjpeg":  {"mjpeg"},
	"prores": {"prores_ks", "prores"},
}

// codecToFormat maps common ffprobe codec names to format keys used in codecFormatPriority.
// Includes both canonical names and container-specific FourCC/CodecID variants.
var codecToFormat = map[string]string{

	"h264":       "h264",
	"h264_nvenc": "h264",
	"libx264":    "h264",
	"libx264rgb": "h264",
	"avc1":       "avc",
	"avc3":       "avc",
	"v_mp4/h264": "avc",

	"hevc":       "hevc",
	"h265":       "hevc",
	"hevc_nvenc": "hevc",
	"libx265":    "hevc",
	"hvc1":       "hevc",
	"hev1":       "hevc",
	"v_mp4/hevc": "hevc",

	"vp9":        "vp9",
	"libvpx-vp9": "vp9",
	"vp09":       "vp9",
	"v_vp9":      "vp9",

	"av1":        "av1",
	"libaom-av1": "av1",
	"libsvtav1":  "av1",
	"av01":       "av1",
	"v_av1":      "av1",

	"vp8":    "vp8",
	"libvpx": "vp8",
	"v_vp8":  "vp8",

	"mpeg2video": "mpeg2",
	"mpeg2":      "mpeg2",
	"v_mpeg2":    "mpeg2",

	"mpeg4":   "mpeg4",
	"msmpeg4": "mpeg4",
	"mp4v":    "mpeg4",
	"xvid":    "mpeg4",
	"divx":    "mpeg4",
	"v_mpeg4": "mpeg4",

	"mjpeg":      "mjpeg",
	"motionjpeg": "mjpeg",
	"mjpg":       "mjpeg",

	"prores":    "prores",
	"prores_ks": "prores",
	"apcn":      "prores",
	"apcs":      "prores",
	"apco":      "prores",
	"apch":      "prores",
	"ap4h":      "prores",
	"ap4x":      "prores",
}

// buildEncoderSuggestion analyzes ffprobe streams and available encoders to produce
// a recommendation for the best encoder to use for transcoding.
func buildEncoderSuggestion(streams []map[string]interface{}, availableEncoders []string) *protocol.EncoderSuggestion {
	if len(streams) == 0 || len(availableEncoders) == 0 {
		return nil
	}

	// Find the first video stream and extract its codec
	var videoCodec string
	for _, stream := range streams {
		if codecType, ok := stream["codec_type"].(string); ok && codecType == "video" {
			if codecName, ok := stream["codec_name"].(string); ok {
				videoCodec = strings.ToLower(codecName)
				break
			}
		}
	}

	if videoCodec == "" {
		return nil
	}

	format, ok := codecToFormat[videoCodec]
	if !ok {
		return nil
	}

	preferredEncoders, ok := codecFormatPriority[format]
	if !ok {
		return nil
	}

	availableSet := make(map[string]bool, len(availableEncoders))
	for _, e := range availableEncoders {
		availableSet[e] = true
	}

	for _, candidate := range preferredEncoders {
		if availableSet[candidate] {
			reason := buildSuggestionReason(candidate, format)
			return &protocol.EncoderSuggestion{
				RecommendedEncoder: candidate,
				Reason:             reason,
			}
		}
	}

	return nil
}

// buildSuggestionReason generates a human-readable reason for the recommendation.
func buildSuggestionReason(encoder, format string) string {
	formatDisplay := format
	if format == "mjpeg" {
		formatDisplay = "MJPEG"
	} else {
		formatDisplay = strings.ToUpper(format)
	}

	if isHWEncoder(encoder) {
		return "Hardware-accelerated " + formatDisplay + " encoding available on this worker"
	}
	return "Detected " + formatDisplay + " video — use software encoder (no hardware encoder available for this format)"
}

// hwEncoderSuffixes lists the suffixes that identify hardware-accelerated encoders.
var hwEncoderSuffixes = []string{
	"_nvenc", "_qsv", "_vaapi", "_amf", "_videotoolbox", "_cuvid", "_vdpau", "_nvdec",
}

// isHWEncoder checks if an encoder name indicates a hardware-accelerated encoder.
func isHWEncoder(name string) bool {
	for _, suffix := range hwEncoderSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
