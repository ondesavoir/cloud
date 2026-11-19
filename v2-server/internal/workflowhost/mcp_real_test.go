//go:build linux

package workflowhost

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/agentruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

const mcpFixtureSecret = "workflow-mcp-private-token"

// The local model calls the selected fixture and recognizes only its two expected setting markers.
type workflowMCPModel struct {
	calls atomic.Int64
	pause atomic.Pointer[mcpModelPause]
}

type mcpModelPause struct{ entered, resume chan struct{} }

func (m *workflowMCPModel) Complete(w http.ResponseWriter, r *http.Request, model nodeservices.Model) {
	m.calls.Add(1)
	if pause := m.pause.Swap(nil); pause != nil {
		close(pause.entered)
		select {
		case <-pause.resume:
		case <-r.Context().Done():
			return
		}
	}
	var req struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		w.WriteHeader(400)
		return
	}
	tool := ""
	for _, candidate := range req.Tools {
		if strings.Contains(candidate.Function.Name, "mcp_probe") {
			tool = candidate.Function.Name
		}
	}
	message := map[string]any{"role": "assistant", "content": "MCP_NOT_SELECTED"}
	finish := "stop"
	if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool" {
		if bytes.Contains(req.Messages[len(req.Messages)-1].Content, []byte("MCP_SETTINGS_RECEIVED")) {
			message["content"] = "MCP_SETTINGS_RECEIVED"
		} else if bytes.Contains(req.Messages[len(req.Messages)-1].Content, []byte("MCP_SETTINGS_UPDATED")) {
			message["content"] = "MCP_SETTINGS_UPDATED"
		} else {
			message["content"] = "MCP_TOOL_FAILED"
		}
	} else if tool != "" {
		message["content"] = nil
		message["tool_calls"] = []any{map[string]any{"index": 0, "id": "call_mcp_probe", "type": "function", "function": map[string]string{"name": tool, "arguments": "{}"}}}
		finish = "tool_calls"
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "mcp-model", "object": "chat.completion", "model": model.ID, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, choice := range []any{map[string]any{"index": 0, "delta": message, "finish_reason": nil}, map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
		data, _ := json.Marshal(map[string]any{"id": "mcp-model", "object": "chat.completion.chunk", "model": model.ID, "choices": []any{choice}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func workflowMCPArchive(t *testing.T) ([]byte, string) {
	t.Helper()
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	files := map[string]string{
		"orax.toml":          "resolver = 1\nidentifier = \"fixture.workflow-mcp\"\nkind = \"mcp\"\nversion = \"1.0.0\"\ndescription = \"Workflow MCP integration fixture\"\n",
		"assets/config.json": `{"schemaVersion":1,"settings":{"token":{"type":"string","title":"Token","description":"Private test credential","required":true,"secret":true}},"transport":{"type":"stdio","command":"assets/server","env":{"MCP_TOKEN":{"setting":"token"}}}}`,
		"assets/server": `#!/usr/bin/python3
import json, os, sys
for line in sys.stdin:
    request = json.loads(line)
    if 'id' not in request:
        continue
    method = request.get('method')
    if method == 'initialize':
        result = {'protocolVersion': request['params']['protocolVersion'], 'capabilities': {'tools': {}}, 'serverInfo': {'name': 'workflow-fixture', 'version': '1.0.0'}}
    elif method == 'tools/list':
        result = {'tools': [{'name': 'mcp_probe', 'description': 'Verify selected MCP configuration', 'inputSchema': {'type': 'object', 'properties': {}}}]}
    elif method == 'tools/call':
        markers = {'workflow-mcp-private-token': 'MCP_SETTINGS_RECEIVED', 'changed-fixture-token': 'MCP_SETTINGS_UPDATED'}
        result = {'content': [{'type': 'text', 'text': markers.get(os.environ.get('MCP_TOKEN'), 'MCP_SETTINGS_MISSING')}]}
    else:
        result = {}
    print(json.dumps({'jsonrpc': '2.0', 'id': request['id'], 'result': result}), flush=True)
`,
	}
	for name, content := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0644)
		if name == "assets/server" {
			header.SetMode(0755)
		}
		writer, err := z.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive.Bytes())
	return archive.Bytes(), hex.EncodeToString(digest[:])
}

// The browser test installs the same stdio fixture into its isolated Playwright database.
func TestInstallBrowserMCPFixture(t *testing.T) {
	tenant := os.Getenv("E2E_MCP_FIXTURE_TENANT")
	if tenant == "" {
		t.Skip("only invoked by the browser lifecycle test")
	}
	ctx := t.Context()
	db, err := store.Open(ctx, os.Getenv("E2E_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	access, err := db.TenantAccess(ctx, tenant, os.Getenv("E2E_MCP_FIXTURE_USER"))
	if err != nil {
		t.Fatal(err)
	}
	archive, digest := workflowMCPArchive(t)
	source := workflowMCPSource(digest)
	if _, err := db.InstallTenantMCP(ctx, access, os.Getenv("E2E_MCP_FIXTURE_SPACE"), source, archive, 0); err != nil {
		t.Fatal(err)
	}
}

func workflowMCPSource(digest string) store.PluginPackageSource {
	return store.PluginPackageSource{Entry: store.PluginCatalogEntry{ID: "official/fixture.workflow-mcp", SourceNamespace: "official", Identifier: "fixture.workflow-mcp", Kind: "mcp", Version: "1.0.0", Title: "Workflow MCP", MarketplaceVisible: true}, Digest: digest}
}

func TestInteractiveMCPKeyFailureBeforeStart(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	access := storetest.Access(t, f.s, f.scope.TenantID, f.scope.UserID)
	spaces, err := f.s.TenantSpaces(ctx, access)
	if err != nil || len(spaces) == 0 {
		t.Fatal("fixture space unavailable", err)
	}
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	archive, digest := workflowMCPArchive(t)
	item, err := f.s.InstallTenantMCP(ctx, access, spaces[0].ID, workflowMCPSource(digest), archive, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetTenantMCPSettings(ctx, access, spaces[0].ID, item.ID, item.Revision, map[string]any{"token": mcpFixtureSecret}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", "")
	var logs bytes.Buffer
	f.host.log = slog.New(slog.NewTextHandler(&logs, nil))
	graph := strings.Replace(interactiveChain, `"interactive":true`, `"interactive":true,"mcps":[{"mcpId":"official/fixture.workflow-mcp","enabled":true}]`, 1)
	rid := f.run(t, graph, str("check"))
	if _, err := f.host.StartRun(ctx, f.scope, rid); err != nil {
		t.Fatal(err)
	}
	_, nodes := f.await(t, rid, terminal)
	n := node(nodes, "a")
	var payload struct {
		Detail map[string]any `json:"error_detail"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if n.SessionID != nil || payload.Detail["kind"] != "interactive_session" || payload.Detail["auto_retryable"] != false {
		t.Fatal("interactive pre-start failure lost its retry policy", payload.Detail)
	}
	if !strings.Contains(logs.String(), rid) || !strings.Contains(logs.String(), n.ID) || strings.Contains(logs.String(), mcpFixtureSecret) {
		t.Fatal("missing run correlation or unsafe credential diagnostic")
	}
}

func TestRealWorkflowMCPLifecycle(t *testing.T) {
	assets := os.Getenv("V2_NODE_ASSETS_DIR")
	if assets == "" {
		t.Skip("set V2_NODE_ASSETS_DIR for real Node and OpenCode")
	}
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{23}, 32)))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, "vm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	db := storetest.Open(t)
	user, tenant := storetest.Login(t, db, "workflow-mcp")
	scope := execution.Scope{TenantID: tenant, UserID: user}
	repositories, err := projectrepos.New(db, projectrepos.Options{Root: filepath.Join(root, "r")})
	if err != nil {
		t.Fatal(err)
	}
	project, err := repositories.SeedDemo(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	models, err := executionruntime.ModelsFromEnvironment(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	models = executionruntime.WithFixtureModels(models)
	model := models["fixture"]
	provider := &workflowMCPModel{}
	model.Binding.Provider = provider
	models["fixture"] = model
	gitPath, err := exec.CommandContext(ctx, "git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	driver, err := agentruntime.New(agentruntime.Options{
		Root: filepath.Join(root, "n"), AssetsDirectory: assets, GitBackend: filepath.Join(strings.TrimSpace(string(gitPath)), "git-http-backend"),
		Bindings:      map[string]nodeservices.Binding{"fixture": model.Binding},
		ReadMCPTarget: db.ReadSessionMCPTarget,
		ResolveProject: func(c context.Context, s execution.Scope, id string) (agentruntime.Project, error) {
			p, e := db.GetProject(c, s, id)
			return agentruntime.Project{MirrorPath: p.MirrorPath, Commit: p.Commit, SnapshotRef: p.SnapshotRef}, e
		},
		RecordWorkspace: func(c context.Context, id string, at agentruntime.Checkout) error {
			return db.RecordWorkspace(c, id, store.WorkspaceLocation{Path: at.Path, BaseCommit: at.BaseCommit})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := driver.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	db.SetMCPChangeObserver(driver.MCPChanged)
	service, err := executionruntime.New(ctx, executionruntime.Config{Store: db, Models: models, Agent: driver})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	archive, digest := workflowMCPArchive(t)
	access, err := db.TenantAccess(ctx, tenant, user)
	if err != nil {
		t.Fatal(err)
	}
	source := workflowMCPSource(digest)
	installed, err := db.InstallTenantMCP(ctx, access, project.SpaceID, source, archive, 0)
	if err != nil {
		t.Fatal(err)
	}
	configured, err := db.SetTenantMCPSettings(ctx, access, project.SpaceID, installed.ID, installed.Revision, map[string]any{"token": mcpFixtureSecret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	host, err := NewHost(Config{Store: db, Execution: service, Catalog: service})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	f := &fixture{s: db, host: host, scope: server.WorkflowRunScope{TenantID: tenant, UserID: user}, project: project.ID}
	graph := strings.Replace(twoAgents, `"modelId":"Other Model"`, `"modelId":"fixture"`, 1)
	graph = strings.Replace(graph, `"roleId":"reviewer"`, `"mcps":[{"mcpId":"official/fixture.workflow-mcp","enabled":true}]`, 1)
	graph = strings.Replace(graph, `"prompt":"审阅这段初稿：{{#a.output#}}"`, `"prompt":"审阅这段初稿：{{#a.output#}}","mcps":[{"mcpId":"official/fixture.workflow-mcp","enabled":false}]`, 1)
	runID := f.run(t, graph, str("Use the selected MCP probe."))
	if _, err := host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	awaitRun := func(id string) (store.WorkflowRun, []store.WorkflowNodeRun) {
		events, unsubscribe := host.Hub().Subscribe(id)
		defer unsubscribe()
		for {
			run, err := db.WorkflowRun(ctx, tenant, id)
			if err != nil {
				t.Fatal(err)
			}
			if terminal(run, nil) {
				nodes, err := db.WorkflowEngineNodeRuns(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return run, nodes
			}
			select {
			case <-events:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	assertPublicRecords := func(n store.WorkflowNodeRun) {
		if n.SessionID == nil {
			t.Fatal("expected a retained execution session")
		}
		session, err := service.ReadSession(ctx, scope, *n.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		turns, err := service.ReadTurns(ctx, scope, *n.SessionID, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []any{session, turns, n} {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(mcpFixtureSecret)) {
				t.Fatal("MCP setting leaked into public execution records")
			}
		}
	}
	run, attempts := awaitRun(runID)
	selectedNode := node(attempts, "a")
	if run.Status != store.WorkflowStatusSucceeded || selectedNode.Output == nil || !strings.Contains(*selectedNode.Output, "MCP_SETTINGS_RECEIVED") {
		t.Fatalf("workflow did not reach the authorized MCP: run=%s node=%s", run.Status, selectedNode.Status)
	}
	assertPublicRecords(selectedNode)
	// Explicitly disabling both bindings leaves the old Node path usable, including
	// the shared-checkout handoff between the two real Agent sessions.
	disabled := strings.Replace(graph, `"enabled":true`, `"enabled":false`, 1)
	emptyRun := f.run(t, disabled, str("Run without MCP."))
	if _, err := host.StartRun(ctx, f.scope, emptyRun); err != nil {
		t.Fatal(err)
	}
	run, attempts = awaitRun(emptyRun)
	if run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("disabled MCP workflow failed: %+v nodes %+v", run, attempts)
	}
	for _, id := range []string{"a", "b"} {
		n := node(attempts, id)
		if n.Output == nil || !strings.Contains(*n.Output, "MCP_NOT_SELECTED") {
			t.Fatalf("node %s output=%v: disabled MCP was available", id, n.Output)
		}
		assertPublicRecords(n)
	}
	// A later run must recheck configuration, fail before starting the Agent, and retain
	// identifiers that let the operator repair the setting without exposing its old value.
	t.Run("admitted driver refreshes the same execution", func(t *testing.T) {
		selected, err := db.ResolveWorkflowMCP(ctx, scope, project.ID, []string{installed.ID})
		if err != nil {
			t.Fatal(err)
		}
		session := execution.Session{ID: "real-mcp-lifecycle", WorkspaceID: "real-mcp-checkout", Target: &execution.Target{ProjectID: project.ID}, MCP: selected,
			Executor: &execution.Executor{ID: "opencode:fixture", AgentID: "official/ora-space.opencode", PluginVersion: "0.6.11", Model: model.Model}}
		defer func() {
			if err := driver.End(ctx, session.ID); err != nil {
				t.Error(err)
			}
		}()
		for i, answer := range []string{"MCP_SETTINGS_RECEIVED", "MCP_SETTINGS_UPDATED"} {
			id := fmt.Sprintf("lifecycle-%d", i)
			turn := execution.Turn{Ref: execution.Ref{SessionID: session.ID, TurnID: id}, Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: "live-mcp", NodeID: "agent", AttemptID: id}, Prompt: execution.TextPrompt("Use the selected MCP probe.")}
			if i == 0 {
				turn, err = db.AdmitExecutionSession(ctx, scope, session, turn, false)
			} else {
				turn, err = db.AdmitExecutionTurn(ctx, scope, session.ID, turn, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			var result execution.Result
			if i == 0 {
				pause := &mcpModelPause{make(chan struct{}), make(chan struct{})}
				provider.pause.Store(pause)
				done := make(chan struct{})
				go func() {
					defer close(done)
					result, err = driver.Run(ctx, scope, session, turn, func(context.Context, execution.Event) error { return nil })
				}()
				func() {
					defer close(pause.resume)
					select {
					case <-pause.entered:
					case <-done:
						t.Fatal("turn ended before model barrier")
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					var saveErr error
					configured, saveErr = db.SetTenantMCPSettings(ctx, access, project.SpaceID, installed.ID, configured.Revision, map[string]any{"token": "changed-fixture-token"}, nil)
					if saveErr != nil {
						t.Fatal(saveErr)
					}
					deadline := time.Now().Add(15 * time.Second)
					for {
						view, readErr := db.ReadSessionMCPStatus(ctx, scope, session.ID)
						if readErr == nil {
							readErr = driver.ObserveSessionMCP(ctx, scope, session.ID, &view, "")
						}
						if readErr != nil {
							t.Fatal(readErr)
						}
						if view.Active != nil && *view.Active == 0 && view.Generation != nil && *view.Generation == 1 && (view.Convergence == "pending" || view.Convergence == "refreshing") {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("busy turn did not retain its original ACP revision: %+v", view)
						}
						time.Sleep(100 * time.Millisecond)
					}
				}()
				<-done
			} else {
				result, err = driver.Run(ctx, scope, session, turn, func(context.Context, execution.Event) error { return nil })
			}
			if err != nil || result.Status != execution.StatusCompleted || !strings.Contains(result.Answer, answer) {
				t.Fatalf("turn %d: status=%s code=%s expected fixture response=%t err=%v", i, result.Status, result.FailureCode, strings.Contains(result.Answer, answer), err)
			}
			if _, err := db.FinishExecutionTurn(ctx, scope, turn.Ref, result); err != nil {
				t.Fatal(err)
			}
			readStatus := func(probe string) execution.SessionMCPStatus {
				t.Helper()
				view, err := db.ReadSessionMCPStatus(ctx, scope, session.ID)
				if err == nil {
					err = driver.ObserveSessionMCP(ctx, scope, session.ID, &view, probe)
				}
				if err != nil {
					t.Fatal(err)
				}
				return view
			}
			readStatus(installed.ID)
			deadline := time.Now().Add(15 * time.Second)
			for {
				view := readStatus("")
				if view.Active != nil && *view.Active == 1 && view.Convergence == "current" && len(view.Members) == 1 && view.Members[0].Health == "healthy" && !view.Members[0].InFlight && !view.Members[0].Cleanup {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("generation %d never became delivered and healthy: %+v", i, view)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	})
	if _, err := db.SetTenantMCPSettings(ctx, access, project.SpaceID, installed.ID, configured.Revision, nil, []string{"token"}); err != nil {
		t.Fatal(err)
	}
	failedRun := f.run(t, graph, str("Run after credential removal."))
	if _, err := host.StartRun(ctx, f.scope, failedRun); err != nil {
		t.Fatal(err)
	}
	run, attempts = awaitRun(failedRun)
	failed := node(attempts, "a")
	var payload struct {
		Detail struct {
			Code          string `json:"code"`
			PluginID      string `json:"plugin_id"`
			SettingID     string `json:"setting_id"`
			AutoRetryable *bool  `json:"auto_retryable"`
		} `json:"error_detail"`
	}
	if err := json.Unmarshal(failed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if run.Status != store.WorkflowStatusFailed || failed.SessionID != nil || payload.Detail.Code != "mcp_setting_missing" || payload.Detail.PluginID != installed.ID || payload.Detail.SettingID != "token" || payload.Detail.AutoRetryable == nil || *payload.Detail.AutoRetryable {
		t.Fatalf("unexpected MCP configuration failure: run=%s node=%+v detail=%+v", run.Status, failed, payload.Detail)
	}
}
