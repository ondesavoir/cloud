package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
)

// TenantMCP is the read-safe view of a downloaded MCP package and its shared configuration.
// Installed means the verified package is cached. A session separately confirms Node installation.
type TenantMCP struct {
	ID                  string                    `json:"id"`
	Name                string                    `json:"name"`
	PluginVersion       string                    `json:"pluginVersion"`
	Revision            int64                     `json:"revision"`
	State               string                    `json:"state"`
	Settings            []pluginmarket.MCPSetting `json:"settings"`
	Values              map[string]any            `json:"values"`
	ConfiguredSecrets   []string                  `json:"configuredSecrets"`
	Complete            bool                      `json:"complete"`
	SettingsMissing     []string                  `json:"settingsMissing"`
	SettingsUnavailable bool                      `json:"settingsUnavailable,omitempty"`
}

// TenantMCPs lists tenant-wide installations without returning any secret override or default.
func (s *Store) TenantMCPs(ctx context.Context, access Access) ([]TenantMCP, error) {
	rows, err := s.pool.Query(ctx, `SELECT plugin_id,name,plugin_version,revision,declaration,settings FROM v2_tenant_mcps WHERE tenant_id=$1 ORDER BY plugin_id`, access.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []TenantMCP{}
	for rows.Next() {
		item, err := scanTenantMCP(rows, access.TenantID)
		if errors.Is(err, pluginmarket.ErrCredentials) {
			// Keep the identity and revision available for removal even if one
			// installation cannot be decrypted with this deployment's key.
			item.State = "installed"
			item.SettingsUnavailable = true
			item.Settings = []pluginmarket.MCPSetting{}
			item.ConfiguredSecrets = []string{}
			item.Values = map[string]any{}
			item.SettingsMissing = []string{}
		} else if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanTenantMCP(row pgx.Row, tenant string) (TenantMCP, error) {
	var out TenantMCP
	var raw, sealed []byte
	if err := row.Scan(&out.ID, &out.Name, &out.PluginVersion, &out.Revision, &raw, &sealed); err != nil {
		return out, err
	}
	var declaration pluginmarket.MCPDeclaration
	if json.Unmarshal(raw, &declaration) != nil {
		return out, pluginmarket.ErrPackage
	}
	values, err := pluginmarket.OpenMCPSettings(sealed, tenant+"/"+out.ID)
	if err != nil {
		return out, err
	}
	out.State = "installed"
	out.Settings = declaration.Settings
	out.Values = map[string]any{}
	out.ConfiguredSecrets = []string{}
	out.SettingsMissing = missingMCPSettings(declaration, values)
	out.Complete = len(out.SettingsMissing) == 0
	for i, setting := range out.Settings {
		value, present := values[setting.ID]
		if setting.Secret {
			if present || setting.Default != nil {
				out.ConfiguredSecrets = append(out.ConfiguredSecrets, setting.ID)
			}
			out.Settings[i].Default = nil
		} else if present {
			out.Values[setting.ID] = value
		}
	}
	return out, nil
}

func missingMCPSettings(declaration pluginmarket.MCPDeclaration, values map[string]any) []string {
	missing := []string{}
	for _, setting := range declaration.Settings {
		referenced := false
		for _, id := range declaration.References {
			referenced = referenced || id == setting.ID
		}
		value, exists := values[setting.ID]
		if !exists {
			value = setting.Default
		}
		text, isText := value.(string)
		if (setting.Required || referenced) && value == nil || setting.Required && isText && strings.TrimSpace(text) == "" {
			missing = append(missing, setting.ID)
		}
	}
	return missing
}

// InstallTenantMCP atomically replaces a verified package. Version changes clear its overrides,
// so configuration from a different declaration cannot silently grant authority to the new one.
func (s *Store) InstallTenantMCP(ctx context.Context, access Access, space string, source PluginPackageSource, archive []byte, revision int64) (TenantMCP, error) {
	if !access.Owner() {
		return TenantMCP{}, ErrForbidden
	}
	if err := requireSpace(ctx, s.pool, access.TenantID, space); err != nil {
		return TenantMCP{}, err
	}
	entry := source.Entry
	if entry.Kind != "mcp" || !execution.ValidMCPPluginID(entry.ID) || !execution.ValidMCPVersion(entry.Version) {
		return TenantMCP{}, ErrPluginKindNotInstallable
	}
	sum := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), source.Digest) {
		return TenantMCP{}, pluginmarket.ErrPackage
	}
	source.Digest = strings.ToLower(source.Digest)
	declaration, err := pluginmarket.VerifyMCPArchive(archive, entry.ID, entry.Version)
	if err != nil {
		return TenantMCP{}, err
	}
	raw, err := json.Marshal(declaration)
	if err != nil {
		return TenantMCP{}, err
	}
	var out TenantMCP
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,7))`, access.TenantID+"/"+entry.ID); err != nil {
			return err
		}
		var oldVersion, oldDigest string
		var oldRevision int64
		err := tx.QueryRow(ctx, `SELECT plugin_version,digest,revision FROM v2_tenant_mcps WHERE tenant_id=$1 AND plugin_id=$2 FOR UPDATE`, access.TenantID, entry.ID).Scan(&oldVersion, &oldDigest, &oldRevision)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if oldVersion == entry.Version && strings.EqualFold(oldDigest, source.Digest) {
			out, err = readTenantMCP(ctx, tx, access.TenantID, entry.ID)
			return err
		}
		if revision != oldRevision {
			return ErrConflict
		}
		nextRevision, err := allocateMCPRevision(ctx, tx, access.TenantID, entry.ID, oldRevision)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO v2_tenant_mcps(tenant_id,plugin_id,name,plugin_version,revision,digest,target,archive,declaration,settings)
		 VALUES($1,$2,$3,$4,$9,$5,$6,$7,$8,NULL)
		 ON CONFLICT(tenant_id,plugin_id) DO UPDATE SET name=excluded.name,plugin_version=excluded.plugin_version,
		 revision=excluded.revision,digest=excluded.digest,target=excluded.target,archive=excluded.archive,declaration=excluded.declaration,settings=NULL`, access.TenantID, entry.ID, entry.Title, entry.Version, source.Digest, source.Target, archive, raw, nextRevision)
		if err != nil {
			return err
		}
		out, err = readTenantMCP(ctx, tx, access.TenantID, entry.ID)
		if err != nil {
			return err
		}
		return advanceInstalledMCP(ctx, tx, access.TenantID, out)
	})
	if err == nil && s.mcpChanged != nil {
		s.mcpChanged(access.TenantID, entry.ID)
	}
	return out, err
}

