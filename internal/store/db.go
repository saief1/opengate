// Package store persists OpenGate state in SQLite (default, via the pure-Go
// modernc.org/sqlite driver, no CGO) or PostgreSQL (via the pure-Go pgx
// driver). Both backends share one SQL dialect-neutral code path; the few
// differences (placeholders, LEAST/MIN, row locking) live in this file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	_ "modernc.org/sqlite"             // registers "sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Dialect identifies the SQL backend.
type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

// Store wraps the database.
type Store struct {
	db      *sql.DB
	dialect Dialect
}

// IsPostgresDSN reports whether a database setting points at PostgreSQL.
func IsPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// Open opens (and migrates) the database. dsn is either a SQLite file path
// (":memory:" for tests) or a postgres:// URL.
func Open(dsn string) (*Store, error) {
	var (
		db      *sql.DB
		err     error
		dialect = SQLite
	)
	if IsPostgresDSN(dsn) {
		dialect = Postgres
		db, err = sql.Open("pgx", dsn)
		if err == nil {
			db.SetMaxOpenConns(20)
			db.SetMaxIdleConns(10)
			db.SetConnMaxLifetime(30 * time.Minute)
		}
	} else {
		q := url.Values{}
		for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
			q.Add("_pragma", p)
		}
		db, err = sql.Open("sqlite", "file:"+dsn+"?"+q.Encode())
		if err == nil && dsn == ":memory:" {
			db.SetMaxOpenConns(1) // each connection would otherwise get its own memory DB
		}
	}
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, dialect: dialect}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Dialect returns the backend in use.
func (s *Store) Dialect() Dialect { return s.dialect }

// Ping checks connectivity (readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// rebind converts '?' placeholders to '$n' for PostgreSQL.
func (s *Store) rebind(q string) string {
	if s.dialect != Postgres {
		return q
	}
	var b strings.Builder
	n := 0
	inStr := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '\'' {
			inStr = !inStr
		}
		if c == '?' && !inStr {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.rebind(q), args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.rebind(q), args...)
}

// least returns the dialect's two-argument minimum function name.
func (s *Store) least() string {
	if s.dialect == Postgres {
		return "LEAST"
	}
	return "MIN"
}

// ---------- migrations ----------

// migration is a list of statements; dialect-specific statements are
// prefixed with "sqlite:" or "postgres:".
type migration struct {
	version int
	name    string
	stmts   []string
}

