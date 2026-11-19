package store_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func mcpPackage(t *testing.T, version string) (store.PluginPackageSource, []byte) {
	t.Helper()
	var body bytes.Buffer
	z := zip.NewWriter(&body)
	for name, contents := range map[string]string{
		"orax.toml":          "identifier = \"example.search\"\nkind = \"mcp\"\nversion = \"" + version + "\"\n",
		"assets/config.json": `{"schemaVersion":1,"settings":{"suffix":{"type":"string","title":"Suffix","description":"Optional suffix","default":""},"apiKey":{"type":"string","title":"API key","description":"Access key","secret":true,"required":true},"count":{"type":"number","title":"Count","description":"Limit","default":3}},"transport":{"type":"http","url":"https://example.test/mcp","headers":{"Authorization":{"setting":"apiKey"},"X-Optional":{"setting":"suffix"}}}}`,
	} {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	data := body.Bytes()
	sum := sha256.Sum256(data)
	return store.PluginPackageSource{Entry: store.PluginCatalogEntry{ID: "official/example.search", Kind: "mcp", Version: version, Title: "Search"}, Digest: hex.EncodeToString(sum[:])}, data
}

func TestTenantMCPRevisionSurvivesRemoval(t *testing.T) {
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	f := newSquadFixture(t)
	owner := f.login(t, "mcp-owner", "owner")
	a := storetest.Access(t, f.store, f.tenant, owner)
	spaces, err := f.store.TenantSpaces(f.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	space := spaces[0].ID
	source, archive := mcpPackage(t, "1.0.0")
	old, err := f.store.InstallTenantMCP(f.ctx, a, space, source, archive, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A revision beyond int32, as the durable allocator would have handed out, must keep fencing
	// stale editors across removal and reinstallation.
	const legacyRevision int64 = 1 << 32
	if _, err := f.db.Exec(f.ctx, `UPDATE v2_tenant_mcps SET revision=$1 WHERE tenant_id=$2 AND plugin_id=$3`, legacyRevision, f.tenant, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, `UPDATE v2_tenant_mcp_revisions SET revision=$1 WHERE tenant_id=$2 AND plugin_id=$3`, legacyRevision, f.tenant, old.ID); err != nil {
		t.Fatal(err)
	}
	migrated := f.store
	if err := migrated.RemoveTenantMCP(f.ctx, a, space, old.ID, legacyRevision); err != nil {
		t.Fatal(err)
	}
	source, archive = mcpPackage(t, "2.0.0")
	current, err := migrated.InstallTenantMCP(f.ctx, a, space, source, archive, 0)
	if err != nil || current.Revision <= legacyRevision {
		t.Fatal("reinstallation reused a prior revision", current.Revision, err)
	}
	for _, stale := range []int64{old.Revision, legacyRevision} {
		older, olderArchive := mcpPackage(t, "1.0.0")
		if _, err := migrated.InstallTenantMCP(f.ctx, a, space, older, olderArchive, stale); !errors.Is(err, store.ErrConflict) {
			t.Fatal("old editor replaced new installation", stale, err)
		}
		if _, err := migrated.SetTenantMCPSettings(f.ctx, a, space, old.ID, stale, map[string]any{"apiKey": "stale-key"}, nil); !errors.Is(err, store.ErrConflict) {
			t.Fatal("old editor changed new installation", stale, err)
		}
		if err := migrated.RemoveTenantMCP(f.ctx, a, space, old.ID, stale); !errors.Is(err, store.ErrConflict) {
			t.Fatal("old editor removed new installation", stale, err)
		}
	}
	// Concurrent editors still share one optimistic lock: exactly one wins and
	// the other cannot reuse its requested revision after that write commits.
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		workers.Go(func() {
			_, err := migrated.SetTenantMCPSettings(f.ctx, a, space, current.ID, current.Revision, map[string]any{"apiKey": "fresh-key"}, nil)
			results <- err
		})
	}
	workers.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal("concurrent editors did not serialize", wins, conflicts)
	}
	items, err := migrated.TenantMCPs(f.ctx, a)
	if err != nil || len(items) != 1 || items[0].Revision <= current.Revision {
		t.Fatal("settings did not advance durable revision", err)
	}
	configured := items[0]
	if err := migrated.RemoveTenantMCP(f.ctx, a, space, current.ID, configured.Revision); err != nil {
		t.Fatal(err)
	}
	reinstalled, err := migrated.InstallTenantMCP(f.ctx, a, space, source, archive, 0)
	if err != nil || reinstalled.Revision <= configured.Revision {
		t.Fatal("settings revision collided after reinstallation", err)
	}
}

func TestWorkflowMCPResolutionPreservesDatabaseFailures(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "mcp-owner", "owner")
	if _, err := f.db.Exec(f.ctx, `INSERT INTO v2_projects(id,tenant_id,data) VALUES('project',$1,'{}')`, f.tenant); err != nil {
		t.Fatal(err)
	}
	// A failed SELECT is not evidence that the selected plugin is missing.
	if _, err := f.db.Exec(f.ctx, `ALTER TABLE v2_tenant_mcps RENAME TO unavailable_mcps`); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.ResolveWorkflowMCP(f.ctx, execution.Scope{TenantID: f.tenant, UserID: owner}, "project", []string{"official/example.search"})
	var setup *execution.MCPFailure
	if err == nil || errors.As(err, &setup) {
		t.Fatal("database failure became a configuration diagnostic", err)
	}
}

