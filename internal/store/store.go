package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var ErrNotFound = domain.ErrNotFound
var ErrConflict = domain.ErrConflict

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct{ db *sql.DB }

type Options struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

func Open(ctx context.Context, databaseURL string, options Options) (*Store, error) {
	if databaseURL == "" {
		return nil, errors.New("database URL is required")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if options.MaxOpenConns <= 0 {
		options.MaxOpenConns = 32
	}
	if options.MaxIdleConns < 0 {
		options.MaxIdleConns = 0
	}
	if options.MaxIdleConns == 0 {
		options.MaxIdleConns = 8
	}
	if options.ConnMaxLifetime <= 0 {
		options.ConnMaxLifetime = 30 * time.Minute
	}
	if options.ConnMaxIdleTime <= 0 {
		options.ConnMaxIdleTime = 5 * time.Minute
	}
	db.SetMaxOpenConns(options.MaxOpenConns)
	db.SetMaxIdleConns(options.MaxIdleConns)
	db.SetConnMaxLifetime(options.ConnMaxLifetime)
	db.SetConnMaxIdleTime(options.ConnMaxIdleTime)
	s := &Store{db: db}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(370761934109860021)`); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(370761934109860021)`)
	if _, err = conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version TEXT PRIMARY KEY, checksum TEXT, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	type migration struct {
		name, checksum string
		body           []byte
	}
	available := make([]migration, 0, len(entries))
	byName := make(map[string]migration, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		item := migration{name: entry.Name(), checksum: hex.EncodeToString(digest[:]), body: body}
		available = append(available, item)
		byName[item.name] = item
	}
	applied := make(map[string]string, len(available))
	rows, err := conn.QueryContext(ctx, `SELECT version,COALESCE(checksum,'') FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var version, checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return err
		}
		if _, ok := byName[version]; !ok {
			rows.Close()
			return fmt.Errorf("database schema migration %q is newer than or unknown to this InferCrane binary", version)
		}
		applied[version] = checksum
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	missing := false
	for _, item := range available {
		checksum, exists := applied[item.name]
		if !exists {
			missing = true
			continue
		}
		if missing {
			return fmt.Errorf("database migration history is non-contiguous at %q", item.name)
		}
		if checksum == "" {
			if _, err = conn.ExecContext(ctx, `UPDATE schema_migrations SET checksum=$1 WHERE version=$2 AND checksum IS NULL`, item.checksum, item.name); err != nil {
				return err
			}
		} else if checksum != item.checksum {
			return fmt.Errorf("database migration %q checksum does not match this InferCrane binary", item.name)
		}
	}
	for _, item := range available {
		if _, exists := applied[item.name]; exists {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(item.body)); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, item.name, item.checksum)
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", item.name, err)
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, `ALTER TABLE schema_migrations ALTER COLUMN checksum SET NOT NULL`); err != nil {
		return err
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}
func null(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// rebind keeps query text readable while translating database/sql placeholders
// to PostgreSQL's positional parameters. Question marks are only used as
// placeholders in this package, never inside SQL string literals.
func rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 1
	for _, r := range query {
		if r == '?' {
			fmt.Fprintf(&b, "$%d", n)
			n++
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func (s *Store) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, rebind(q), args...)
}
func (s *Store) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, rebind(q), args...)
}
func (s *Store) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, rebind(q), args...)
}

type tx struct{ *sql.Tx }

func (s *Store) beginTx(ctx context.Context) (*tx, error) {
	inner, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &tx{inner}, nil
}
func (t *tx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, rebind(q), args...)
}
func (t *tx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, rebind(q), args...)
}

func (s *Store) AddTarget(ctx context.Context, target domain.Target) (domain.Target, error) {
	return s.AddTargetForTenant(ctx, "global", target)
}
func (s *Store) AddTargetForTenant(ctx context.Context, tenant string, target domain.Target) (domain.Target, error) {
	target.URL = NormalizeURL(target.URL)
	var existing domain.Target
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,name,url,provider,runtime,COALESCE(upstream_model_name,''),health,COALESCE(provider_resource_id,''),COALESCE(provider_details_json::text,''),created_at,updated_at FROM targets WHERE tenant_id=? AND (name=? OR url=?)`, tenant, target.Name, target.URL).Scan(&existing.ID, &existing.Name, &existing.URL, &existing.Provider, &existing.Runtime, &existing.UpstreamModel, &existing.Health, &existing.ProviderResourceID, &existing.ProviderDetails, &created, &updated)
	if err == nil {
		existing.CreatedAt, existing.UpdatedAt = parseTime(created), parseTime(updated)
		// Empty optional metadata means "unspecified" on an idempotent retry. An
		// adoption or observation may have enriched the target after its original
		// registration; a later bootstrap must not erase that metadata or fail just
		// because it did not repeat it. Explicit contradictory metadata remains a
		// conflict.
		upstreamCompatible := target.UpstreamModel == "" || existing.UpstreamModel == target.UpstreamModel
		if existing.Name == target.Name && existing.URL == target.URL && existing.Runtime == target.Runtime && existing.Provider == target.Provider && upstreamCompatible {
			return existing, nil
		}
		return domain.Target{}, fmt.Errorf("%w: target name or URL already registered", ErrConflict)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Target{}, err
	}
	id, err := newID()
	if err != nil {
		return domain.Target{}, err
	}
	target.ID = id
	target.Health = "starting"
	stamp := now()
	target.CreatedAt, target.UpdatedAt = parseTime(stamp), parseTime(stamp)
	_, err = s.ExecContext(ctx, `INSERT INTO targets(id,name,url,provider,runtime,upstream_model_name,health,provider_resource_id,provider_details_json,tenant_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?::jsonb,?,?,?)`, target.ID, target.Name, target.URL, target.Provider, target.Runtime, null(target.UpstreamModel), target.Health, null(target.ProviderResourceID), nullJSON(target.ProviderDetails), tenant, stamp, stamp)
	if isUniqueViolation(err) {
		return domain.Target{}, fmt.Errorf("%w: target name or URL already registered", ErrConflict)
	}
	return target, err
}

