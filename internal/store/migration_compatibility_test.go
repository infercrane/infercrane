package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func isolatedMigrationDatabase(t *testing.T) (context.Context, string, *sql.DB) {
	t.Helper()
	baseURL := os.Getenv("INFERCRANE_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("INFERCRANE_TEST_DATABASE_URL is required for PostgreSQL migration tests")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", baseURL)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "compat_" + id
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
		_ = admin.Close()
	})
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return ctx, parsed.String(), admin
}

func TestConcurrentStartupSerializesMigrations(t *testing.T) {
	ctx, databaseURL, _ := isolatedMigrationDatabase(t)
	const starters = 4
	stores := make([]*Store, starters)
	errs := make([]error, starters)
	start := make(chan struct{})
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(starters)
	done.Add(starters)
	for i := range starters {
		go func(index int) {
			defer done.Done()
			ready.Done()
			<-start
			stores[index], errs[index] = Open(ctx, databaseURL, Options{MaxOpenConns: 2, MaxIdleConns: 1})
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent startup %d: %v", i, err)
		}
		defer stores[i].Close()
	}
	migrations := embeddedMigrations(t)
	var count, distinct int
	if err := stores[0].db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT version) FROM schema_migrations`).Scan(&count, &distinct); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) || distinct != len(migrations) {
		t.Fatalf("migration ledger count=%d distinct=%d want=%d", count, distinct, len(migrations))
	}
}

func embeddedMigrations(t *testing.T) []struct {
	name, checksum string
	body           []byte
} {
	t.Helper()
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var migrations []struct {
		name, checksum string
		body           []byte
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, readErr := migrationFiles.ReadFile("migrations/" + entry.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}
		digest := sha256.Sum256(body)
		migrations = append(migrations, struct {
			name, checksum string
			body           []byte
		}{entry.Name(), hex.EncodeToString(digest[:]), body})
	}
	return migrations
}

func TestEveryHistoricalMigrationPrefixUpgradesToCurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive migration-prefix compatibility is covered by the normal test gate")
	}
	migrations := embeddedMigrations(t)
	for prefix := 0; prefix <= len(migrations); prefix++ {
		t.Run(fmt.Sprintf("prefix-%02d", prefix), func(t *testing.T) {
			ctx, databaseURL, _ := isolatedMigrationDatabase(t)
			legacy, err := sql.Open("pgx", databaseURL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = legacy.ExecContext(ctx, `CREATE TABLE schema_migrations(version TEXT PRIMARY KEY, checksum TEXT, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
				t.Fatal(err)
			}
			for _, migration := range migrations[:prefix] {
				tx, beginErr := legacy.BeginTx(ctx, nil)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				if _, err = tx.ExecContext(ctx, string(migration.body)); err == nil {
					_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, migration.name, migration.checksum)
				}
				if err != nil {
					_ = tx.Rollback()
					t.Fatalf("apply prefix migration %s: %v", migration.name, err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if err = legacy.Close(); err != nil {
				t.Fatal(err)
			}
			upgraded, err := Open(ctx, databaseURL, Options{MaxOpenConns: 2, MaxIdleConns: 1})
			if err != nil {
				t.Fatalf("upgrade prefix %d: %v", prefix, err)
			}
			defer upgraded.Close()
			var count, checksummed int
			if err = upgraded.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(checksum) FROM schema_migrations`).Scan(&count, &checksummed); err != nil || count != len(migrations) || checksummed != len(migrations) {
				t.Fatalf("ledger count=%d checksummed=%d want=%d err=%v", count, checksummed, len(migrations), err)
			}
		})
	}
}

func TestMigrationLedgerRejectsTamperGapAndNewerDatabase(t *testing.T) {
	for name, mutate := range map[string]string{
		"checksum": `UPDATE schema_migrations SET checksum=repeat('0',64) WHERE version='001_initial.sql'`,
		"gap":      `DELETE FROM schema_migrations WHERE version='010_autoscaling_operations.sql'`,
		"newer":    `INSERT INTO schema_migrations(version,checksum) VALUES('999_future.sql',repeat('f',64))`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, databaseURL, _ := isolatedMigrationDatabase(t)
			current, err := Open(ctx, databaseURL, Options{MaxOpenConns: 2, MaxIdleConns: 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = current.db.ExecContext(ctx, mutate); err != nil {
				t.Fatal(err)
			}
			if err = current.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, openErr := Open(ctx, databaseURL, Options{}); openErr == nil {
				reopened.Close()
				t.Fatalf("%s migration history was accepted", name)
			}
		})
	}
}

func TestModelAPISupplyEvidenceTablesHaveImmutableTriggers(t *testing.T) {
	ctx, databaseURL, _ := isolatedMigrationDatabase(t)
	current, err := Open(ctx, databaseURL, Options{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()

	rows, err := current.db.QueryContext(ctx, `
		SELECT c.relname,t.tgname
		FROM pg_trigger t
		JOIN pg_class c ON c.oid=t.tgrelid
		WHERE NOT t.tgisinternal AND c.relname IN (
			'model_api_supplier_offers',
			'model_api_supply_qualifications',
			'model_api_supply_plans',
			'model_api_supply_plan_candidates'
		)
		ORDER BY c.relname,t.tgname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var table, trigger string
		if err = rows.Scan(&table, &trigger); err != nil {
			t.Fatal(err)
		}
		found[table] = trigger
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"model_api_supplier_offers":        "model_api_supplier_offers_immutable",
		"model_api_supply_qualifications":  "model_api_supply_qualifications_immutable",
		"model_api_supply_plans":           "model_api_supply_plans_immutable",
		"model_api_supply_plan_candidates": "model_api_supply_plan_candidates_immutable",
	}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("immutable supply triggers=%v want=%v", found, want)
	}
}

