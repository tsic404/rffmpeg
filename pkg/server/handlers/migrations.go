package handlers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/tsic404/rffmpeg/pkg/protocol"
	"github.com/tsic404/rffmpeg/pkg/server/db"
	"github.com/tsic404/rffmpeg/pkg/server/migration"
)

// ListMigrationEvents handles listing worker migration audit events.
// GET /api/v1/migrations
// Query params: limit (default 50, capped at 500), offset (default 0).
// Results are ordered newest-first (timestamp DESC, id DESC).
func (h *Handler) ListMigrationEvents(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r, 50, 500)

	events, err := h.db.GetMigrationEvents(limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to list migration events", err,
		))
		return
	}

	targets, err := h.redistributionTargets(events)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to list migration targets", err,
		))
		return
	}

	writeJSON(w, http.StatusOK, MigrationEventsResponse{
		Events: migrationEventsToInfo(events, targets),
	})
}

// GetMigrationEvent retrieves a single worker migration audit event by ID.
// GET /api/v1/migrations/{eventId}
func (h *Handler) GetMigrationEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "eventId")

	event, err := h.db.GetMigrationEvent(eventID)
	if err != nil {
		if errors.Is(err, protocol.ErrMigrationEventNotFound) {
			writeError(w, http.StatusNotFound, protocol.NewProtocolError(
				protocol.ErrCodeNotFound, "Migration event not found", err,
			))
			return
		}
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get migration event", err,
		))
		return
	}

	redistributions, err := h.db.GetJobRedistributionsByEvent(eventID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, protocol.NewProtocolError(
			protocol.ErrCodeInternalError, "Failed to get migration targets", err,
		))
		return
	}

	info := migration.FromDBEvent(event)
	info.Targets = migration.RedistributionsByEvent(redistributions)[eventID]
	writeJSON(w, http.StatusOK, MigrationEventResponse{Event: info})
}

// redistributionTargets fetches the per-job redistribution targets for the
// given events in one batched query, grouped by event ID.
func (h *Handler) redistributionTargets(events []*db.MigrationEvent) (map[string][]migration.JobRedistributionTarget, error) {
	if len(events) == 0 {
		return nil, nil
	}
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	redistributions, err := h.db.GetJobRedistributionsByEvents(ids)
	if err != nil {
		return nil, err
	}
	return migration.RedistributionsByEvent(redistributions), nil
}

// migrationEventsToInfo converts db.MigrationEvent records into the public
// migration.EventInfo slice, attaching the per-job redistribution targets.
func migrationEventsToInfo(events []*db.MigrationEvent, targets map[string][]migration.JobRedistributionTarget) []migration.EventInfo {
	if events == nil {
		return []migration.EventInfo{}
	}
	result := make([]migration.EventInfo, len(events))
	for i, event := range events {
		info := migration.FromDBEvent(event)
		info.Targets = targets[event.ID]
		result[i] = info
	}
	return result
}

// MigrationEventResponse wraps a single migration event for JSON responses.
type MigrationEventResponse struct {
	Event migration.EventInfo `json:"event"`
}

// MigrationEventsResponse wraps a page of migration events for JSON responses.
type MigrationEventsResponse struct {
	Events []migration.EventInfo `json:"events"`
}
