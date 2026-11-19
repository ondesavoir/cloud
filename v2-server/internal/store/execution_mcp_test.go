package store_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestMCPTargetAndTurnGeneration(t *testing.T) {
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	f := newSquadFixture(t)
	owner := f.login(t, "generation-owner", "owner")
	scope := execution.Scope{TenantID: f.tenant, UserID: owner}
	access := storetest.Access(t, f.store, f.tenant, owner)
	spaces, err := f.store.TenantSpaces(f.ctx, access)
	if err != nil {
		t.Fatal(err)
	}
	space := spaces[0].ID
	if _, err := f.db.Exec(f.ctx, `INSERT INTO v2_projects(id,tenant_id,data) VALUES('generation-project',$1,'{}')`, f.tenant); err != nil {
		t.Fatal(err)
	}
	source, archive := mcpPackage(t, "1.0.0")
	item, err := f.store.InstallTenantMCP(f.ctx, access, space, source, archive, 0)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "only-for-generation-test"
	item, err = f.store.SetTenantMCPSettings(f.ctx, access, space, item.ID, item.Revision, map[string]any{"apiKey": secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := f.store.ResolveWorkflowMCP(f.ctx, scope, "generation-project", []string{item.ID})
	if err != nil {
		t.Fatal(err)
	}
	session := execution.Session{ID: "selected", Target: &execution.Target{ProjectID: "generation-project"}, MCP: selection}
	turn := func(sid, id string) execution.Turn {
		return execution.Turn{Ref: execution.Ref{SessionID: sid, TurnID: id}, Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: "workflow", AttemptID: id}, Prompt: execution.TextPrompt("continue")}
	}
	first := turn(session.ID, "first")
	admitted, err := f.store.AdmitExecutionSession(f.ctx, scope, session, first, false)
	if err != nil || admitted.MCPGeneration == nil || *admitted.MCPGeneration != 0 {
		t.Fatal("dispatch did not receive committed initial generation", err)
	}
	assertTarget := func(generation int64, state, code string) {
		t.Helper()
		var g int64
		var raw []byte
		if err := f.db.QueryRow(f.ctx, `SELECT mcp_generation,mcp_target FROM v2_execution_sessions WHERE id=$1`, session.ID).Scan(&g, &raw); err != nil {
			t.Fatal(err)
		}
		var members map[string]struct {
			State, Code string
			Version     string `json:"plugin_version"`
			Revision    int64  `json:"configuration_revision"`
		}
		if json.Unmarshal(raw, &members) != nil || len(members) != 1 || bytes.Contains(raw, []byte(secret)) {
			t.Fatal("target must retain only the original member and its value-free identity")
		}
		m := members[item.ID]
		if g != generation || m.State != state || m.Code != code {
			t.Fatalf("generation %d, state %q, code %q", g, m.State, m.Code)
		}
		if state == "configured" && (m.Version != item.PluginVersion || m.Revision != item.Revision) {
			t.Fatal("configured identity did not match committed package and settings")
		}
	}
	assertFloor := func(admitted execution.Turn, floor int64) {
		t.Helper()
		got, err := f.store.ReadExecutionAttempt(f.ctx, scope, admitted.Caller)
		if err != nil || got.MCPGeneration == nil || *got.MCPGeneration != floor {
			t.Fatalf("turn %s missing durable floor %d: %v", admitted.Ref.TurnID, floor, err)
		}
	}
	assertTarget(0, "configured", "")
	target, err := f.store.ReadSessionMCPTarget(f.ctx, scope, session.ID, selection, -1)
	if err != nil || target == nil || target.Generation != 0 || len(target.Configured.Bindings) != 1 ||
		target.Configured.Bindings[0].Settings["apiKey"] != secret || !bytes.Equal(target.Configured.Packages[0].Archive, archive) {
		t.Fatal("initial delivery did not resolve the authorized archive and settings", err)
	}
	if same, err := f.store.ReadSessionMCPTarget(f.ctx, scope, session.ID, selection, 0); err != nil || same != nil {
		t.Fatal("unchanged target loaded a second snapshot", err)
	}
	if _, err := f.store.ReadSessionMCPTarget(f.ctx, scope, session.ID, nil, 0); err == nil {
		t.Fatal("missing selection accepted on unchanged generation")
	}
	assertFloor(first, 0)
	plain := execution.Session{ID: "plain", Target: session.Target}
	plainTurn := turn(plain.ID, "ordinary")
	ordinary, err := f.store.AdmitExecutionSession(f.ctx, scope, plain, plainTurn, false)
	if err != nil || ordinary.MCPGeneration != nil {
		t.Fatal("ordinary session acquired a lifecycle generation", err)
	}
	// An idempotent reinstall does not advance the target.
	if _, err := f.store.InstallTenantMCP(f.ctx, access, space, source, archive, item.Revision); err != nil {
		t.Fatal(err)
	}
	assertTarget(0, "configured", "")
	item, err = f.store.SetTenantMCPSettings(f.ctx, access, space, item.ID, item.Revision, nil, []string{"apiKey"})
	if err != nil {
		t.Fatal(err)
	}
	assertTarget(1, "unavailable", "mcp_setting_missing")
	assertFloor(first, 0)
	stale := session
	stale.ID = "stale"
	if err := f.store.CreateExecutionSession(f.ctx, scope, stale, turn(stale.ID, "stale-turn")); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("stale resolved settings were admitted: %v", err)
	}
	if _, err := f.store.FinishExecutionTurn(f.ctx, scope, first.Ref, execution.Result{Status: execution.StatusCompleted, AnswerComplete: true}); err != nil {
		t.Fatal(err)
	}
	second := turn(session.ID, "second")
	admitted, err = f.store.AdmitExecutionTurn(f.ctx, scope, session.ID, second, false)
	if err != nil || admitted.MCPGeneration == nil || *admitted.MCPGeneration != 1 {
		t.Fatal("dispatch did not receive committed follow-up generation", err)
	}
	assertFloor(second, 1)
	if err := f.store.RemoveTenantMCP(f.ctx, access, space, item.ID, item.Revision); err != nil {
		t.Fatal(err)
	}
	assertTarget(2, "unavailable", "mcp_plugin_missing")
	target, err = f.store.ReadSessionMCPTarget(f.ctx, scope, session.ID, selection, 0)
	if err != nil || target.Generation != 2 || target.Unavailable[item.ID] != "mcp_plugin_missing" || len(target.Configured.Packages) != 0 {
		t.Fatal("delivery lost a removed member or recovered old material", err)
	}
	reloaded, err := f.store.ReadExecutionSession(f.ctx, scope, session.ID)
	if err != nil || reloaded.MCP == nil || len(reloaded.MCP.Bindings) != 1 ||
		reloaded.MCP.Bindings[0].PluginID != item.ID || reloaded.MCP.Bindings[0].Settings != nil || len(reloaded.MCP.Packages) != 0 {
		t.Fatal("reloading after removal lost the selection or restored private inputs", err)
	}
	item, err = f.store.InstallTenantMCP(f.ctx, access, space, source, archive, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertTarget(3, "unavailable", "mcp_setting_missing")
	item, err = f.store.SetTenantMCPSettings(f.ctx, access, space, item.ID, item.Revision, map[string]any{"apiKey": secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertTarget(4, "configured", "")
	target, err = f.store.ReadSessionMCPTarget(f.ctx, scope, session.ID, selection, 2)
	if err != nil || target.Generation != 4 || target.Configured.Bindings[0].Revision != item.Revision {
		t.Fatal("delivery did not resolve the reinstalled revision", err)
	}
	var ordinaryUnchanged bool
	if err := f.db.QueryRow(f.ctx, `SELECT mcp_target IS NULL AND mcp_generation IS NULL FROM v2_execution_sessions WHERE id=$1`, plain.ID).Scan(&ordinaryUnchanged); err != nil || !ordinaryUnchanged {
		t.Fatal("tenant configuration widened a session with no selection", err)
	}
	assertFloor(second, 1)
	if _, err := f.store.FinishExecutionTurn(f.ctx, scope, second.Ref, execution.Result{Status: execution.StatusCompleted, AnswerComplete: true}); err != nil {
		t.Fatal(err)
	}
	// A configuration save racing the next turn may serialize on either side, but
	// dispatch and the durable turn must agree even after both commits.
	saved := make(chan error, 1)
	go func() {
		_, err := f.store.SetTenantMCPSettings(f.ctx, access, space, item.ID, item.Revision, map[string]any{"count": float64(6)}, nil)
		saved <- err
	}()
	third := turn(session.ID, "third")
	admitted, err = f.store.AdmitExecutionTurn(f.ctx, scope, session.ID, third, false)
	saveErr := <-saved
	if err != nil || saveErr != nil || admitted.MCPGeneration == nil || (*admitted.MCPGeneration != 4 && *admitted.MCPGeneration != 5) {
		t.Fatal("concurrent admission or settings save failed", err, saveErr)
	}
	assertFloor(third, *admitted.MCPGeneration)
	items, err := f.store.TenantMCPs(f.ctx, access)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	item = items[0]
	assertTarget(5, "configured", "")
	// The failed target advance must roll back the configuration save too.
	if _, err := f.db.Exec(f.ctx, `UPDATE v2_execution_sessions SET mcp_generation=$1 WHERE id=$2`, execution.MaxMCPCounter, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetTenantMCPSettings(f.ctx, access, space, item.ID, item.Revision, nil, []string{"apiKey"}); err == nil {
		t.Fatal("saved configuration past the generation limit")
	}
	assertTarget(execution.MaxMCPCounter, "configured", "")
	after, err := f.store.ResolveWorkflowMCP(f.ctx, scope, "generation-project", []string{item.ID})
	if err != nil || after.Bindings[0].Revision != item.Revision || after.Bindings[0].Settings["apiKey"] != secret {
		t.Fatal("configuration and generation did not roll back together", err)
	}
	if _, err := f.store.FinishExecutionTurn(f.ctx, scope, third.Ref, execution.Result{Status: execution.StatusCompleted, AnswerComplete: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.EndExecutionSession(f.ctx, scope, session.ID); err != nil {
		t.Fatal(err)
	}
	// An ended session at the limit cannot prevent later configuration changes.
	if err := f.store.RemoveTenantMCP(f.ctx, access, space, item.ID, item.Revision); err != nil {
		t.Fatal(err)
	}
	assertTarget(execution.MaxMCPCounter, "configured", "")
}
