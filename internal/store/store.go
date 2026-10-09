// Package store persists OpenGate state in SQLite using the pure-Go
// modernc.org/sqlite driver (no CGO, so the binary stays static).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/opengate-dev/opengate/internal/secure"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1) // each connection would otherwise get its own memory DB
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const schemaV1 = `
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  log_bodies INTEGER NOT NULL DEFAULT 0,
  killed INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS virtual_keys (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  name TEXT NOT NULL,
  agent TEXT NOT NULL DEFAULT '',
  user_label TEXT NOT NULL DEFAULT '',
  key_hash TEXT NOT NULL UNIQUE,
  prefix TEXT NOT NULL,
  killed INTEGER NOT NULL DEFAULT 0,
  revoked INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS limits (
  scope TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  daily_usd REAL NOT NULL DEFAULT 0,
  monthly_usd REAL NOT NULL DEFAULT 0,
  total_usd REAL NOT NULL DEFAULT 0,
  rpm INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (scope, scope_id)
);
CREATE TABLE IF NOT EXISTS provider_keys (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  hint TEXT NOT NULL DEFAULT '',
  ciphertext TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS request_logs (
  id TEXT PRIMARY KEY,
  ts INTEGER NOT NULL,
  project_id TEXT NOT NULL,
  key_id TEXT NOT NULL,
  agent TEXT NOT NULL DEFAULT '',
  user_label TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  provider_model TEXT NOT NULL DEFAULT '',
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  status INTEGER NOT NULL,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  stream INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  usage_estimated INTEGER NOT NULL DEFAULT 0,
  request_body TEXT,
  response_body TEXT
);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON request_logs(ts);
CREATE INDEX IF NOT EXISTS idx_logs_key_ts ON request_logs(key_id, ts);
CREATE INDEX IF NOT EXISTS idx_logs_project_ts ON request_logs(project_id, ts);
CREATE INDEX IF NOT EXISTS idx_logs_agent_ts ON request_logs(project_id, agent, ts);
CREATE TABLE IF NOT EXISTS alerts_sent (
  scope TEXT NOT NULL,
  scope_id TEXT NOT NULL,
  period TEXT NOT NULL,
  period_start INTEGER NOT NULL,
  threshold REAL NOT NULL,
  sent_at INTEGER NOT NULL,
  PRIMARY KEY (scope, scope_id, period, period_start, threshold)
);
`

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v < 1 {
		if _, err := s.db.ExecContext(ctx, schemaV1); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }
func fromMS(v int64) time.Time {
	return time.UnixMilli(v).UTC()
}
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- Projects ----------

// Project groups virtual keys, budgets and logs.
type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	LogBodies bool      `json:"log_bodies"`
	Killed    bool      `json:"killed"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateProject inserts a new project.
func (s *Store) CreateProject(ctx context.Context, name string, logBodies bool) (*Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("project name is required")
	}
	p := &Project{ID: secure.RandomID("prj_"), Name: name, LogBodies: logBodies, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO projects(id,name,log_bodies,killed,created_at) VALUES(?,?,?,0,?)`,
		p.ID, p.Name, b2i(logBodies), ms(p.CreatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("project %q already exists", name)
		}
		return nil, err
	}
	return p, nil
}

// EnsureProject returns the project with the given name, creating it if needed.
func (s *Store) EnsureProject(ctx context.Context, name string) (*Project, error) {
	p, err := s.ProjectByName(ctx, name)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return s.CreateProject(ctx, name, false)
}

const projectCols = `id,name,log_bodies,killed,created_at`

func scanProject(sc interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var lb, k int
	var c int64
	if err := sc.Scan(&p.ID, &p.Name, &lb, &k, &c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.LogBodies, p.Killed, p.CreatedAt = lb == 1, k == 1, fromMS(c)
	return &p, nil
}

// GetProject fetches a project by id.
func (s *Store) GetProject(ctx context.Context, id string) (*Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id=?`, id))
}

// ProjectByName fetches a project by name.
func (s *Store) ProjectByName(ctx context.Context, name string) (*Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE name=?`, name))
}

// ListProjects lists all projects.
func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProjectLogBodies toggles opt-in body logging.
func (s *Store) SetProjectLogBodies(ctx context.Context, id string, on bool) error {
	return s.execOne(ctx, `UPDATE projects SET log_bodies=? WHERE id=?`, b2i(on), id)
}

