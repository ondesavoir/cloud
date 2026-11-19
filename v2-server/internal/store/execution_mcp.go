package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// mcpTargetMember is the value-free desired identity. An unavailable member retains its slot.
type mcpTargetMember struct {
	State    string `json:"state"`
	Version  string `json:"plugin_version,omitempty"`
	Revision int64  `json:"configuration_revision,omitempty"`
	Code     string `json:"code,omitempty"`
}

// initialMCPTarget fences the resolver's snapshot against concurrent configuration changes.
// Share locks are taken in canonical plugin order and held until generation zero and its
// first turn commit. A writer that wins first causes a stale start to retry resolution.
func initialMCPTarget(ctx context.Context, tx pgx.Tx, scope execution.Scope, session execution.Session, kind execution.CallerKind) ([]byte, error) {
	if session.MCP == nil || len(session.MCP.Bindings) == 0 {
		return nil, nil
	}
	if kind != execution.CallerWorkflow || session.Target == nil {
		return nil, execution.ErrInvalidRequest
	}
	if err := session.MCP.Validate(); err != nil {
		return nil, err
	}
	target := make(map[string]mcpTargetMember, len(session.MCP.Bindings))
	for _, b := range session.MCP.Bindings {
		var version string
		var revision int64
		err := tx.QueryRow(ctx, `SELECT plugin_version, revision FROM v2_tenant_mcps
			WHERE tenant_id=$1 AND plugin_id=$2 FOR SHARE`, scope.TenantID, b.PluginID).Scan(&version, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &execution.MCPFailure{Code: "mcp_plugin_missing", PluginID: b.PluginID}
		}
		if err != nil {
			return nil, err
		}
		if version != b.PluginVersion || revision != b.Revision {
			return nil, execution.ErrConflict
		}
		target[b.PluginID] = mcpTargetMember{State: "configured", Version: version, Revision: revision}
	}
	return json.Marshal(target)
}

// advanceMCPTargets runs inside the configuration writer's transaction. Lock matching
// sessions in a common order even when different plugins affect overlapping sets.
func advanceMCPTargets(ctx context.Context, tx pgx.Tx, tenant, id string, member mcpTargetMember) error {
	raw, err := json.Marshal(member)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH selected AS MATERIALIZED (
		SELECT id FROM v2_execution_sessions WHERE tenant_id=$1 AND ended_at IS NULL
		AND mcp_target ? $2 ORDER BY id FOR UPDATE
	) UPDATE v2_execution_sessions AS s SET mcp_generation=s.mcp_generation+1,
		mcp_target=jsonb_set(s.mcp_target,ARRAY[$2],$3::jsonb,false)
		FROM selected WHERE s.id=selected.id`, tenant, id, raw)
	return err
}

func advanceInstalledMCP(ctx context.Context, tx pgx.Tx, tenant string, item TenantMCP) error {
	member := mcpTargetMember{State: "configured", Version: item.PluginVersion, Revision: item.Revision}
	if !item.Complete {
		member = mcpTargetMember{State: "unavailable", Code: "mcp_setting_missing"}
	}
	return advanceMCPTargets(ctx, tx, tenant, item.ID, member)
}

// ReadSessionMCPTarget resolves the latest whole target from a single MVCC snapshot.
// Even the unchanged-generation fast path verifies the durable selection against the
// live Agent's original set. A lost selection cannot be reconstructed from installations.
func (s *Store) ReadSessionMCPTarget(ctx context.Context, scope execution.Scope, sid string, selected *execution.MCPSelection, known int64) (*execution.MCPTarget, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := requireExecutionAccess(ctx, tx, scope); err != nil {
		return nil, executionError(err)
	}
	view, err := readMCPStatus(ctx, tx, scope, sid)
	if err != nil {
		return nil, err
	}
	if view.Convergence == "ended" {
		return nil, execution.ErrSessionEnded
	}
	invalid := &execution.MCPFailure{Code: "mcp_selection_invalid"}
	if selected == nil || len(selected.Bindings) == 0 || view.Generation == nil || len(view.Members) != len(selected.Bindings) {
		return nil, invalid
	}
	result := &execution.MCPTarget{Generation: *view.Generation, Unavailable: map[string]string{}}
	for i, member := range view.Members {
		if selected.Bindings[i].PluginID != member.ID {
			return nil, invalid
		}
		if !member.Configured {
			result.Unavailable[member.ID] = member.Code
		} else {
			if result.Generation == known {
				continue
			}
			binding, pkg, err := readMCPBinding(ctx, tx, scope.TenantID, member.ID, true)
			if err != nil {
				return nil, err
			}
			if binding.PluginVersion != member.Version || binding.Revision != member.Revision {
				return nil, execution.ErrConflict
			}
			result.Configured.Bindings = append(result.Configured.Bindings, binding)
			result.Configured.Packages = append(result.Configured.Packages, pkg)
		}
	}
	if result.Generation == known {
		return nil, nil
	}
	if err := result.Configured.Validate(); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadSessionMCPStatus never decrypts settings or loads archives, including for team viewers.
func (s *Store) ReadSessionMCPStatus(ctx context.Context, scope execution.Scope, sid string) (execution.SessionMCPStatus, error) {
	return readMCPStatus(ctx, s.pool, scope, sid)
}

func readMCPStatus(ctx context.Context, q dbTX, scope execution.Scope, sid string) (execution.SessionMCPStatus, error) {
	view := execution.SessionMCPStatus{Convergence: "unknown", Members: []execution.MCPMemberStatus{}}
	var stored, desired []byte
	var ended bool
	err := q.QueryRow(ctx, `SELECT data->'MCP',mcp_target,mcp_generation,ended_at IS NOT NULL
		FROM v2_execution_sessions WHERE tenant_id=$1 AND user_id=$2 AND id=$3`, scope.TenantID, scope.UserID, sid).Scan(&stored, &desired, &view.Generation, &ended)
	if err != nil {
		return view, executionError(err)
	}
	if ended {
		view.Convergence = "ended"
	}
	var original execution.MCPSelection
	var target map[string]mcpTargetMember
	invalid := &execution.MCPFailure{Code: "mcp_selection_invalid"}
	if len(stored) > 0 && json.Unmarshal(stored, &original) != nil {
		return view, invalid
	}
	if view.Generation == nil && len(original.Bindings) == 0 {
		return view, nil
	}
	if view.Generation == nil || len(original.Bindings) == 0 || len(original.Bindings) > 16 || json.Unmarshal(desired, &target) != nil || len(target) != len(original.Bindings) {
		return view, invalid
	}
	for i, b := range original.Bindings {
		member, ok := target[b.PluginID]
		if !ok || !execution.ValidMCPPluginID(b.PluginID) || i > 0 && original.Bindings[i-1].PluginID >= b.PluginID {
			return view, invalid
		}
		row := execution.MCPMemberStatus{ID: b.PluginID, Health: "unknown"}
		switch member.State {
		case "configured":
			if !execution.ValidMCPVersion(member.Version) || member.Revision < 0 || member.Revision > execution.MaxMCPCounter {
				return view, invalid
			}
			row.Configured, row.Version, row.Revision = true, member.Version, member.Revision
		case "unavailable":
			if member.Code != "mcp_setting_missing" && member.Code != "mcp_plugin_missing" {
				return view, invalid
			}
			row.Code = member.Code
		default:
			return view, invalid
		}
		view.Members = append(view.Members, row)
	}
	return view, nil
}
