//go:build linux

package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
)

func (d *Driver) SupportsMCPLifecycle() bool { return d.opts.ReadMCPTarget != nil }

func (a *agentSession) prepareMCP(ctx context.Context, selected *execution.MCPSelection) error {
	if selected == nil || len(selected.Bindings) == 0 {
		return nil
	}
	p := a.project
	capable, ok := p.ipc().(interface{ Supports(string) bool })
	// Cloud v2 must not downgrade a nonempty selection to a frozen setup on older Nodes.
	if !ok || !capable.Supports("session_mcp") || !capable.Supports("agent_session") || !capable.Supports("session_mcp_lifecycle") {
		return &execution.MCPFailure{Code: "mcp_node_capability_missing"}
	}
	if p.driver.opts.ReadMCPTarget == nil {
		return &execution.MCPFailure{Code: "mcp_lifecycle_not_ready"}
	}
	target, err := p.driver.opts.ReadMCPTarget(ctx, a.scope, a.frozen.ID, selected, -1)
	if err != nil {
		return err
	}
	if target == nil || target.Generation != 0 || !selected.SameMembers(&target.Configured) || len(target.Unavailable) != 0 || len(target.Configured.Packages) != len(selected.Bindings) {
		return execution.ErrConflict
	}
	if err := target.Configured.Validate(); err != nil {
		return err
	}
	for _, pkg := range target.Configured.Packages {
		if err := p.installMCP(ctx, pkg); err != nil {
			return err
		}
	}
	target.Configured.Packages = nil
	a.frozen.MCP = &target.Configured
	return nil
}

// installMCP stages only package bytes. Settings never enter files, package URLs or install frames.
func (p *projectRuntime) installMCP(ctx context.Context, archive execution.MCPPackage) error {
	sum := sha256.Sum256(archive.Archive)
	if len(archive.Archive) == 0 || hex.EncodeToString(sum[:]) != archive.Digest || p.services == nil {
		return execution.ErrUnavailable
	}
	file, err := os.CreateTemp(p.root, "mcp-*.orax")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(archive.Archive)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	url, release, err := p.services.StageMarketPackage(nodeservices.Package{Name: "mcp-" + archive.Digest, Path: file.Name(), SHA256: archive.Digest})
	if err != nil {
		return err
	}
	defer release()
	download := map[string]string{"url": url, "sha256": archive.Digest}
	releaseSpec := map[string]any{"kind": "universal", "download": download}
	if archive.Target != "" {
		releaseSpec = map[string]any{"kind": "targets", "targets": []any{map[string]any{"target": archive.Target, "download": download}}}
	}
	item := map[string]any{"plugin_id": archive.PluginID, "version": archive.PluginVersion, "release": releaseSpec}
	message, err := p.control(ctx, p.driver.nextID("mcp"), "install_plugins", "plugins_result", map[string]any{"spec": map[string]any{"node_id": p.nodeID(), "plugins": []any{item}}})
	if err != nil {
		return err
	}
	var response struct {
		Kind   string
		Result struct {
			Items []struct {
				ID      string `json:"plugin_id"`
				Outcome struct{ Kind, Version string }
			}
		}
	}
	if json.Unmarshal(message.Payload, &response) != nil || response.Kind != "plugins_completed" || len(response.Result.Items) != 1 {
		return execution.ErrUnavailable
	}
	installed := response.Result.Items[0]
	if installed.ID != archive.PluginID || installed.Outcome.Kind != "installed" || installed.Outcome.Version != archive.PluginVersion {
		return execution.ErrUnavailable
	}
	return nil
}

func mcpSpec(selection *execution.MCPSelection) []any {
	bindings := make([]any, 0, len(selection.Bindings))
	for _, binding := range selection.Bindings {
		bindings = append(bindings, map[string]any{"plugin_id": binding.PluginID, "plugin_version": binding.PluginVersion, "settings": binding.Settings})
	}
	return bindings
}
