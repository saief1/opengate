package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opengate-dev/opengate/internal/secure"
)

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

// DefaultOrgID is the organization every single-tenant install uses.
const DefaultOrgID = "org_default"

// ModelPolicy restricts which models a key/project may call and caps
// max_tokens. Patterns are exact names or prefixes ending in '*', matched
// against both the requested model/alias and the resolved provider/model.
type ModelPolicy struct {
	AllowedModels []string `json:"allowed_models"`
	DeniedModels  []string `json:"denied_models"`
	MaxTokens     int      `json:"max_tokens"`
}

func joinList(v []string) string {
	var out []string
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ",")
}

func splitList(v string) []string {
	if v == "" {
		return []string{}
	}
	return strings.Split(v, ",")
}

func isUnique(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key"))
}

// Project groups virtual keys, budgets and logs.
type Project struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	LogBodies bool      `json:"log_bodies"`
	Killed    bool      `json:"killed"`
	CreatedAt time.Time `json:"created_at"`
	ModelPolicy
	// Guardrails overrides the server-wide guardrail defaults (nil = defaults).
	Guardrails *GuardSettings `json:"guardrails"`
	// CacheTTLSeconds > 0 opts the project into exact-match response caching.
	CacheTTLSeconds int `json:"cache_ttl_seconds"`
}

// GuardSettings configures PII/secret redaction and prompt-injection
// detection. Modes: pii off|log|redact|block, injection off|log|block.
type GuardSettings struct {
	PII        string   `json:"pii"`
	Categories []string `json:"categories,omitempty"` // empty = all
	Responses  bool     `json:"responses"`            // also scan (non-streaming) responses
	Injection  string   `json:"injection"`
}

// SetProjectGuardrails stores (or with nil clears) a project's guardrail override.
func (s *Store) SetProjectGuardrails(ctx context.Context, id string, g *GuardSettings) error {
	v := ""
	if g != nil {
		b, err := json.Marshal(g)
		if err != nil {
			return err
		}
		v = string(b)
	}
	return s.execOne(ctx, `UPDATE projects SET guardrails=? WHERE id=?`, v, id)
}

// SetProjectCacheTTL sets the response cache TTL (0 disables caching).
func (s *Store) SetProjectCacheTTL(ctx context.Context, id string, seconds int) error {
	if seconds < 0 {
		return errors.New("cache_ttl_seconds must not be negative")
	}
	return s.execOne(ctx, `UPDATE projects SET cache_ttl_s=? WHERE id=?`, seconds, id)
}

// CreateProject inserts a new project.
func (s *Store) CreateProject(ctx context.Context, name string, logBodies bool) (*Project, error) {
	return s.CreateProjectInOrg(ctx, DefaultOrgID, name, logBodies)
}

// CreateProjectInOrg inserts a new project owned by an organization.
func (s *Store) CreateProjectInOrg(ctx context.Context, orgID, name string, logBodies bool) (*Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("project name is required")
	}
	if orgID == "" {
		orgID = DefaultOrgID
	}
	p := &Project{ID: secure.RandomID("prj_"), OrgID: orgID, Name: name, LogBodies: logBodies, CreatedAt: time.Now().UTC(),
		ModelPolicy: ModelPolicy{AllowedModels: []string{}, DeniedModels: []string{}}}
	_, err := s.exec(ctx, `INSERT INTO projects(id,org_id,name,log_bodies,killed,created_at) VALUES(?,?,?,?,0,?)`,
		p.ID, p.OrgID, p.Name, b2i(logBodies), ms(p.CreatedAt))
	if err != nil {
		if isUnique(err) {
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

const projectCols = `id,org_id,name,log_bodies,killed,created_at,allowed_models,denied_models,max_tokens,guardrails,cache_ttl_s`

func scanProject(sc interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var lb, k int
	var c int64
	var allow, deny, guard string
	if err := sc.Scan(&p.ID, &p.OrgID, &p.Name, &lb, &k, &c, &allow, &deny, &p.MaxTokens, &guard, &p.CacheTTLSeconds); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.LogBodies, p.Killed, p.CreatedAt = lb == 1, k == 1, fromMS(c)
	p.AllowedModels, p.DeniedModels = splitList(allow), splitList(deny)
	if guard != "" {
		var g GuardSettings
		if json.Unmarshal([]byte(guard), &g) == nil {
			p.Guardrails = &g
		}
	}
	return &p, nil
}

// GetProject fetches a project by id.
func (s *Store) GetProject(ctx context.Context, id string) (*Project, error) {
	return scanProject(s.queryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE id=?`, id))
}

// ProjectByName fetches a project by name in the default organization.
func (s *Store) ProjectByName(ctx context.Context, name string) (*Project, error) {
	return s.ProjectByNameInOrg(ctx, DefaultOrgID, name)
}

// ProjectByNameInOrg fetches a project by name within an organization.
func (s *Store) ProjectByNameInOrg(ctx context.Context, orgID, name string) (*Project, error) {
	return scanProject(s.queryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE org_id=? AND name=?`, orgID, name))
}

// SetProjectPolicy replaces a project's model policy.
func (s *Store) SetProjectPolicy(ctx context.Context, id string, p ModelPolicy) error {
	if p.MaxTokens < 0 {
		return errors.New("max_tokens must not be negative")
	}
	return s.execOne(ctx, `UPDATE projects SET allowed_models=?, denied_models=?, max_tokens=? WHERE id=?`,
		joinList(p.AllowedModels), joinList(p.DeniedModels), p.MaxTokens, id)
}

// ListProjects lists all projects (all organizations).
func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	return s.ListProjectsInOrg(ctx, "")
}

