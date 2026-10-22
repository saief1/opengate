package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/secure"
)

// RouteMatch holds the conditions of a routing rule. All set conditions must
// hold; an empty match matches every request.
type RouteMatch struct {
	// Models are requested-model patterns ("openai/*", "gpt-4o", "smart").
	Models []string `json:"models,omitempty"`
	// MinPromptTokens / MaxPromptTokens bound the estimated prompt size.
	MinPromptTokens int `json:"min_prompt_tokens,omitempty"`
	MaxPromptTokens int `json:"max_prompt_tokens,omitempty"`
	// Tags must all be present in the X-OpenGate-Tags header.
	Tags []string `json:"tags,omitempty"`
	// Headers must match exactly (header names are case-insensitive).
	Headers map[string]string `json:"headers,omitempty"`
	// Agents restricts the rule to keys with these agent labels.
	Agents []string `json:"agents,omitempty"`
	// EstCostAboveUSD is a cost ceiling: the rule fires when the request's
	// estimated cost on the requested model exceeds this amount.
	EstCostAboveUSD float64 `json:"est_cost_above_usd,omitempty"`
}

// RoutingRule rewrites the target of matching chat requests.
type RoutingRule struct {
	ID        string     `json:"id"`
	OrgID     string     `json:"org_id"`
	ProjectID string     `json:"project_id"` // "" = every project in the org
	Name      string     `json:"name"`
	Priority  int        `json:"priority"` // lower runs first
	Enabled   bool       `json:"enabled"`
	Match     RouteMatch `json:"match"`
	Target    string     `json:"target"`    // provider/model, alias or "auto"
	Fallbacks []string   `json:"fallbacks"` // tried after the target's own list
	CreatedAt time.Time  `json:"created_at"`
}

func (r *RoutingRule) validate() error {
	r.Name, r.Target = strings.TrimSpace(r.Name), strings.TrimSpace(r.Target)
	if r.Name == "" || r.Target == "" {
		return errors.New("routing rule needs a name and a target")
	}
	if r.Match.MinPromptTokens < 0 || r.Match.MaxPromptTokens < 0 || r.Match.EstCostAboveUSD < 0 {
		return errors.New("routing thresholds must not be negative")
	}
	if r.Match.MaxPromptTokens > 0 && r.Match.MinPromptTokens > r.Match.MaxPromptTokens {
		return errors.New("min_prompt_tokens must be <= max_prompt_tokens")
	}
	if r.OrgID == "" {
		r.OrgID = DefaultOrgID
	}
	return nil
}

