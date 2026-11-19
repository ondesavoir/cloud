package mcpfixture

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// Install seeds the real stdio MCP used by the workflow and browser acceptance tests.
func Install(ctx context.Context, db *store.Store, access store.Access, space string) (store.TenantMCP, error) {
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
	for _, name := range []string{"orax.toml", "assets/config.json", "assets/server"} {
		content := files[name]
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0644)
		if name == "assets/server" {
			header.SetMode(0755)
		}
		writer, err := z.CreateHeader(header)
		if err != nil {
			return store.TenantMCP{}, err
		}
		if _, err = writer.Write([]byte(content)); err != nil {
			return store.TenantMCP{}, err
		}
	}
	if err := z.Close(); err != nil {
		return store.TenantMCP{}, err
	}
	digest := sha256.Sum256(archive.Bytes())
	entry := store.PluginCatalogEntry{ID: "official/fixture.workflow-mcp", SourceNamespace: "official", Identifier: "fixture.workflow-mcp", Kind: "mcp", Version: "1.0.0", Title: "Workflow MCP", MarketplaceVisible: true}
	return db.InstallTenantMCP(ctx, access, space, store.PluginPackageSource{Entry: entry, Digest: hex.EncodeToString(digest[:])}, archive.Bytes(), 0)
}