// SetProjectKilled flips the project kill switch.
func (s *Store) SetProjectKilled(ctx context.Context, id string, killed bool) error {
	return s.execOne(ctx, `UPDATE projects SET killed=? WHERE id=?`, b2i(killed), id)
}

func (s *Store) execOne(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- Virtual keys ----------

// VirtualKey is a client credential. The plaintext is never stored.
type VirtualKey struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	Agent     string    `json:"agent"`
	UserLabel string    `json:"user"`
	Prefix    string    `json:"prefix"`
	Killed    bool      `json:"killed"`
	Revoked   bool      `json:"revoked"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateKey generates a virtual key; the plaintext is returned once.
func (s *Store) CreateKey(ctx context.Context, projectID, name, agent, user string) (string, *VirtualKey, error) {
	if _, err := s.GetProject(ctx, projectID); err != nil {
		return "", nil, fmt.Errorf("project %s: %w", projectID, err)
	}
	plain, err := secure.GenerateVirtualKey()
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(name) == "" {
		name = "key"
	}
	k := &VirtualKey{ID: secure.RandomID("key_"), ProjectID: projectID, Name: strings.TrimSpace(name),
		Agent: strings.TrimSpace(agent), UserLabel: strings.TrimSpace(user), Prefix: secure.DisplayPrefix(plain), CreatedAt: time.Now().UTC()}
	_, err = s.db.ExecContext(ctx, `INSERT INTO virtual_keys(id,project_id,name,agent,user_label,key_hash,prefix,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		k.ID, k.ProjectID, k.Name, k.Agent, k.UserLabel, secure.HashKey(plain), k.Prefix, ms(k.CreatedAt))
	if err != nil {
		return "", nil, err
	}
	return plain, k, nil
}

const keyCols = `id,project_id,name,agent,user_label,prefix,killed,revoked,created_at`

