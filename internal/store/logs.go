package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// RequestLog is one gateway request.
type RequestLog struct {
	ID               string    `json:"id"`
	TS               time.Time `json:"ts"`
	ProjectID        string    `json:"project_id"`
	KeyID            string    `json:"key_id"`
	Agent            string    `json:"agent"`
	UserLabel        string    `json:"user"`
	Endpoint         string    `json:"endpoint"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	ProviderModel    string    `json:"provider_model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	CacheWriteTokens int       `json:"cache_write_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	LatencyMS        int64     `json:"latency_ms"`
	Status           int       `json:"status"`
	ErrorCode        string    `json:"error_code,omitempty"`
	ErrorMessage     string    `json:"error_message,omitempty"`
	Stream           bool      `json:"stream"`
	Attempts         int       `json:"attempts"`
	UsageEstimated   bool      `json:"usage_estimated"`
	CustomerID       string    `json:"customer_id,omitempty"`
	Route            string    `json:"route,omitempty"`
	BaselineCostUSD  float64   `json:"baseline_cost_usd,omitempty"`
	BilledUSD        float64   `json:"billed_usd,omitempty"`
	CacheHit         bool      `json:"cache_hit"`
	Guard            string    `json:"guard,omitempty"`
	GuardAction      string    `json:"guard_action,omitempty"`
	RequestBody      string    `json:"request_body,omitempty"`
	ResponseBody     string    `json:"response_body,omitempty"`
}