func (s *Store) TargetForTenantByName(ctx context.Context, tenant, name string) (domain.Target, error) {
	var target domain.Target
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,name,url,provider,runtime,COALESCE(upstream_model_name,''),health,COALESCE(provider_resource_id,''),COALESCE(provider_details_json::text,''),created_at,updated_at FROM targets WHERE tenant_id=? AND name=?`, tenant, name).Scan(&target.ID, &target.Name, &target.URL, &target.Provider, &target.Runtime, &target.UpstreamModel, &target.Health, &target.ProviderResourceID, &target.ProviderDetails, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Target{}, ErrNotFound
	}
	if err != nil {
		return domain.Target{}, err
	}
	target.CreatedAt, target.UpdatedAt = parseTime(created), parseTime(updated)
	return target, nil
}

func (s *Store) TargetForTenantByID(ctx context.Context, tenant, id string) (domain.Target, error) {
	var target domain.Target
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,name,url,provider,runtime,COALESCE(upstream_model_name,''),health,COALESCE(provider_resource_id,''),COALESCE(provider_details_json::text,''),created_at,updated_at FROM targets WHERE tenant_id=? AND id=?`, tenant, id).Scan(&target.ID, &target.Name, &target.URL, &target.Provider, &target.Runtime, &target.UpstreamModel, &target.Health, &target.ProviderResourceID, &target.ProviderDetails, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Target{}, ErrNotFound
	}
	if err != nil {
		return domain.Target{}, err
	}
	target.CreatedAt, target.UpdatedAt = parseTime(created), parseTime(updated)
	return target, nil
}
func nullJSON(value string) any {
	if value == "" {
		return "null"
	}
	return value
}
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
