package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/authz"
)

func TestV03MigrationPreservesLegacyPermissionsWithoutEscalation(t *testing.T) {
	baseURL := os.Getenv("INFERCRANE_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("INFERCRANE_TEST_DATABASE_URL is required for PostgreSQL migration tests")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", baseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "upgrade_" + id
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`) })

	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	upgradeURL := parsed.String()
	legacy, err := sql.Open("pgx", upgradeURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.ExecContext(ctx, `CREATE TABLE schema_migrations(version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "021_" {
			continue
		}
		body, readErr := migrationFiles.ReadFile("migrations/" + entry.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}
		tx, beginErr := legacy.BeginTx(ctx, nil)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, entry.Name())
		}
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply legacy migration %s: %v", entry.Name(), err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	const token = "legacy-operator-token"
	if _, err = legacy.ExecContext(ctx, `INSERT INTO principals(id,tenant_id,name,role,credential_hash,created_at) VALUES('legacy','global','legacy','operator',$1,NOW())`, credentialHash(token)); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, upgradeURL, Options{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	principal, err := upgraded.AuthenticatePrincipal(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	want := "read,deploy,delete"
	if got := strings.Join(principal.Scopes, ","); got != want || authz.AllowedScoped(authz.Operator, principal.Scopes, authz.ManageExternal) {
		t.Fatalf("legacy scopes=%q want=%q", got, want)
	}
	for _, table := range []string{"secret_references", "external_target_policies"} {
		var exists bool
		if err = upgraded.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, fmt.Sprintf("%s.%s", schema, table)).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %s exists=%t err=%v", table, exists, err)
		}
	}
}