// InsertLog stores a request log.
func (s *Store) InsertLog(ctx context.Context, l *RequestLog) error {
	var reqBody, respBody any
	if l.RequestBody != "" {
		reqBody = l.RequestBody
	}
	if l.ResponseBody != "" {
		respBody = l.ResponseBody
	}
	if l.Endpoint == "" {
		l.Endpoint = "chat"
	}
	_, err := s.exec(ctx, `INSERT INTO request_logs(`+logCols+`,request_body,response_body)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.ID, ms(l.TS), l.ProjectID, l.KeyID, l.Agent, l.UserLabel, l.Model, l.Provider, l.ProviderModel,
		l.PromptTokens, l.CompletionTokens, l.CostUSD, l.LatencyMS, l.Status, l.ErrorCode, l.ErrorMessage,
		b2i(l.Stream), l.Attempts, b2i(l.UsageEstimated),
		l.Endpoint, l.CachedTokens, l.CacheWriteTokens, l.CustomerID, l.Route, l.BaselineCostUSD, l.BilledUSD, b2i(l.CacheHit), l.Guard, l.GuardAction,
		reqBody, respBody)
	return err
}

// LogFilter narrows ListLogs.
type LogFilter struct {
	OrgID      string
	ProjectID  string
	KeyID      string
	Agent      string
	Model      string
	CustomerID string
	ErrorsOnly bool
	GuardOnly  bool
	Limit      int
	Offset     int
}

const logCols = `id,ts,project_id,key_id,agent,user_label,model,provider,provider_model,prompt_tokens,completion_tokens,cost_usd,latency_ms,status,error_code,error_message,stream,attempts,usage_estimated,endpoint,cached_tokens,cache_write_tokens,customer_id,route,baseline_cost_usd,billed_usd,cache_hit,guard,guard_action`

func scanLog(sc interface{ Scan(...any) error }, withBodies bool) (*RequestLog, error) {
	var l RequestLog
	var ts int64
	var stream, est, hit int
	dest := []any{&l.ID, &ts, &l.ProjectID, &l.KeyID, &l.Agent, &l.UserLabel, &l.Model, &l.Provider, &l.ProviderModel,
		&l.PromptTokens, &l.CompletionTokens, &l.CostUSD, &l.LatencyMS, &l.Status, &l.ErrorCode, &l.ErrorMessage, &stream, &l.Attempts, &est,
		&l.Endpoint, &l.CachedTokens, &l.CacheWriteTokens, &l.CustomerID, &l.Route, &l.BaselineCostUSD, &l.BilledUSD, &hit, &l.Guard, &l.GuardAction}
	var rb, sb sql.NullString
	if withBodies {
		dest = append(dest, &rb, &sb)
	}
	if err := sc.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	l.TS, l.Stream, l.UsageEstimated, l.CacheHit = fromMS(ts), stream == 1, est == 1, hit == 1
	l.RequestBody, l.ResponseBody = rb.String, sb.String
	return &l, nil
}

// StatsFilter scopes aggregate queries to an organization and/or project.
type StatsFilter struct {
	OrgID     string
	ProjectID string
}

func (f StatsFilter) where() (string, []any) {
	var w []string
	var args []any
	if f.OrgID != "" {
		w, args = append(w, "project_id IN (SELECT id FROM projects WHERE org_id=?)"), append(args, f.OrgID)
	}
	if f.ProjectID != "" {
		w, args = append(w, "project_id=?"), append(args, f.ProjectID)
	}
	if len(w) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(w, " AND "), args
}

// ListLogs returns logs newest first (without bodies) and the total count.
func (s *Store) ListLogs(ctx context.Context, f LogFilter) ([]*RequestLog, int, error) {
	var where []string
	var args []any
	if f.OrgID != "" {
		where, args = append(where, "project_id IN (SELECT id FROM projects WHERE org_id=?)"), append(args, f.OrgID)
	}
	if f.CustomerID != "" {
		where, args = append(where, "customer_id=?"), append(args, f.CustomerID)
	}
	if f.GuardOnly {
		where = append(where, "guard<>''")
	}
	if f.ProjectID != "" {
		where, args = append(where, "project_id=?"), append(args, f.ProjectID)
	}
	if f.KeyID != "" {
		where, args = append(where, "key_id=?"), append(args, f.KeyID)
	}
	if f.Agent != "" {
		where, args = append(where, "agent=?"), append(args, f.Agent)
	}
	if f.Model != "" {
		where, args = append(where, "model=?"), append(args, f.Model)
	}
	if f.ErrorsOnly {
		where = append(where, "status>=400")
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM request_logs`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := s.query(ctx, `SELECT `+logCols+` FROM request_logs`+w+` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*RequestLog{}
	for rows.Next() {
		l, err := scanLog(rows, false)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// GetLog returns one log including bodies.
func (s *Store) GetLog(ctx context.Context, id string) (*RequestLog, error) {
	return scanLog(s.queryRow(ctx, `SELECT `+logCols+`,request_body,response_body FROM request_logs WHERE id=?`, id), true)
}

// SpendSince sums cost for a scope since t.
func (s *Store) SpendSince(ctx context.Context, scope, scopeID string, since time.Time) (float64, error) {
	var q string
	var args []any
	switch scope {
	case ScopeKey:
		q, args = `key_id=?`, []any{scopeID}
	case ScopeProject:
		q, args = `project_id=?`, []any{scopeID}
	case ScopeAgent:
		proj, agent, _ := strings.Cut(scopeID, "/")
		q, args = `project_id=? AND agent=?`, []any{proj, agent}
	default:
		return 0, errors.New("unknown scope")
	}
	var v sql.NullFloat64
	err := s.queryRow(ctx, `SELECT SUM(cost_usd) FROM request_logs WHERE `+q+` AND ts>=?`, append(args, ms(since))...).Scan(&v)
	return v.Float64, err
}

// SeriesPoint is one bucket of the spend time series.
type SeriesPoint struct {
	Bucket   time.Time `json:"bucket"`
	Provider string    `json:"provider"`
	CostUSD  float64   `json:"cost_usd"`
	Requests int       `json:"requests"`
	Tokens   int       `json:"tokens"`
}

// SpendSeries buckets spend by day (or hour) and provider since t.
func (s *Store) SpendSeries(ctx context.Context, f StatsFilter, since time.Time, bucket time.Duration) ([]SeriesPoint, error) {
	b := bucket.Milliseconds()
	w, wa := f.where()
	rows, err := s.query(ctx, `SELECT (ts/?)*? AS b, provider, SUM(cost_usd), COUNT(*), CAST(SUM(prompt_tokens+completion_tokens) AS BIGINT)
FROM request_logs WHERE ts>=?`+w+` GROUP BY b, provider ORDER BY b`, append([]any{b, b, ms(since)}, wa...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeriesPoint{}
	for rows.Next() {
		var p SeriesPoint
		var bk int64
		if err := rows.Scan(&bk, &p.Provider, &p.CostUSD, &p.Requests, &p.Tokens); err != nil {
			return nil, err
		}
		p.Bucket = fromMS(bk)
		if p.Provider == "" {
			p.Provider = "(rejected)"
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Summary aggregates totals since t.
type Summary struct {
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	Blocked      int     `json:"blocked"`
	CostUSD      float64 `json:"cost_usd"`
	Tokens       int     `json:"tokens"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	SavedUSD     float64 `json:"saved_usd"`  // routing savings vs. requested/baseline model
	CacheHits    int     `json:"cache_hits"` // responses served from the cache
	CacheSaved   float64 `json:"cache_saved_usd"`
	Flagged      int     `json:"guard_flagged"` // requests with guardrail findings
	BilledUSD    float64 `json:"billed_usd"`
}

// SummarySince returns aggregate stats since t.
func (s *Store) SummarySince(ctx context.Context, f StatsFilter, since time.Time) (Summary, error) {
	var sm Summary
	var lat, cost, saved, cacheSaved, billed sql.NullFloat64
	var tok, errs, blocked, hits, flagged sql.NullInt64
	w, wa := f.where()
	err := s.queryRow(ctx, `SELECT COUNT(*),
CAST(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END) AS BIGINT),
CAST(SUM(CASE WHEN error_code IN ('budget_exceeded','key_killed','project_killed','killed','killed_mid_stream','rate_limit_exceeded','model_not_allowed','max_tokens_exceeded','guardrail_blocked','prompt_injection') THEN 1 ELSE 0 END) AS BIGINT),
COALESCE(SUM(cost_usd),0), CAST(SUM(prompt_tokens+completion_tokens) AS BIGINT), AVG(CASE WHEN status<400 THEN latency_ms END),
SUM(CASE WHEN cache_hit=0 AND baseline_cost_usd>cost_usd THEN baseline_cost_usd-cost_usd ELSE 0 END),
CAST(SUM(cache_hit) AS BIGINT), SUM(CASE WHEN cache_hit=1 THEN baseline_cost_usd ELSE 0 END),
CAST(SUM(CASE WHEN guard<>'' THEN 1 ELSE 0 END) AS BIGINT), SUM(billed_usd)
FROM request_logs WHERE ts>=?`+w, append([]any{ms(since)}, wa...)...).
		Scan(&sm.Requests, &errs, &blocked, &cost, &tok, &lat, &saved, &hits, &cacheSaved, &flagged, &billed)
	sm.Errors, sm.Blocked, sm.CostUSD, sm.Tokens, sm.AvgLatencyMS = int(errs.Int64), int(blocked.Int64), cost.Float64, int(tok.Int64), lat.Float64
	sm.SavedUSD, sm.CacheHits, sm.CacheSaved, sm.Flagged, sm.BilledUSD = saved.Float64, int(hits.Int64), cacheSaved.Float64, int(flagged.Int64), billed.Float64
	return sm, err
}

// Breakdown row for top-N tables.
type Breakdown struct {
	Name     string  `json:"name"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int     `json:"requests"`
	SavedUSD float64 `json:"saved_usd"`
}

// BreakdownSince groups spend by "agent", "model", "project", "key",
// "customer" or "route" since t.
func (s *Store) BreakdownSince(ctx context.Context, f StatsFilter, by string, since time.Time, limit int) ([]Breakdown, error) {
	col := map[string]string{
		"agent":    "CASE WHEN agent='' THEN '(none)' ELSE agent END",
		"model":    "model",
		"project":  "project_id",
		"key":      "key_id",
		"customer": "customer_id",
		"route":    "route",
	}[by]
	if col == "" {
		return nil, errors.New("bad group")
	}
	w, wa := f.where()
	extra := ""
	if by == "customer" || by == "route" {
		extra = " AND " + col + "<>''"
	}
	rows, err := s.query(ctx, `SELECT `+col+` AS g, SUM(cost_usd) AS c, COUNT(*), SUM(CASE WHEN baseline_cost_usd>cost_usd THEN baseline_cost_usd-cost_usd ELSE 0 END) FROM request_logs WHERE ts>=?`+w+extra+` GROUP BY g ORDER BY c DESC LIMIT ?`,
		append(append([]any{ms(since)}, wa...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Breakdown{}
	for rows.Next() {
		var b Breakdown
		if err := rows.Scan(&b.Name, &b.CostUSD, &b.Requests, &b.SavedUSD); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkAlertSent records an alert; it returns false if it was already sent.
func (s *Store) MarkAlertSent(ctx context.Context, scope, scopeID, period string, periodStart time.Time, threshold float64) (bool, error) {
	res, err := s.exec(ctx, `INSERT INTO alerts_sent(scope,scope_id,period,period_start,threshold,sent_at) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		scope, scopeID, period, ms(periodStart), threshold, ms(time.Now()))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
