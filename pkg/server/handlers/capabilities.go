package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tsic404/rffmpeg/pkg/protocol"
)

// ListAllEncoders handles listing all unique encoders across all workers
func (h *Handler) ListAllEncoders(w http.ResponseWriter, r *http.Request) {
	encoders, err := h.db.GetAllEncodersInfo()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get encoders", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, ListEncodersResponse{Encoders: encoders})
}

// ListAllDecoders handles listing all unique decoders across all workers
func (h *Handler) ListAllDecoders(w http.ResponseWriter, r *http.Request) {
	decoders, err := h.db.GetAllDecodersInfo()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get decoders", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, ListDecodersResponse{Decoders: decoders})
}

// ListAllHwaccels handles listing all unique hwaccels across all workers.
// GET /api/v1/hwaccels
// Default: text/plain single-column list (matching ffmpeg -hwaccels output).
// Query parameter ?json=true returns a JSON array.
func (h *Handler) ListAllHwaccels(w http.ResponseWriter, r *http.Request) {
	hwaccels, err := h.db.GetAllHwaccels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get hwaccels", err,
		))
		return
	}

	if r.URL.Query().Get("json") == "true" || r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, ListHwaccelsResponse{Hwaccels: hwaccels})
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for _, h := range hwaccels {
		fmt.Fprintln(w, h)
	}
}

// ListEncodersResponse is the response for listing all encoders
type ListEncodersResponse struct {
	Encoders []protocol.EncoderInfo `json:"encoders"`
}

// ListDecodersResponse is the response for listing all decoders
type ListDecodersResponse struct {
	Decoders []protocol.DecoderInfo `json:"decoders"`
}

// ListHwaccelsResponse is the response for listing all hwaccels in JSON mode
type ListHwaccelsResponse struct {
	Hwaccels []string `json:"hwaccels"`
}

// Outputs plain text by default (matching ffmpeg -codecs format);
// use ?json=1 for structured JSON output.
func (h *Handler) ListAllCodecs(w http.ResponseWriter, r *http.Request) {
	items, err := h.db.GetAllCodecs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get codecs", err,
		))
		return
	}

	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, InfoFlagResponse{Items: items})
		return
	}

	writeInfoFlagText(w, "Codecs", items)
}

// ListAllFilters handles listing all unique filters across all workers.
// Outputs plain text by default (matching ffmpeg -filters format);
// use ?json=1 for structured JSON output.
func (h *Handler) ListAllFilters(w http.ResponseWriter, r *http.Request) {
	items, err := h.db.GetAllFilters()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get filters", err,
		))
		return
	}

	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, InfoFlagResponse{Items: items})
		return
	}

	writeInfoFlagText(w, "Filters", items)
}

// ListAllPixFmts handles listing all unique pixel formats across all workers.
// Outputs plain text by default (matching ffmpeg -pix_fmts format);
// use ?json=1 for structured JSON output.
func (h *Handler) ListAllPixFmts(w http.ResponseWriter, r *http.Request) {
	items, err := h.db.GetAllPixFmts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get pixel formats", err,
		))
		return
	}

	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, InfoFlagResponse{Items: items})
		return
	}

	writeInfoFlagText(w, "Pixel formats", items)
}

// ListAllFormats handles listing all unique formats across all workers.
// Outputs plain text by default (matching ffmpeg -formats format);
// use ?json=1 for structured JSON output.
func (h *Handler) ListAllFormats(w http.ResponseWriter, r *http.Request) {
	items, err := h.db.GetAllFormats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get formats", err,
		))
		return
	}

	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, http.StatusOK, InfoFlagResponse{Items: items})
		return
	}

	writeInfoFlagText(w, "File formats", items)
}

// InfoFlagResponse is the JSON response for info flag endpoints (codecs/filters/pix_fmts/formats).
type InfoFlagResponse struct {
	Items []string `json:"items"`
}

// writeInfoFlagText writes a plain-text response in the ffmpeg info flag format.
// Header format: "Header:\nitem1\nitem2\n..."
func writeInfoFlagText(w http.ResponseWriter, header string, items []string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Build output: header + newline + items
	var sb strings.Builder
	sb.WriteString(header)
	sb.WriteString(":\n")
	for _, item := range items {
		sb.WriteString(item)
		sb.WriteByte('\n')
	}
	w.Write([]byte(sb.String()))
}
