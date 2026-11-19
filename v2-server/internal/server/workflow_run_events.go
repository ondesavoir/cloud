package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/chat"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// WorkflowRunEvents tells which runs changed. Subscribe returns a channel that receives a value
// after each committed change of the run, coalescing the ones not consumed yet, and is never
// closed; cancel releases the subscription. workflowhost.Hub implements it.
type WorkflowRunEvents interface {
	Subscribe(runID string) (<-chan struct{}, func())
}

// workflowRunEvents streams workflow_run_invalidated for one run as Server-Sent Events, the shape
// of desktop's app event: no state, only that the run must be read again, so the page keeps reading
// the persisted run over HTTP. One is sent as soon as the stream opens, so a page that read the run
// before subscribing misses no change in between.
func (h *handler) workflowRunEvents(w http.ResponseWriter, r *http.Request, a access) {
	s, tenant, ok := h.workflowRunScope(w, a)
	if !ok {
		return
	}
	if h.opts.WorkflowRunEvents == nil {
		fault(w, http.StatusServiceUnavailable, "workflow_run_events_unavailable")
		return
	}
	runID := r.PathValue("rid")
	if !workflowUUID.MatchString(runID) {
		fault(w, http.StatusNotFound, "workflow_run_not_found")
		return
	}
	if _, err := s.WorkflowRun(r.Context(), tenant, runID); err != nil {
		workflowRunFault(w, err, "workflow run events")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		internal(w, "workflow run events", errors.New("response writer cannot flush"))
		return
	}
	// Subscribe before the first event, so nothing committed after it goes unannounced.
	changed, cancel := h.opts.WorkflowRunEvents.Subscribe(runID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	invalidated := func() {
		writeEvent(w, "workflow_run_invalidated", "", map[string]string{"runId": runID})
		flusher.Flush()
	}
	invalidated()

	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		case <-changed:
			invalidated()
		}
	}
}

// workflowNodeEvents relays one node attempt's session the way the chat routes relay a
// conversation's, so the run page draws it with the chat page's own rendering. The shared
// execution service keeps a session in the scope of whoever started the run; after checking the
// caller's tenant membership and locating the run in that tenant, the handler reads the session in
// the starter's scope so any workspace member can follow live and replayed events.
func (h *handler) workflowNodeEvents(w http.ResponseWriter, r *http.Request, a access) {
	scope, sessionID, ok := h.workflowAttemptSession(w, r, a)
	if !ok {
		return
	}
	nodeRunID := r.PathValue("nodeRunId")
	after, ok := eventCursor(r)
	if !ok {
		fault(w, http.StatusBadRequest, "invalid_cursor")
		return
	}
	stream, err := h.opts.Execution.Subscribe(r.Context(), scope, sessionID, after)
	switch {
	case err == nil:
	case errors.Is(err, execution.ErrNotFound):
		fault(w, http.StatusNotFound, "workflow_node_session_not_found")
		return
	case errors.Is(err, execution.ErrInvalidRequest):
		fault(w, http.StatusBadRequest, "invalid_cursor")
		return
	case errors.Is(err, execution.ErrUnavailable):
		fault(w, http.StatusServiceUnavailable, "execution_unavailable")
		return
	default:
		internal(w, "workflow node subscribe", err)
		return
	}
	relaySessionEvents(w, r, stream, func(ctx context.Context, sid, turnID string) (string, error) {
		return chat.TurnPrompt(ctx, h.opts.Execution, scope, sid, turnID)
	}, "node_run", nodeRunID)
}

// workflowAttemptSession resolves the session through a run and attempt in the caller's tenant.
func (h *handler) workflowAttemptSession(w http.ResponseWriter, r *http.Request, a access) (execution.Scope, string, bool) {
	s, tenant, ok := h.workflowRunScope(w, a)
	if !ok {
		return execution.Scope{}, "", false
	}
	runID, nodeRunID := r.PathValue("rid"), r.PathValue("nodeRunId")
	if !workflowUUID.MatchString(runID) {
		fault(w, http.StatusNotFound, "workflow_run_not_found")
		return execution.Scope{}, "", false
	}
	run, err := s.WorkflowRun(r.Context(), tenant, runID)
	if err != nil {
		workflowRunFault(w, err, "workflow node events")
		return execution.Scope{}, "", false
	}
	nodes, err := s.WorkflowRunNodes(r.Context(), tenant, runID)
	if err != nil {
		workflowRunFault(w, err, "workflow node events")
		return execution.Scope{}, "", false
	}
	// Replaced failed attempts remain readable through their own session, with the same run
	// membership and execution-owner checks as live attempts.
	failed, err := s.WorkflowRunFailedAttempts(r.Context(), tenant, runID)
	if err != nil {
		workflowRunFault(w, err, "workflow node history")
		return execution.Scope{}, "", false
	}
	nodes = append(nodes, failed...)
	var sessionID *string
	found := false
	for _, node := range nodes {
		if node.ID == nodeRunID {
			sessionID, found = node.SessionID, true
			break
		}
	}
	if !found {
		fault(w, http.StatusNotFound, "workflow_node_run_not_found")
		return execution.Scope{}, "", false
	}
	if sessionID == nil {
		// Not dispatched yet: the session is bound only once its first prompt was accepted.
		fault(w, http.StatusNotFound, "workflow_node_session_not_found")
		return execution.Scope{}, "", false
	}
	starter := run.StartedBy
	if starter == "" {
		starter = a.UserID
	}
	scope := execution.Scope{TenantID: tenant, UserID: starter}
	return scope, *sessionID, true
}
