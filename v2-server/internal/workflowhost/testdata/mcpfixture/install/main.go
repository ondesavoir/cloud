// Command install seeds the workflow stdio MCP in a Playwright test database.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflowhost/testdata/mcpfixture"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 4 || os.Getenv("E2E_DATABASE_URL") == "" {
		return fmt.Errorf("set E2E_DATABASE_URL and provide tenant, user and space IDs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(ctx, os.Getenv("E2E_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	access, err := db.TenantAccess(ctx, os.Args[1], os.Args[2])
	if err != nil {
		return err
	}
	_, err = mcpfixture.Install(ctx, db, access, os.Args[3])
	return err
}
