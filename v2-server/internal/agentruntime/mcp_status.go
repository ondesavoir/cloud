//go:build linux

package agentruntime

import (
	"context"
	"reflect"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// ObserveSessionMCP only addresses an existing execution owned by this scope. The
// caller supplies the value-free committed target and has authorized any manual probe.
func (d *Driver) ObserveSessionMCP(ctx context.Context, scope execution.Scope, sid string, view *execution.SessionMCPStatus, probe string) error {
	d.mu.Lock()
	a := d.sessions[sid]
	d.mu.Unlock()
	if a == nil || a.scope != scope || view.Generation == nil || view.Convergence == "ended" {
		return observationUnavailable(probe)
	}
	select {
	case <-a.ready:
	default:
		return observationUnavailable(probe)
	}
	a.mu.Lock()
	selection := a.frozen.MCP
	started, ending, ended := a.started, a.ending, a.ended
	a.mu.Unlock()
	if ending && !ended {
		view.Convergence = "stopping"
	}
	if !started || ending || ended || selection == nil || len(selection.Bindings) != len(view.Members) {
		return observationUnavailable(probe)
	}
	for i, b := range selection.Bindings {
		if b.PluginID != view.Members[i].ID {
			return observationUnavailable(probe)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// A manual retry always queries first, including after a lost acknowledgement.
	// On a racing stale reply re-read once, with a fresh attempt for the same target.
	for tries := 0; tries < 2; tries++ {
		reply, err := a.mcpRequest(ctx, "get_session_mcp_state", map[string]any{})
		if err != nil || reply.Result != "accepted" || reply.State == nil {
			return observationUnavailable(probe)
		}
		state := reply.State
		if !state.Desired.matchesSelection(selection) || state.Desired.Generation == nil || *state.Desired.Generation != *view.Generation || len(state.Members) != len(view.Members) {
			return observationUnavailable(probe)
		}
		for i, row := range view.Members {
			m := state.Desired.Members[i]
			if state.Members[i].ID != row.ID || row.Configured && (m.State != "configured" || m.Version != row.Version || *view.Generation > 0 && m.Revision != row.Revision) ||
				!row.Configured && (m.State != "unavailable" || m.Code != row.Code) {
				return observationUnavailable(probe)
			}
		}
		switch state.Convergence {
		case "current":
			if !reflect.DeepEqual(state.Active, &state.Desired) {
				return observationUnavailable(probe)
			}
		case "pending", "blocked", "refreshing", "stopping":
		default:
			return observationUnavailable(probe)
		}
		if probe != "" {
			sent := false
			for i, m := range state.Members {
				if m.ID != probe {
					continue
				}
				if !view.Members[i].Configured || m.Attempt == nil || *m.Attempt < 0 || *m.Attempt >= execution.MaxMCPCounter {
					return execution.ErrConflict
				}
				res, err := a.mcpRequest(ctx, "probe_session_mcp", map[string]any{"plugin_id": probe, "generation": *view.Generation, "probe_attempt": *m.Attempt + 1})
				if err != nil {
					return err
				}
				switch res.Result {
				case "accepted", "duplicate":
					return nil
				case "stale":
					sent = true
				case "capacity_exceeded":
					return execution.ErrUnavailable
				default:
					return execution.ErrConflict
				}
				break
			}
			if !sent {
				return execution.ErrInvalidRequest
			}
			continue
		}
		view.Convergence, view.Capacity = state.Convergence, state.Capacity
		if state.Active != nil && state.Active.matchesSelection(selection) && *state.Active.Generation <= *view.Generation {
			view.Active = state.Active.Generation
		}
		for i, m := range state.Members {
			row := &view.Members[i]
			row.Queued, row.InFlight, row.Cleanup = m.Queued, m.InFlight, m.Cleanup
			switch m.Health.Status {
			case "healthy", "unhealthy":
				row.Health = m.Health.Status
			}
		}
		return nil
	}
	return execution.ErrConflict
}

func observationUnavailable(probe string) error {
	if probe != "" {
		return execution.ErrUnavailable
	}
	return nil
}