// Types: BIGINT for millisecond timestamps/counters and DOUBLE PRECISION for
// money work on both backends (SQLite maps them to INTEGER/REAL affinity).
var migrations = []migration{
	{1, "initial schema", []string{
		`CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  log_bodies INTEGER NOT NULL DEFAULT 0,
  killed INTEGER NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS virtual_keys (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  name TEXT NOT NULL,
  agent TEXT NOT NULL DEFAULT '',
  user_label TEXT NOT NULL DEFAULT '',
  key_hash TEXT NOT NULL UNIQUE,
  prefix TEXT NOT NULL,
  killed INTEGER NOT NULL DEFAULT 0,
  revoked INTEGER NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS limits (
  scope TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  daily_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
  monthly_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
  total_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
  rpm INTEGER NOT NULL DEFAULT 0,
  updated_at BIGINT NOT NULL,
  PRIMARY KEY (scope, scope_id)
)`,
		`CREATE TABLE IF NOT EXISTS provider_keys (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  hint TEXT NOT NULL DEFAULT '',
  ciphertext TEXT NOT NULL,
  created_at BIGINT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS request_logs (
  id TEXT PRIMARY KEY,
  ts BIGINT NOT NULL,
  project_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  agent TEXT NOT NULL DEFAULT '',
  user_label TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  provider_model TEXT NOT NULL DEFAULT '',
  prompt_tokens BIGINT NOT NULL DEFAULT 0,
  completion_tokens BIGINT NOT NULL DEFAULT 0,
  cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
  latency_ms BIGINT NOT NULL DEFAULT 0,
  status INTEGER NOT NULL,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  stream INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  usage_estimated INTEGER NOT NULL DEFAULT 0,
  request_body TEXT,
  response_body TEXT
)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_ts ON request_logs(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_key_ts ON request_logs(key_id, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_project_ts ON request_logs(project_id, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_agent_ts ON request_logs(project_id, agent, ts)`,
		`CREATE TABLE IF NOT EXISTS alerts_sent (
  scope TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  period TEXT NOT NULL,
  period_start BIGINT NOT NULL,
  threshold DOUBLE PRECISION NOT NULL,
  sent_at BIGINT NOT NULL,
  PRIMARY KEY (scope, scope_id, period, period_start, threshold)
)`,
	}},
	{2, "policies, cache tokens, shared rate limits, orgs on projects", []string{
		`ALTER TABLE projects ADD COLUMN org_id TEXT NOT NULL DEFAULT 'org_default'`,
		`ALTER TABLE projects ADD COLUMN allowed_models TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN denied_models TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN max_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE virtual_keys ADD COLUMN allowed_models TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE virtual_keys ADD COLUMN denied_models TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE virtual_keys ADD COLUMN max_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE request_logs ADD COLUMN cached_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE request_logs ADD COLUMN cache_write_tokens BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE request_logs ADD COLUMN endpoint TEXT NOT NULL DEFAULT 'chat'`,
		`ALTER TABLE request_logs ADD COLUMN customer_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_logs ADD COLUMN route TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_logs ADD COLUMN baseline_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0`,
		`ALTER TABLE request_logs ADD COLUMN billed_usd DOUBLE PRECISION NOT NULL DEFAULT 0`,
		`ALTER TABLE request_logs ADD COLUMN cache_hit INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE request_logs ADD COLUMN guard TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_logs ADD COLUMN guard_action TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_logs_customer_ts ON request_logs(project_id, customer_id, ts)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_org_name ON projects(org_id, name)`,
		`CREATE TABLE IF NOT EXISTS rate_buckets (
  bucket TEXT PRIMARY KEY,
  tokens DOUBLE PRECISION NOT NULL,
  capacity DOUBLE PRECISION NOT NULL,
  last_ms BIGINT NOT NULL
)`,
	}},
	{3, "routing rules, guardrails, customers and billing, response cache", []string{
		`ALTER TABLE projects ADD COLUMN guardrails TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN cache_ttl_s BIGINT NOT NULL DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS routing_rules (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL,
  project_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 100,
  enabled INTEGER NOT NULL DEFAULT 1,
  match_json TEXT NOT NULL,
  target TEXT NOT NULL,
  fallbacks TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL
)`,
		`CREATE INDEX IF NOT EXISTS idx_routing_org ON routing_rules(org_id, priority)`,
		`CREATE TABLE IF NOT EXISTS customers (
  project_id TEXT NOT NULL,
  id TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  stripe_customer_id TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  PRIMARY KEY (project_id, id)
)`,
		`CREATE TABLE IF NOT EXISTS markup_rules (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL,
  project_id TEXT NOT NULL DEFAULT '',
  customer_id TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  markup_pct DOUBLE PRECISION NOT NULL,
  created_at BIGINT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS stripe_sync (
  project_id TEXT NOT NULL,
  customer_id TEXT NOT NULL,
  synced_to BIGINT NOT NULL,
  remainder DOUBLE PRECISION NOT NULL DEFAULT 0,
  last_value BIGINT NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at BIGINT NOT NULL,
  PRIMARY KEY (project_id, customer_id)
)`,
		`CREATE TABLE IF NOT EXISTS response_cache (
  cache_key TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  endpoint TEXT NOT NULL,
  provider TEXT NOT NULL,
  provider_model TEXT NOT NULL,
  body TEXT NOT NULL,
  prompt_tokens BIGINT NOT NULL DEFAULT 0,
  completion_tokens BIGINT NOT NULL DEFAULT 0,
  cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
  hits BIGINT NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL
)`,
		`CREATE INDEX IF NOT EXISTS idx_cache_expires ON response_cache(expires_at)`,
	}},
	{4, "organizations, users, sessions, invites, audit log, per-org provider keys", []string{
		`CREATE TABLE IF NOT EXISTS orgs (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  created_at BIGINT NOT NULL
)`,
		`INSERT INTO orgs(id,name,created_at) VALUES('org_default','Default',0) ON CONFLICT DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL,
  last_login_at BIGINT NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS memberships (
  org_id TEXT NOT NULL REFERENCES orgs(id),
  user_id TEXT NOT NULL REFERENCES users(id),
  role TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (org_id, user_id)
)`,
		`CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id),
  org_id TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL,
  last_seen_at BIGINT NOT NULL,
  ip TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT ''
)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
		`CREATE TABLE IF NOT EXISTS invites (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL REFERENCES orgs(id),
  email TEXT NOT NULL,
  role TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  created_by TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL,
  accepted_at BIGINT NOT NULL DEFAULT 0
)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
  id TEXT PRIMARY KEY,
  org_id TEXT NOT NULL,
  ts BIGINT NOT NULL,
  actor_type TEXT NOT NULL,
  actor_id TEXT NOT NULL DEFAULT '',
  actor_email TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  status INTEGER NOT NULL DEFAULT 0,
  ip TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_org_ts ON audit_log(org_id, ts)`,
		`ALTER TABLE provider_keys ADD COLUMN org_id TEXT NOT NULL DEFAULT 'org_default'`,
	}},
	{5, "security hardening: passthrough feature allowlist, customer attribution controls", []string{
		`ALTER TABLE projects ADD COLUMN allowed_features TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN trust_customer_header INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE virtual_keys ADD COLUMN customer_id TEXT NOT NULL DEFAULT ''`,
	}},
	{6, "cloud: org plans and subscriptions, monthly usage counters, Stripe webhook idempotency", []string{
		`CREATE TABLE IF NOT EXISTS org_billing (
  org_id TEXT PRIMARY KEY REFERENCES orgs(id),
  plan TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  stripe_customer_id TEXT NOT NULL DEFAULT '',
  stripe_subscription_id TEXT NOT NULL DEFAULT '',
  current_period_end BIGINT NOT NULL DEFAULT 0,
  cancel_at_period_end INTEGER NOT NULL DEFAULT 0,
  past_due_since BIGINT NOT NULL DEFAULT 0,
  updated_at BIGINT NOT NULL
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_org_billing_customer ON org_billing(stripe_customer_id) WHERE stripe_customer_id <> ''`,
		`CREATE TABLE IF NOT EXISTS org_usage (
  org_id TEXT NOT NULL,
  period TEXT NOT NULL,
  requests BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (org_id, period)
)`,
		`CREATE TABLE IF NOT EXISTS stripe_events (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  received_at BIGINT NOT NULL
)`,
	}},
}

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if s.dialect == Postgres {
		// Serialize concurrent migrators (several replicas starting at once).
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(726374)`); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(726374)`)
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)`); err != nil {
		return err
	}
	applied := map[int]bool{}
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, st := range m.stmts {
			if rest, ok := strings.CutPrefix(st, "sqlite:"); ok {
				if s.dialect != SQLite {
					continue
				}
				st = rest
			} else if rest, ok := strings.CutPrefix(st, "postgres:"); ok {
				if s.dialect != Postgres {
					continue
				}
				st = rest
			}
			if _, err := tx.ExecContext(ctx, st); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d (%s): %w\n%s", m.version, m.name, err, st)
			}
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)`), m.version, m.name, time.Now().UnixMilli()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the newest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.queryRow(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return int(v.Int64), err
}