func readTenantMCP(ctx context.Context, q dbTX, tenant, id string) (TenantMCP, error) {
	item, err := scanTenantMCP(q.QueryRow(ctx, `SELECT plugin_id,name,plugin_version,revision,declaration,settings FROM v2_tenant_mcps WHERE tenant_id=$1 AND plugin_id=$2`, tenant, id), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrPluginNotInstalled
	}
	return item, err
}

// allocateMCPRevision belongs to the same transaction as the installation/settings
// write. The durable counter survives removal, and an existing row can seed it.
func allocateMCPRevision(ctx context.Context, tx pgx.Tx, tenant, id string, current int64) (int64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `INSERT INTO v2_tenant_mcp_revisions(tenant_id,plugin_id,revision)
	 VALUES($1,$2,$3::bigint+1)
	 ON CONFLICT(tenant_id,plugin_id) DO UPDATE
	 SET revision=GREATEST(v2_tenant_mcp_revisions.revision,$3)+1
	 RETURNING revision`, tenant, id, current).Scan(&revision)
	if err == nil && revision > execution.MaxMCPCounter {
		return 0, ErrConflict
	}
	return revision, err
}

// SetTenantMCPSettings merges explicitly provided values and clears only named overrides.
// Missing secrets retain their previous value. Encryption happens before the first SQL write.
func (s *Store) SetTenantMCPSettings(ctx context.Context, access Access, space, id string, revision int64, values map[string]any, clear []string) (TenantMCP, error) {
	if !access.Owner() {
		return TenantMCP{}, ErrForbidden
	}
	if err := requireSpace(ctx, s.pool, access.TenantID, space); err != nil {
		return TenantMCP{}, err
	}
	var out TenantMCP
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var raw, sealed []byte
		var current int64
		err := tx.QueryRow(ctx, `SELECT revision,declaration,settings FROM v2_tenant_mcps WHERE tenant_id=$1 AND plugin_id=$2 FOR UPDATE`, access.TenantID, id).Scan(&current, &raw, &sealed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPluginNotInstalled
		}
		if err != nil {
			return err
		}
		if current != revision {
			return ErrConflict
		}
		var declaration pluginmarket.MCPDeclaration
		if json.Unmarshal(raw, &declaration) != nil {
			return pluginmarket.ErrPackage
		}
		stored, err := pluginmarket.OpenMCPSettings(sealed, access.TenantID+"/"+id)
		if err != nil {
			return err
		}
		declared := map[string]pluginmarket.MCPSetting{}
		for _, setting := range declaration.Settings {
			declared[setting.ID] = setting
		}
		for key, value := range values {
			setting, ok := declared[key]
			if !ok || !pluginmarket.MCPValueMatches(setting.Type, value) {
				return &execution.MCPFailure{Code: "mcp_setting_missing", PluginID: id, SettingID: key}
			}
			stored[key] = value
		}
		for _, key := range clear {
			if _, ok := declared[key]; !ok {
				return &execution.MCPFailure{Code: "mcp_setting_missing", PluginID: id, SettingID: key}
			}
			delete(stored, key)
		}
		selection := execution.MCPSelection{Bindings: []execution.MCPBinding{{PluginID: id, PluginVersion: "1.0.0", Settings: stored}}}
		if err := selection.Validate(); err != nil {
			return err
		}
		sealed, err = pluginmarket.SealMCPSettings(stored, access.TenantID+"/"+id)
		if err != nil {
			return err
		}
		nextRevision, err := allocateMCPRevision(ctx, tx, access.TenantID, id, current)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE v2_tenant_mcps SET settings=$3,revision=$4 WHERE tenant_id=$1 AND plugin_id=$2`, access.TenantID, id, sealed, nextRevision); err != nil {
			return err
		}
		out, err = readTenantMCP(ctx, tx, access.TenantID, id)
		if err != nil {
			return err
		}
		return advanceInstalledMCP(ctx, tx, access.TenantID, out)
	})
	if err == nil && s.mcpChanged != nil {
		s.mcpChanged(access.TenantID, id)
	}
	return out, err
}

// RemoveTenantMCP marks this member unavailable for every active session which selected it.
func (s *Store) RemoveTenantMCP(ctx context.Context, access Access, space, id string, revision int64) error {
	if !access.Owner() {
		return ErrForbidden
	}
	if err := requireSpace(ctx, s.pool, access.TenantID, space); err != nil {
		return err
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM v2_tenant_mcps WHERE tenant_id=$1 AND plugin_id=$2 AND revision=$3`, access.TenantID, id, revision)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return advanceMCPTargets(ctx, tx, access.TenantID, id, mcpTargetMember{State: "unavailable", Code: "mcp_plugin_missing"})
	})
	if err == nil && s.mcpChanged != nil {
		s.mcpChanged(access.TenantID, id)
	}
	return err
}