func TestDeploymentLogicalModelDescriptionOmitsInternalWording(t *testing.T) {
	const internalDescription = "Created for legacy deployment API compatibility"
	const customerDescription = internalDescription + " for the coding team"
	ctx, databaseURL, _ := isolatedMigrationDatabase(t)
	previous, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	if _, err = previous.ExecContext(ctx, `CREATE TABLE schema_migrations(version TEXT PRIMARY KEY, checksum TEXT, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range embeddedMigrations(t) {
		if migration.name >= "079_" {
			break
		}
		tx, beginErr := previous.BeginTx(ctx, nil)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		if _, err = tx.ExecContext(ctx, string(migration.body)); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, migration.name, migration.checksum)
		}
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply migration %s: %v", migration.name, err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = previous.ExecContext(ctx, `INSERT INTO deployments(id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,tenant_id,created_at,updated_at) VALUES('existing','existing','org/model','vllm','round-robin','running','pending',1,1,false,'global',NOW(),NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err = previous.ExecContext(ctx, `INSERT INTO logical_models(id,tenant_id,name,description,created_at,updated_at) VALUES('custom','global','custom',$1,NOW(),NOW())`, customerDescription); err != nil {
		t.Fatal(err)
	}
	var description, previousSource string
	if err = previous.QueryRowContext(ctx, `SELECT description FROM logical_models WHERE id='existing-logical-model'`).Scan(&description); err != nil || description != internalDescription {
		t.Fatalf("description before upgrade=%q err=%v", description, err)
	}
	if err = previous.QueryRowContext(ctx, `SELECT prosrc FROM pg_proc WHERE oid=to_regprocedure('infercrane_create_legacy_endpoint()')`).Scan(&previousSource); err != nil {
		t.Fatal(err)
	}
	if err = previous.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, databaseURL, Options{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var source string
	if err = upgraded.QueryRowContext(ctx, `SELECT prosrc FROM pg_proc WHERE oid=to_regprocedure('infercrane_create_legacy_endpoint()')`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(previousSource, "'"+internalDescription+"'", "''", 1); want == previousSource || source != want {
		t.Fatalf("trigger function changed beyond its description:\n%s", source)
	}
	existing, err := upgraded.ResolveEndpointForTenant(ctx, "global", "existing")
	if err != nil || existing.LogicalModel.Description != "" {
		t.Fatalf("existing logical model=%#v err=%v", existing.LogicalModel, err)
	}
	custom, err := upgraded.LogicalModelForTenant(ctx, "global", "custom")
	if err != nil || custom.Description != customerDescription {
		t.Fatalf("customer description=%q err=%v", custom.Description, err)
	}
	target, err := upgraded.AddTarget(ctx, domain.Target{Name: "fresh-target", URL: "http://fresh-target", Provider: "existing", Runtime: "vllm", UpstreamModel: "upstream"})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := upgraded.CreateDeployment(ctx, domain.Deployment{Name: "fresh", Model: "org/model"}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	created, err := upgraded.ResolveEndpointForTenant(ctx, "global", deployment.Name)
	if err != nil || created.LogicalModel.Name != deployment.Name || created.LogicalModel.Description != "" {
		t.Fatalf("new logical model=%#v err=%v", created.LogicalModel, err)
	}
	if len(created.Bindings) != 1 || created.Bindings[0].DeploymentID != deployment.ID || created.ActivePlan.ID != deployment.ID+"-plan-1" {
		t.Fatalf("new endpoint bindings=%#v plan=%#v", created.Bindings, created.ActivePlan)
	}
	var remaining int
	if err = upgraded.QueryRowContext(ctx, `SELECT COUNT(*) FROM logical_models WHERE description=?`, internalDescription).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("internal descriptions remaining=%d err=%v", remaining, err)
	}
}
