package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

func TestMCPResolutionInfrastructureErrorsRemainRetryable(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, store.ErrForbidden, execution.ErrNotFound, errors.New("database commit failed: private detail"), fmt.Errorf("begin: %w", context.Canceled)} {
		failure := mcpFailure(err)
		detail := failure.Detail(1, false, time.Unix(0, 0))
		if failure.Kind != workflow.FailureSession || failure.Code != "" || detail["auto_retryable"] != true || failure.Message != "execution could not resolve MCP configuration" {
			t.Fatalf("infrastructure error misclassified or exposed: %+v", failure)
		}
	}
	for _, code := range []string{"mcp_plugin_missing", "mcp_setting_missing"} {
		failure := mcpFailure(fmt.Errorf("resolve: %w", &execution.MCPFailure{Code: code, PluginID: "official/acme.tools"}))
		if failure.Code != code || failure.Detail(1, false, time.Unix(0, 0))["auto_retryable"] != false {
			t.Fatalf("typed setup error lost classification: %+v", failure)
		}
	}
}

func TestMCPLifecycleGateFailuresAreExplicitAndNotRetryable(t *testing.T) {
	for _, code := range []string{"mcp_node_capability_missing", "mcp_lifecycle_not_ready"} {
		t.Run(code, func(t *testing.T) {
			failure := mcpFailure(&execution.MCPFailure{Code: code})
			detail := failure.Detail(1, false, time.Unix(0, 0))
			if detail["code"] != code || detail["auto_retryable"] != false || detail["resumable"] != true {
				t.Fatalf("capability gate lost its failure contract: %+v", detail)
			}
		})
	}
}

func TestStartRefusalRetainsMCPDiagnostic(t *testing.T) {
	f := newFixture(t)
	f.exec.beforeStart = func(execution.StartRequest) error {
		return fmt.Errorf("start: %w", &execution.MCPFailure{Code: "mcp_node_capability_missing"})
	}
	rid := f.run(t, twoAgents, str("check"))
	if _, err := f.host.StartRun(t.Context(), f.scope, rid); err != nil {
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
	if n.SessionID != nil || payload.Detail["kind"] != "missing_agent_config" || payload.Detail["code"] != "mcp_node_capability_missing" || payload.Detail["auto_retryable"] != false {
		t.Fatal("Start refusal lost the setup diagnostic", payload.Detail)
	}
}

func TestMCPSetupFailureRetainsIdentifiersAndCannotAutoRetry(t *testing.T) {
	f := newFixture(t)
	runID := f.run(t, twoAgents, str("check"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	ref := f.exec.awaitStart(t)
	f.exec.finish(ref, execution.Result{Status: execution.StatusFailed, FailureCode: "mcp_setting_missing", MCPFailure: &execution.MCPFailure{Code: "mcp_setting_missing", PluginID: "official/acme.tools", SettingID: "token", Transport: "http"}})
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed {
		t.Fatal(run.Status)
	}
	var payload struct {
		Detail map[string]any `json:"error_detail"`
	}
	if err := json.Unmarshal(node(nodes, "a").Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"kind": "missing_agent_config", "code": "mcp_setting_missing", "plugin_id": "official/acme.tools", "setting_id": "token", "transport": "http", "auto_retryable": false, "resumable": true} {
		if payload.Detail[key] != want {
			t.Fatalf("%s = %v, want %v", key, payload.Detail[key], want)
		}
	}
	f.exec.mu.Lock()
	defer f.exec.mu.Unlock()
	if len(f.exec.started) != 1 {
		t.Fatal("MCP setup failure retried")
	}
}