// UpsertRoutingRule creates (empty ID) or replaces a rule.
func (s *Store) UpsertRoutingRule(ctx context.Context, r *RoutingRule) error {
	if err := r.validate(); err != nil {
		return err
	}
	m, _ := json.Marshal(r.Match)
	if r.ID == "" {
		r.ID, r.CreatedAt = secure.RandomID("rr_"), time.Now().UTC()
		_, err := s.exec(ctx, `INSERT INTO routing_rules(id,org_id,project_id,name,priority,enabled,match_json,target,fallbacks,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.OrgID, r.ProjectID, r.Name, r.Priority, b2i(r.Enabled), string(m), r.Target, joinList(r.Fallbacks), ms(r.CreatedAt))
		return err
	}
	return s.execOne(ctx, `UPDATE routing_rules SET project_id=?, name=?, priority=?, enabled=?, match_json=?, target=?, fallbacks=? WHERE id=? AND org_id=?`,
		r.ProjectID, r.Name, r.Priority, b2i(r.Enabled), string(m), r.Target, joinList(r.Fallbacks), r.ID, r.OrgID)
}

// ListRoutingRules returns an org's rules ordered by priority ("" = all orgs).
func (s *Store) ListRoutingRules(ctx context.Context, orgID string) ([]*RoutingRule, error) {
	q, args := `SELECT id,org_id,project_id,name,priority,enabled,match_json,target,fallbacks,created_at FROM routing_rules`, []any{}
	if orgID != "" {
		q, args = q+` WHERE org_id=?`, append(args, orgID)
	}
	rows, err := s.query(ctx, q+` ORDER BY priority, created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RoutingRule{}
	for rows.Next() {
		var r RoutingRule
		var en int
		var m, fb string
		var c int64
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.Name, &r.Priority, &en, &m, &r.Target, &fb, &c); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(m), &r.Match)
		r.Enabled, r.Fallbacks, r.CreatedAt = en == 1, splitList(fb), fromMS(c)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DeleteRoutingRule removes a rule of an org.
func (s *Store) DeleteRoutingRule(ctx context.Context, orgID, id string) error {
	return s.execOne(ctx, `DELETE FROM routing_rules WHERE id=? AND org_id=?`, id, orgID)
}

// ---------- customers & markup ----------

// Customer is end-customer metadata of an AI app (usage itself lives in
// request_logs.customer_id).
type Customer struct {
	ProjectID        string    `json:"project_id"`
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	StripeCustomerID string    `json:"stripe_customer_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// UpsertCustomer stores customer metadata. Setting a Stripe customer ID for
// the first time starts Stripe usage sync from now (no backfill).
func (s *Store) UpsertCustomer(ctx context.Context, c *Customer) error {
	c.ID, c.StripeCustomerID = strings.TrimSpace(c.ID), strings.TrimSpace(c.StripeCustomerID)
	if c.ProjectID == "" || c.ID == "" {
		return errors.New("project_id and customer id are required")
	}
	now := time.Now().UTC()
	c.UpdatedAt = now
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	_, err := s.exec(ctx, `INSERT INTO customers(project_id,id,name,stripe_customer_id,created_at,updated_at) VALUES(?,?,?,?,?,?)
ON CONFLICT(project_id,id) DO UPDATE SET name=excluded.name, stripe_customer_id=excluded.stripe_customer_id, updated_at=excluded.updated_at`,
		c.ProjectID, c.ID, c.Name, c.StripeCustomerID, ms(c.CreatedAt), ms(now))
	if err != nil || c.StripeCustomerID == "" {
		return err
	}
	_, err = s.exec(ctx, `INSERT INTO stripe_sync(project_id,customer_id,synced_to,remainder,updated_at) VALUES(?,?,?,0,?) ON CONFLICT DO NOTHING`,
		c.ProjectID, c.ID, ms(now), ms(now))
	return err
}

// ListCustomerMeta returns metadata rows for the given projects ("" = all).
func (s *Store) ListCustomerMeta(ctx context.Context, projectIDs []string) ([]*Customer, error) {
	q, args := `SELECT project_id,id,name,stripe_customer_id,created_at,updated_at FROM customers`, []any{}
	if len(projectIDs) > 0 {
		q += ` WHERE project_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(projectIDs)), ",") + `)`
		for _, p := range projectIDs {
			args = append(args, p)
		}
	}
	rows, err := s.query(ctx, q+` ORDER BY project_id, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Customer{}
	for rows.Next() {
		var c Customer
		var ca, ua int64
		if err := rows.Scan(&c.ProjectID, &c.ID, &c.Name, &c.StripeCustomerID, &ca, &ua); err != nil {
			return nil, err
		}
		c.CreatedAt, c.UpdatedAt = fromMS(ca), fromMS(ua)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// MarkupRule adds a percentage on top of provider cost when billing end
// customers. Empty ProjectID/CustomerID/Model match anything; the most
// specific matching rule wins.
type MarkupRule struct {
	ID         string  `json:"id"`
	OrgID      string  `json:"org_id"`
	ProjectID  string  `json:"project_id"`
	CustomerID string  `json:"customer_id"`
	Model      string  `json:"model"` // pattern, e.g. "openai/*"
	MarkupPct  float64 `json:"markup_pct"`
	// PerMTokUSD adds a flat fee per million tokens (prompt + completion)
	// on top of the percentage markup.
	PerMTokUSD float64   `json:"per_mtok_usd"`
	CreatedAt  time.Time `json:"created_at"`
}

// AddMarkupRule inserts a markup rule.
func (s *Store) AddMarkupRule(ctx context.Context, m *MarkupRule) error {
	if m.MarkupPct < -100 || m.MarkupPct > 100000 {
		return errors.New("markup_pct must be between -100 and 100000")
	}
	if m.PerMTokUSD < 0 || m.PerMTokUSD > 10000 {
		return errors.New("per_mtok_usd must be between 0 and 10000")
	}
	if m.OrgID == "" {
		m.OrgID = DefaultOrgID
	}
	m.ID, m.CreatedAt = secure.RandomID("mk_"), time.Now().UTC()
	_, err := s.exec(ctx, `INSERT INTO markup_rules(id,org_id,project_id,customer_id,model,markup_pct,per_mtok_usd,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		m.ID, m.OrgID, m.ProjectID, strings.TrimSpace(m.CustomerID), strings.TrimSpace(m.Model), m.MarkupPct, m.PerMTokUSD, ms(m.CreatedAt))
	return err
}