// ResolveWorkflowMCP freezes tenant-owned metadata and memory-only overrides for one node.
// Package bytes are acquired separately against the admitted session's committed target.
func (s *Store) ResolveWorkflowMCP(ctx context.Context, scope execution.Scope, project string, ids []string) (*execution.MCPSelection, error) {
	result := &execution.MCPSelection{Bindings: []execution.MCPBinding{}, Packages: []execution.MCPPackage{}}
	if len(ids) == 0 {
		return result, nil
	}
	if _, err := s.GetProject(ctx, scope, project); err != nil {
		return nil, err
	}
	if len(ids) > 16 {
		return nil, &execution.MCPFailure{Code: "mcp_selection_invalid"}
	}
	ordered := append([]string(nil), ids...)
	sort.Strings(ordered)
	// All selected packages and settings come from one database snapshot, even while an
	// owner updates multiple installations concurrently with node dispatch.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	for _, id := range ordered {
		binding, pkg, err := readMCPBinding(ctx, tx, scope.TenantID, id, false)
		if err != nil {
			return nil, err
		}
		result.Bindings = append(result.Bindings, binding)
		result.Packages = append(result.Packages, pkg)
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// readMCPBinding resolves one exact tenant installation within the caller's snapshot.
// Only the live delivery path needs package bytes. Deployment key errors stay retryable.
func readMCPBinding(ctx context.Context, tx pgx.Tx, tenant, id string, archive bool) (execution.MCPBinding, execution.MCPPackage, error) {
	b := execution.MCPBinding{PluginID: id}
	p := execution.MCPPackage{PluginID: id}
	var raw, sealed []byte
	err := tx.QueryRow(ctx, `SELECT plugin_version,revision,digest,target,declaration,settings,
		CASE WHEN $3 THEN archive END FROM v2_tenant_mcps WHERE tenant_id=$1 AND plugin_id=$2`,
		tenant, id, archive).Scan(&b.PluginVersion, &b.Revision, &p.Digest, &p.Target, &raw, &sealed, &p.Archive)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, p, &execution.MCPFailure{Code: "mcp_plugin_missing", PluginID: id}
	}
	if err != nil {
		return b, p, err
	}
	p.PluginVersion = b.PluginVersion
	var declaration pluginmarket.MCPDeclaration
	if json.Unmarshal(raw, &declaration) != nil {
		return b, p, pluginmarket.ErrPackage
	}
	b.Settings, err = pluginmarket.OpenMCPSettings(sealed, tenant+"/"+id)
	if err != nil {
		return b, p, err
	}
	if missing := missingMCPSettings(declaration, b.Settings); len(missing) > 0 {
		return b, p, &execution.MCPFailure{Code: "mcp_setting_missing", PluginID: id, SettingID: missing[0], Transport: declaration.Transport}
	}
	return b, p, nil
}