// ListProjectsInOrg lists projects of one organization ("" = all).
func (s *Store) ListProjectsInOrg(ctx context.Context, orgID string) ([]*Project, error) {
	q, args := `SELECT `+projectCols+` FROM projects`, []any{}
	if orgID != "" {
		q += ` WHERE org_id=?`
		args = append(args, orgID)
	}
	rows, err := s.query(ctx, q+` ORDER BY created_at`, args...)
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
	res, err := s.exec(ctx, q, args...)
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
	ModelPolicy
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
		Agent: strings.TrimSpace(agent), UserLabel: strings.TrimSpace(user), Prefix: secure.DisplayPrefix(plain), CreatedAt: time.Now().UTC(),
		ModelPolicy: ModelPolicy{AllowedModels: []string{}, DeniedModels: []string{}}}
	_, err = s.exec(ctx, `INSERT INTO virtual_keys(id,project_id,name,agent,user_label,key_hash,prefix,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		k.ID, k.ProjectID, k.Name, k.Agent, k.UserLabel, secure.HashKey(plain), k.Prefix, ms(k.CreatedAt))
	if err != nil {
		return "", nil, err
	}
	return plain, k, nil
}

const keyCols = `id,project_id,name,agent,user_label,prefix,killed,revoked,created_at,allowed_models,denied_models,max_tokens`

func scanKey(sc interface{ Scan(...any) error }) (*VirtualKey, error) {
	var k VirtualKey
	var killed, revoked int
	var c int64
	var allow, deny string
	if err := sc.Scan(&k.ID, &k.ProjectID, &k.Name, &k.Agent, &k.UserLabel, &k.Prefix, &killed, &revoked, &c, &allow, &deny, &k.MaxTokens); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	k.Killed, k.Revoked, k.CreatedAt = killed == 1, revoked == 1, fromMS(c)
	k.AllowedModels, k.DeniedModels = splitList(allow), splitList(deny)
	return &k, nil
}

// KeyByHash looks up a key by the hash of its plaintext.
func (s *Store) KeyByHash(ctx context.Context, hash string) (*VirtualKey, error) {
	return scanKey(s.queryRow(ctx, `SELECT `+keyCols+` FROM virtual_keys WHERE key_hash=?`, hash))
}

// GetKey fetches a key by id.
func (s *Store) GetKey(ctx context.Context, id string) (*VirtualKey, error) {
	return scanKey(s.queryRow(ctx, `SELECT `+keyCols+` FROM virtual_keys WHERE id=?`, id))
}

// ListKeys lists keys, optionally for a single project.
func (s *Store) ListKeys(ctx context.Context, projectID string) ([]*VirtualKey, error) {
	q := `SELECT ` + keyCols + ` FROM virtual_keys`
	var args []any
	if projectID != "" {
		q += ` WHERE project_id=?`
		args = append(args, projectID)
	}
	rows, err := s.query(ctx, q+` ORDER BY created_at`, args...)
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

// SetKeyPolicy replaces a key's model policy.
func (s *Store) SetKeyPolicy(ctx context.Context, id string, p ModelPolicy) error {
	if p.MaxTokens < 0 {
		return errors.New("max_tokens must not be negative")
	}
	return s.execOne(ctx, `UPDATE virtual_keys SET allowed_models=?, denied_models=?, max_tokens=? WHERE id=?`,
		joinList(p.AllowedModels), joinList(p.DeniedModels), p.MaxTokens, id)
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
	_, err := s.exec(ctx, `INSERT INTO limits(scope,scope_id,daily_usd,monthly_usd,total_usd,rpm,updated_at) VALUES(?,?,?,?,?,?,?)
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
	rows, err := s.query(ctx, `SELECT scope,scope_id,daily_usd,monthly_usd,total_usd,rpm,updated_at FROM limits ORDER BY scope,scope_id`)
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
	_, err = s.exec(ctx, `INSERT INTO provider_keys(id,provider,label,hint,ciphertext,created_at) VALUES(?,?,?,?,?,?)`,
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
	rows, err := s.query(ctx, q+` ORDER BY created_at DESC, id`, args...)
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

// ---------- Rate limit buckets ----------

// TakeToken atomically refills and consumes one token from a bucket. It
// returns whether a token was taken and the (refilled) token count before
// the attempt. The whole operation is one upsert, so concurrent replicas
// cannot double-spend a token.
func (s *Store) TakeToken(ctx context.Context, bucket string, capacity, ratePerMS float64, nowMS int64) (bool, float64, error) {
	refill := s.least() + `(excluded.capacity, rate_buckets.tokens + CAST((excluded.last_ms - rate_buckets.last_ms) AS DOUBLE PRECISION) * CAST(? AS DOUBLE PRECISION))`
	res, err := s.exec(ctx, `INSERT INTO rate_buckets(bucket,tokens,capacity,last_ms) VALUES(?,?,?,?)
ON CONFLICT(bucket) DO UPDATE SET tokens = `+refill+` - 1, capacity = excluded.capacity, last_ms = excluded.last_ms
WHERE `+refill+` >= 1`, bucket, capacity-1, capacity, nowMS, ratePerMS, ratePerMS)
	if err != nil {
		return false, 0, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, 0, nil
	}
	var tokens float64
	var last int64
	if err := s.queryRow(ctx, `SELECT tokens,last_ms FROM rate_buckets WHERE bucket=?`, bucket).Scan(&tokens, &last); err != nil {
		return false, 0, err
	}
	return false, tokens + float64(nowMS-last)*ratePerMS, nil
}

// PruneRateBuckets deletes buckets idle since before cutoff (they would be
// full again anyway).
func (s *Store) PruneRateBuckets(ctx context.Context, cutoff time.Time) error {
	_, err := s.exec(ctx, `DELETE FROM rate_buckets WHERE last_ms < ?`, ms(cutoff))
	return err
}