func scanKey(sc interface{ Scan(...any) error }) (*VirtualKey, error) {
	var k VirtualKey
	var killed, revoked int
	var c int64
	if err := sc.Scan(&k.ID, &k.ProjectID, &k.Name, &k.Agent, &k.UserLabel, &k.Prefix, &killed, &revoked, &c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	k.Killed, k.Revoked, k.CreatedAt = killed == 1, revoked == 1, fromMS(c)
	return &k, nil
}

// KeyByHash looks up a key by the hash of its plaintext.
func (s *Store) KeyByHash(ctx context.Context, hash string) (*VirtualKey, error) {
	return scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM virtual_keys WHERE key_hash=?`, hash))
}

// GetKey fetches a key by id.
func (s *Store) GetKey(ctx context.Context, id string) (*VirtualKey, error) {
	return scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyCols+` FROM virtual_keys WHERE id=?`, id))
}

// ListKeys lists keys, optionally for a single project.
func (s *Store) ListKeys(ctx context.Context, projectID string) ([]*VirtualKey, error) {
	q := `SELECT ` + keyCols + ` FROM virtual_keys`
	var args []any
	if projectID != "" {
		q += ` WHERE project_id=?`
		args = append(args, projectID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*VirtualKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetKeyKilled flips a key's kill switch.
func (s *Store) SetKeyKilled(ctx context.Context, id string, killed bool) error {
	return s.execOne(ctx, `UPDATE virtual_keys SET killed=? WHERE id=?`, b2i(killed), id)
}

// RevokeKey permanently disables a key.
func (s *Store) RevokeKey(ctx context.Context, id string) error {
	return s.execOne(ctx, `UPDATE virtual_keys SET revoked=1 WHERE id=?`, id)
}

// ---------- Limits ----------

// Scope kinds for limits and spend.
const (
	ScopeKey     = "key"
	ScopeAgent   = "agent"   // scope_id = project_id + "/" + agent label
	ScopeProject = "project" // scope_id = project_id
)

// Limit holds spend (USD) and rate limits for a scope. Zero means unlimited.
type Limit struct {
	Scope      string    `json:"scope"`
	ScopeID    string    `json:"scope_id"`
	DailyUSD   float64   `json:"daily_usd"`
	MonthlyUSD float64   `json:"monthly_usd"`
	TotalUSD   float64   `json:"total_usd"`
	RPM        int       `json:"rpm"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ValidScope reports whether scope is a known kind.
func ValidScope(scope string) bool {
	return scope == ScopeKey || scope == ScopeAgent || scope == ScopeProject
}

// UpsertLimit creates or replaces a limit.
func (s *Store) UpsertLimit(ctx context.Context, l Limit) error {
	if !ValidScope(l.Scope) || l.ScopeID == "" {
		return errors.New("limit needs scope (key|agent|project) and scope_id")
	}
	if l.DailyUSD < 0 || l.MonthlyUSD < 0 || l.TotalUSD < 0 || l.RPM < 0 {
		return errors.New("limits must not be negative")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO limits(scope,scope_id,daily_usd,monthly_usd,total_usd,rpm,updated_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(scope,scope_id) DO UPDATE SET daily_usd=excluded.daily_usd, monthly_usd=excluded.monthly_usd, total_usd=excluded.total_usd, rpm=excluded.rpm, updated_at=excluded.updated_at`,
		l.Scope, l.ScopeID, l.DailyUSD, l.MonthlyUSD, l.TotalUSD, l.RPM, ms(time.Now()))
	return err
}

// DeleteLimit removes a limit.
func (s *Store) DeleteLimit(ctx context.Context, scope, scopeID string) error {
	return s.execOne(ctx, `DELETE FROM limits WHERE scope=? AND scope_id=?`, scope, scopeID)
}

// ListLimits returns all limits.
func (s *Store) ListLimits(ctx context.Context) ([]Limit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope,scope_id,daily_usd,monthly_usd,total_usd,rpm,updated_at FROM limits ORDER BY scope,scope_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Limit
	for rows.Next() {
		var l Limit
		var u int64
		if err := rows.Scan(&l.Scope, &l.ScopeID, &l.DailyUSD, &l.MonthlyUSD, &l.TotalUSD, &l.RPM, &u); err != nil {
			return nil, err
		}
		l.UpdatedAt = fromMS(u)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---------- Provider keys ----------

// ProviderKey is an encrypted upstream credential.
type ProviderKey struct {
	ID         string    `json:"id"`
	Provider   string    `json:"provider"`
	Label      string    `json:"label"`
	Hint       string    `json:"hint"`
	Ciphertext string    `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
}

// ProviderKeyAAD binds a ciphertext to its row.
func ProviderKeyAAD(id, provider string) []byte {
	return []byte("opengate/provider-key/" + id + "/" + provider)
}

// AddProviderKey encrypts and stores an upstream key.
func (s *Store) AddProviderKey(ctx context.Context, c *secure.Cipher, provider, label, plaintext string) (*ProviderKey, error) {
	plaintext = strings.TrimSpace(plaintext)
	if provider == "" || plaintext == "" {
		return nil, errors.New("provider and key are required")
	}
	pk := &ProviderKey{ID: secure.RandomID("pk_"), Provider: provider, Label: strings.TrimSpace(label), CreatedAt: time.Now().UTC()}
	if len(plaintext) > 8 {
		pk.Hint = "…" + plaintext[len(plaintext)-4:]
	}
	ct, err := c.Encrypt([]byte(plaintext), ProviderKeyAAD(pk.ID, provider))
	if err != nil {
		return nil, err
	}
	pk.Ciphertext = ct
	_, err = s.db.ExecContext(ctx, `INSERT INTO provider_keys(id,provider,label,hint,ciphertext,created_at) VALUES(?,?,?,?,?,?)`,
		pk.ID, pk.Provider, pk.Label, pk.Hint, pk.Ciphertext, ms(pk.CreatedAt))
	if err != nil {
		return nil, err
	}
	return pk, nil
}

// ListProviderKeys lists keys (newest first), optionally filtered by provider.
func (s *Store) ListProviderKeys(ctx context.Context, provider string) ([]*ProviderKey, error) {
	q := `SELECT id,provider,label,hint,ciphertext,created_at FROM provider_keys`
	var args []any
	if provider != "" {
		q += ` WHERE provider=?`
		args = append(args, provider)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProviderKey
	for rows.Next() {
		var pk ProviderKey
		var c int64
		if err := rows.Scan(&pk.ID, &pk.Provider, &pk.Label, &pk.Hint, &pk.Ciphertext, &c); err != nil {
			return nil, err
		}
		pk.CreatedAt = fromMS(c)
		out = append(out, &pk)
	}
	return out, rows.Err()
}

// DeleteProviderKey removes an upstream key.
func (s *Store) DeleteProviderKey(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM provider_keys WHERE id=?`, id)
}