// ListMarkupRules lists an org's markup rules ("" = all).
func (s *Store) ListMarkupRules(ctx context.Context, orgID string) ([]*MarkupRule, error) {
	q, args := `SELECT id,org_id,project_id,customer_id,model,markup_pct,per_mtok_usd,created_at FROM markup_rules`, []any{}
	if orgID != "" {
		q, args = q+` WHERE org_id=?`, append(args, orgID)
	}
	rows, err := s.query(ctx, q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MarkupRule{}
	for rows.Next() {
		var m MarkupRule
		var c int64
		if err := rows.Scan(&m.ID, &m.OrgID, &m.ProjectID, &m.CustomerID, &m.Model, &m.MarkupPct, &m.PerMTokUSD, &c); err != nil {
			return nil, err
		}
		m.CreatedAt = fromMS(c)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// DeleteMarkupRule removes a markup rule.
func (s *Store) DeleteMarkupRule(ctx context.Context, orgID, id string) error {
	return s.execOne(ctx, `DELETE FROM markup_rules WHERE id=? AND org_id=?`, id, orgID)
}

// CustomerUsage aggregates one customer's usage.
type CustomerUsage struct {
	ProjectID        string  `json:"project_id"`
	CustomerID       string  `json:"customer_id"`
	Requests         int     `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	BilledUSD        float64 `json:"billed_usd"`
	LastSeen         int64   `json:"last_seen_ms"`
}

// CustomerUsageBetween aggregates successful requests with a customer ID in [from, to).
func (s *Store) CustomerUsageBetween(ctx context.Context, f StatsFilter, from, to time.Time) ([]*CustomerUsage, error) {
	w, wa := f.where()
	rows, err := s.query(ctx, `SELECT project_id, customer_id, COUNT(*), CAST(COALESCE(SUM(prompt_tokens),0) AS BIGINT), CAST(COALESCE(SUM(completion_tokens),0) AS BIGINT),
COALESCE(SUM(cost_usd),0), COALESCE(SUM(billed_usd),0), MAX(ts)
FROM request_logs WHERE customer_id<>'' AND status<400 AND ts>=? AND ts<?`+w+` GROUP BY project_id, customer_id ORDER BY 7 DESC`,
		append([]any{ms(from), ms(to)}, wa...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CustomerUsage{}
	for rows.Next() {
		var u CustomerUsage
		if err := rows.Scan(&u.ProjectID, &u.CustomerID, &u.Requests, &u.PromptTokens, &u.CompletionTokens, &u.CostUSD, &u.BilledUSD, &u.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

// ---------- Stripe sync state ----------

// StripeSyncState tracks how far one customer's usage was reported to Stripe.
type StripeSyncState struct {
	ProjectID        string    `json:"project_id"`
	CustomerID       string    `json:"customer_id"`
	StripeCustomerID string    `json:"stripe_customer_id"`
	SyncedTo         time.Time `json:"synced_to"`
	Remainder        float64   `json:"remainder"` // fractional units carried to the next event
	LastValue        int64     `json:"last_value"`
	LastError        string    `json:"last_error,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// StripeSyncStates lists customers with a Stripe mapping and their cursor.
func (s *Store) StripeSyncStates(ctx context.Context) ([]*StripeSyncState, error) {
	rows, err := s.query(ctx, `SELECT st.project_id, st.customer_id, c.stripe_customer_id, st.synced_to, st.remainder, st.last_value, st.last_error, st.updated_at
FROM stripe_sync st JOIN customers c ON c.project_id=st.project_id AND c.id=st.customer_id WHERE c.stripe_customer_id<>'' ORDER BY st.project_id, st.customer_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*StripeSyncState{}
	for rows.Next() {
		var st StripeSyncState
		var to, up int64
		if err := rows.Scan(&st.ProjectID, &st.CustomerID, &st.StripeCustomerID, &to, &st.Remainder, &st.LastValue, &st.LastError, &up); err != nil {
			return nil, err
		}
		st.SyncedTo, st.UpdatedAt = fromMS(to), fromMS(up)
		out = append(out, &st)
	}
	return out, rows.Err()
}

// UsageWindow sums billed USD and tokens of one customer in (from, to].
func (s *Store) UsageWindow(ctx context.Context, projectID, customerID string, from, to time.Time) (billed float64, tokens int64, err error) {
	var b sql.NullFloat64
	var t sql.NullInt64
	err = s.queryRow(ctx, `SELECT SUM(billed_usd), CAST(SUM(prompt_tokens+completion_tokens) AS BIGINT) FROM request_logs
WHERE project_id=? AND customer_id=? AND status<400 AND ts>? AND ts<=?`, projectID, customerID, ms(from), ms(to)).Scan(&b, &t)
	return b.Float64, t.Int64, err
}

// AdvanceStripeSync moves a customer's cursor from -> to after a successful
// report. It is a compare-and-swap: ErrNotFound means the cursor is no
// longer at from (another writer advanced it), and nothing changed.
func (s *Store) AdvanceStripeSync(ctx context.Context, projectID, customerID string, from, to time.Time, remainder float64, value int64) error {
	return s.execOne(ctx, `UPDATE stripe_sync SET synced_to=?, remainder=?, last_value=?, last_error='', updated_at=? WHERE project_id=? AND customer_id=? AND synced_to=?`,
		ms(to), remainder, value, ms(time.Now()), projectID, customerID, ms(from))
}

// FailStripeSync records a sync error without moving the cursor.
func (s *Store) FailStripeSync(ctx context.Context, projectID, customerID, msg string) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return s.execOne(ctx, `UPDATE stripe_sync SET last_error=?, updated_at=? WHERE project_id=? AND customer_id=?`, msg, ms(time.Now()), projectID, customerID)
}

// ---------- response cache ----------

// CacheEntry is one cached upstream response.
type CacheEntry struct {
	Key              string
	ProjectID        string
	Endpoint         string
	Provider         string
	ProviderModel    string
	Body             []byte
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	ExpiresAt        time.Time
}

// CacheGet returns a live entry and counts the hit.
func (s *Store) CacheGet(ctx context.Context, key string, now time.Time) (*CacheEntry, error) {
	var e CacheEntry
	var body string
	var exp int64
	err := s.queryRow(ctx, `SELECT cache_key,project_id,endpoint,provider,provider_model,body,prompt_tokens,completion_tokens,cost_usd,expires_at
FROM response_cache WHERE cache_key=? AND expires_at>?`, key, ms(now)).
		Scan(&e.Key, &e.ProjectID, &e.Endpoint, &e.Provider, &e.ProviderModel, &body, &e.PromptTokens, &e.CompletionTokens, &e.CostUSD, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.Body, e.ExpiresAt = []byte(body), fromMS(exp)
	_, _ = s.exec(ctx, `UPDATE response_cache SET hits=hits+1 WHERE cache_key=?`, key)
	return &e, nil
}

// CachePut stores (or refreshes) an entry.
func (s *Store) CachePut(ctx context.Context, e *CacheEntry) error {
	now := time.Now()
	_, err := s.exec(ctx, `INSERT INTO response_cache(cache_key,project_id,endpoint,provider,provider_model,body,prompt_tokens,completion_tokens,cost_usd,hits,created_at,expires_at)
VALUES(?,?,?,?,?,?,?,?,?,0,?,?) ON CONFLICT(cache_key) DO UPDATE SET body=excluded.body, provider=excluded.provider, provider_model=excluded.provider_model,
prompt_tokens=excluded.prompt_tokens, completion_tokens=excluded.completion_tokens, cost_usd=excluded.cost_usd, created_at=excluded.created_at, expires_at=excluded.expires_at`,
		e.Key, e.ProjectID, e.Endpoint, e.Provider, e.ProviderModel, string(e.Body), e.PromptTokens, e.CompletionTokens, e.CostUSD, ms(now), ms(e.ExpiresAt))
	return err
}

// CacheStats summarizes the cache for a project ("" = all).
type CacheStats struct {
	Entries int   `json:"entries"`
	Hits    int64 `json:"hits"`
	Bytes   int64 `json:"bytes"`
}

// CacheStatsFor returns live-entry statistics for the given projects
// (nil = all projects; empty non-nil = none).
func (s *Store) CacheStatsFor(ctx context.Context, projectIDs []string) (CacheStats, error) {
	var st CacheStats
	q, args := `SELECT COUNT(*), CAST(COALESCE(SUM(hits),0) AS BIGINT), CAST(COALESCE(SUM(LENGTH(body)),0) AS BIGINT) FROM response_cache WHERE expires_at>?`, []any{ms(time.Now())}
	if projectIDs != nil {
		if len(projectIDs) == 0 {
			return st, nil
		}
		q += ` AND project_id IN (?` + strings.Repeat(",?", len(projectIDs)-1) + `)`
		for _, id := range projectIDs {
			args = append(args, id)
		}
	}
	err := s.queryRow(ctx, q, args...).Scan(&st.Entries, &st.Hits, &st.Bytes)
	return st, err
}

// PurgeCache deletes expired entries, or every entry of a project when
// projectID is set.
func (s *Store) PurgeCache(ctx context.Context, projectID string) (int64, error) {
	var res sql.Result
	var err error
	if projectID != "" {
		res, err = s.exec(ctx, `DELETE FROM response_cache WHERE project_id=?`, projectID)
	} else {
		res, err = s.exec(ctx, `DELETE FROM response_cache WHERE expires_at<=?`, ms(time.Now()))
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
