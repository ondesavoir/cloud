package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

func (h *handler) workflowNodeMCP(w http.ResponseWriter, r *http.Request, a access) {
	scope, sid, ok := h.workflowAttemptSession(w, r, a)
	if !ok {
		return
	}
	probe := ""
	var expected *int64
	if r.Method == http.MethodPost {
		if scope.UserID != a.UserID {
			fault(w, http.StatusForbidden, "workflow_run_probe_forbidden")
			return
		}
		var in struct {
			Plugin     string `json:"pluginId"`
			Generation *int64 `json:"generation"`
		}
		if !decodeWorkflow(w, r, &in) {
			return
		}
		if !execution.ValidMCPPluginID(in.Plugin) || in.Generation == nil || *in.Generation < 0 || *in.Generation > execution.MaxMCPCounter {
			fault(w, http.StatusBadRequest, "invalid_mcp_probe")
			return
		}
		probe, expected = in.Plugin, in.Generation
	}
	reader, ok := h.store.(interface {
		ReadSessionMCPStatus(context.Context, execution.Scope, string) (execution.SessionMCPStatus, error)
	})
	if !ok {
		fault(w, http.StatusServiceUnavailable, "mcp_status_unavailable")
		return
	}
	view, err := reader.ReadSessionMCPStatus(r.Context(), scope, sid)
	if err != nil {
		if errors.Is(err, execution.ErrNotFound) {
			fault(w, http.StatusNotFound, "workflow_node_session_not_found")
		} else {
			fault(w, http.StatusServiceUnavailable, "mcp_status_unavailable")
		}
		return
	}
	view.CanProbe = scope.UserID == a.UserID && h.opts.SessionMCP != nil && view.Generation != nil && view.Convergence != "ended"
	if probe != "" {
		if !view.CanProbe || view.Generation == nil || *view.Generation != *expected {
			fault(w, http.StatusConflict, "mcp_target_changed")
			return
		}
		found := false
		for _, m := range view.Members {
			if m.ID == probe && m.Configured {
				found = true
				break
			}
		}
		if !found {
			fault(w, http.StatusConflict, "mcp_not_configured")
			return
		}
	}
	if h.opts.SessionMCP != nil && view.Generation != nil && view.Convergence != "ended" {
		err = h.opts.SessionMCP.ObserveSessionMCP(r.Context(), scope, sid, &view, probe)
		if err != nil {
			if errors.Is(err, execution.ErrConflict) {
				fault(w, http.StatusConflict, "mcp_target_changed")
			} else {
				fault(w, http.StatusServiceUnavailable, "mcp_status_unavailable")
			}
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	if probe != "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