func TestTenantMCPConfigurationIsSharedSealedAndVersioned(t *testing.T) {
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	f := newSquadFixture(t)
	owner := f.login(t, "mcp-owner", "owner")
	member := f.login(t, "mcp-member", "member")
	a := storetest.Access(t, f.store, f.tenant, owner)
	m := storetest.Access(t, f.store, f.tenant, member)
	spaces, err := f.store.TenantSpaces(f.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	space := spaces[0].ID
	source, archive := mcpPackage(t, "1.0.0")
	source.Digest = strings.ToUpper(source.Digest)
	if _, err := f.store.InstallTenantMCP(f.ctx, m, space, source, archive, 0); !errors.Is(err, store.ErrForbidden) {
		t.Fatal("member installed", err)
	}
	bad := append([]byte(nil), archive...)
	bad[0] ^= 1
	if _, err := f.store.InstallTenantMCP(f.ctx, a, space, source, bad, 0); err == nil {
		t.Fatal("bad digest accepted")
	}
	item, err := f.store.InstallTenantMCP(f.ctx, a, space, source, archive, 0)
	if err != nil || item.Complete || len(item.SettingsMissing) != 1 || item.SettingsMissing[0] != "apiKey" {
		t.Fatal(item, err)
	}
	item, err = f.store.SetTenantMCPSettings(f.ctx, a, space, item.ID, item.Revision, map[string]any{"apiKey": "   "}, nil)
	if err != nil || item.Complete {
		t.Fatal("blank required value was complete", err)
	}
	if _, err = f.db.Exec(f.ctx, `INSERT INTO v2_projects(id,tenant_id,data,plugin_space_id) VALUES('mcp-project',$1,'{}',$2)`, f.tenant, space); err != nil {
		t.Fatal(err)
	}
	scope := execution.Scope{TenantID: f.tenant, UserID: member}
	if _, err = f.store.ResolveWorkflowMCP(f.ctx, scope, "mcp-project", []string{item.ID}); err == nil {
		t.Fatal("missing required secret accepted")
	}
	const secret = "test-only-mcp-secret-1201"
	item, err = f.store.SetTenantMCPSettings(f.ctx, a, space, item.ID, item.Revision, map[string]any{"apiKey": secret}, nil)
	if err != nil || !item.Complete {
		t.Fatal(item, err)
	}
	public, _ := json.Marshal(item)
	if bytes.Contains(public, []byte(secret)) || len(item.ConfiguredSecrets) != 1 {
		t.Fatal("public secret exposure")
	}
	var sealed []byte
	if err = f.db.QueryRow(f.ctx, `SELECT settings FROM v2_tenant_mcps WHERE tenant_id=$1 AND plugin_id=$2`, f.tenant, item.ID).Scan(&sealed); err != nil || bytes.Contains(sealed, []byte(secret)) {
		t.Fatal("unsealed settings", err)
	}
	if _, err = f.store.SetTenantMCPSettings(f.ctx, m, space, item.ID, item.Revision, map[string]any{"apiKey": "bad"}, nil); !errors.Is(err, store.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err = f.store.SetTenantMCPSettings(f.ctx, a, space, item.ID, item.Revision-1, map[string]any{}, nil); !errors.Is(err, store.ErrConflict) {
		t.Fatal(err)
	}
	if _, err = f.store.SetTenantMCPSettings(f.ctx, a, space, item.ID, item.Revision, map[string]any{"count": true}, nil); err == nil {
		t.Fatal("wrong scalar type accepted")
	}
	item, err = f.store.SetTenantMCPSettings(f.ctx, a, space, item.ID, item.Revision, map[string]any{"count": float64(4)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := f.store.ResolveWorkflowMCP(f.ctx, scope, "mcp-project", []string{item.ID})
	if err != nil || selection.Bindings[0].Settings["apiKey"] != secret || selection.Bindings[0].Settings["count"] != float64(4) {
		t.Fatal("shared frozen values", err)
	}
	if selection.Packages[0].Digest != strings.ToLower(source.Digest) {
		t.Fatal("digest was not canonical")
	}
	if len(selection.Packages[0].Archive) != 0 {
		t.Fatal("pre-admission resolution must not load package archives")
	}
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.ResolveWorkflowMCP(cancelled, scope, "mcp-project", []string{item.ID}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost its infrastructure error", err)
	}
	frozen, _ := json.Marshal(selection)
	if bytes.Contains(frozen, []byte(secret)) || bytes.Contains(frozen, archive) {
		t.Fatal("snapshot persisted secret or archive")
	}
	if _, err = f.store.ResolveWorkflowMCP(f.ctx, scope, "mcp-project", []string{item.ID, item.ID}); err == nil {
		t.Fatal("duplicate selected plugin accepted")
	}
	if _, err = f.store.ResolveWorkflowMCP(f.ctx, scope, "missing-project", []string{item.ID}); err == nil {
		t.Fatal("missing project allowed")
	}
	var otherTenant string
	if err := f.db.QueryRow(f.ctx, `INSERT INTO tenants(name) VALUES('MCP isolated tenant') RETURNING id`).Scan(&otherTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, `INSERT INTO v2_projects(id,tenant_id,data) VALUES('other-mcp-project',$1,'{}')`, otherTenant); err != nil {
		t.Fatal(err)
	}
	otherScope := execution.Scope{TenantID: otherTenant, UserID: member}
	if _, err := f.store.ResolveWorkflowMCP(f.ctx, otherScope, "mcp-project", []string{item.ID}); !errors.Is(err, execution.ErrNotFound) {
		t.Fatal("cross-tenant project resolved", err)
	}
	var missing *execution.MCPFailure
	if _, err := f.store.ResolveWorkflowMCP(f.ctx, otherScope, "other-mcp-project", []string{item.ID}); !errors.As(err, &missing) || missing.Code != "mcp_plugin_missing" {
		t.Fatal("another tenant's installation resolved", err)
	}
	other := m
	other.TenantID = "11111111-1111-4111-8111-111111111111"
	items, err := f.store.TenantMCPs(f.ctx, other)
	if err != nil || len(items) != 0 {
		t.Fatal("cross tenant leakage", err)
	}
	for _, key := range []string{"", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))} {
		t.Run("deployment key unavailable", func(t *testing.T) {
			t.Setenv("V2_MARKET_CREDENTIAL_KEY", key)
			rows, err := f.store.TenantMCPs(f.ctx, a)
			if err != nil || len(rows) != 1 || rows[0].ID != item.ID || rows[0].Revision != item.Revision || !rows[0].SettingsUnavailable || rows[0].Complete || len(rows[0].ConfiguredSecrets) != 0 {
				t.Fatal("unreadable settings obscured installation identity or looked usable", err)
			}
			_, err = f.store.ResolveWorkflowMCP(f.ctx, scope, "mcp-project", []string{item.ID})
			var setup *execution.MCPFailure
			if !errors.Is(err, pluginmarket.ErrCredentials) || errors.As(err, &setup) {
				t.Fatal("credential failure lost its infrastructure classification", err)
			}
		})
	}
	t.Run("remove without deployment key", func(t *testing.T) {
		t.Setenv("V2_MARKET_CREDENTIAL_KEY", "")
		if err := f.store.RemoveTenantMCP(f.ctx, a, space, item.ID, item.Revision); err != nil {
			t.Fatal(err)
		}
	})
	if _, err = f.store.ResolveWorkflowMCP(f.ctx, scope, "mcp-project", []string{item.ID}); err == nil {
		t.Fatal("removed plugin selected")
	}
	if selection.Bindings[0].Settings["apiKey"] != secret {
		t.Fatal("active snapshot mutated")
	}
	item, err = f.store.InstallTenantMCP(f.ctx, a, space, source, archive, 0)
	if err != nil {
		t.Fatal(err)
	}
	item, err = f.store.SetTenantMCPSettings(f.ctx, a, space, item.ID, item.Revision, map[string]any{"apiKey": secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, newArchive := mcpPackage(t, "1.0.1")
	item, err = f.store.InstallTenantMCP(f.ctx, a, space, upgraded, newArchive, item.Revision)
	if err != nil || item.Complete {
		t.Fatal("upgrade retained prior configuration", err)
	}
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", "")
	if _, err = f.store.SetTenantMCPSettings(f.ctx, a, space, item.ID, item.Revision, map[string]any{"apiKey": secret}, nil); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("key failure was unsafe", err)
	}
}
