package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// All HTTP access and run lookups are real. Only the selected session's
// committed status and Node observation are supplied by the fixture.
type mcpStatusFixture struct {
	*store.Store
	scope  execution.Scope
	probes []string
}

func (f *mcpStatusFixture) ReadSessionMCPStatus(_ context.Context, scope execution.Scope, sid string) (execution.SessionMCPStatus, error) {
	if scope != f.scope || sid != "mcp-session" {
		return execution.SessionMCPStatus{}, execution.ErrNotFound
	}
	return execution.SessionMCPStatus{Generation: new(int64(2)), Convergence: "unknown", Members: []execution.MCPMemberStatus{
		{ID: "official/fixture.tools", Configured: true, Version: "1.0.0", Health: "unknown"},
	}}, nil
}

func (f *mcpStatusFixture) ObserveSessionMCP(_ context.Context, scope execution.Scope, sid string, _ *execution.SessionMCPStatus, probe string) error {
	if scope != f.scope || sid != "mcp-session" {
		return execution.ErrNotFound
	}
	f.probes = append(f.probes, probe)
	return nil
}

func TestSessionMCPHTTPAccessAndGeneration(t *testing.T) {
	call, db := workflowServerWithStore(t, nil)
	tenant, user, rid := pendingRun(t, call, db)
	ctx := t.Context()
	if err := db.SetWorkflowStarter(ctx, tenant, rid, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartWorkflowRun(ctx, tenant, rid); err != nil {
		t.Fatal(err)
	}
	node, err := db.CreateWorkflowNodeRun(ctx, tenant, rid, store.WorkflowNodeRunInput{NodeID: "agent", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindWorkflowNodeSession(ctx, tenant, rid, node.ID, "mcp-session"); err != nil {
		t.Fatal(err)
	}
	f := &mcpStatusFixture{Store: db, scope: execution.Scope{TenantID: tenant, UserID: user}}
	h := server.New(f, server.Options{SessionMCP: f})
	member := storetest.SignIn(t, db, "mcp-viewer", "Viewer", store.RoleMember)
	starter := storetest.SignIn(t, db, "workflow-owner", "Owner", store.RoleOwner)
	path := "/api/v2/tenants/" + tenant + "/workflow-runs/" + rid + "/nodes/" + node.ID + "/mcp"
	request := func(method, url, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: server.SessionCookie, Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("GET", path, member, "")
	var view execution.SessionMCPStatus
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &view) != nil ||
		view.CanProbe || len(view.Members) != 1 || view.Members[0].ID != "official/fixture.tools" || len(f.probes) != 1 || f.probes[0] != "" {
		t.Fatalf("team view = %d %s", w.Code, w.Body.String())
	}
	const valid = `{"pluginId":"official/fixture.tools","generation":2}`
	for _, tc := range []struct {
		token, body, code string
		status            int
	}{
		{member, valid, "workflow_run_probe_forbidden", 403},
		{starter, `{"pluginId":"official/fixture.tools","generation":1}`, "mcp_target_changed", 409},
		{starter, `{"pluginId":"official/fixture.other","generation":2}`, "mcp_not_configured", 409},
	} {
		w := request("POST", path+"/probe", tc.token, tc.body)
		var reason struct{ Code string }
		if w.Code != tc.status || json.Unmarshal(w.Body.Bytes(), &reason) != nil || reason.Code != tc.code || len(f.probes) != 1 {
			t.Fatalf("invalid probe reached Node or lost its reason: %d %s", w.Code, w.Body.String())
		}
	}
	w = request("POST", path+"/probe", starter, valid)
	if w.Code != 202 || len(f.probes) != 2 || f.probes[1] != "official/fixture.tools" {
		t.Fatalf("configured starter probe = %d, observed %v", w.Code, f.probes)
	}
}
