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
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	ProviderModel    string    `json:"provider_model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	LatencyMS        int64     `json:"latency_ms"`
	Status           int       `json:"status"`
	ErrorCode        string    `json:"error_code,omitempty"`
	ErrorMessage     string    `json:"error_message,omitempty"`
	Stream           bool      `json:"stream"`
	Attempts         int       `json:"attempts"`
	UsageEstimated   bool      `json:"usage_estimated"`
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO request_logs(id,ts,project_id,key_id,agent,user_label,model,provider,provider_model,
prompt_tokens,completion_tokens,cost_usd,latency_ms,status,error_code,error_message,stream,attempts,usage_estimated,request_body,response_body)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.ID, ms(l.TS), l.ProjectID, l.KeyID, l.Agent, l.UserLabel, l.Model, l.Provider, l.ProviderModel,
		l.PromptTokens, l.CompletionTokens, l.CostUSD, l.LatencyMS, l.Status, l.ErrorCode, l.ErrorMessage,
		b2i(l.Stream), l.Attempts, b2i(l.UsageEstimated), reqBody, respBody)
	return err
}

// LogFilter narrows ListLogs.
type LogFilter struct {
	ProjectID  string
	KeyID      string
	Agent      string
	Model      string
	ErrorsOnly bool
	Limit      int
	Offset     int
}

const logCols = `id,ts,project_id,key_id,agent,user_label,model,provider,provider_model,prompt_tokens,completion_tokens,cost_usd,latency_ms,status,error_code,error_message,stream,attempts,usage_estimated`

func scanLog(sc interface{ Scan(...any) error }, withBodies bool) (*RequestLog, error) {
	var l RequestLog
	var ts int64
	var stream, est int
	dest := []any{&l.ID, &ts, &l.ProjectID, &l.KeyID, &l.Agent, &l.UserLabel, &l.Model, &l.Provider, &l.ProviderModel,
		&l.PromptTokens, &l.CompletionTokens, &l.CostUSD, &l.LatencyMS, &l.Status, &l.ErrorCode, &l.ErrorMessage, &stream, &l.Attempts, &est}
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
	l.TS, l.Stream, l.UsageEstimated = fromMS(ts), stream == 1, est == 1
	l.RequestBody, l.ResponseBody = rb.String, sb.String
	return &l, nil
}

// ListLogs returns logs newest first (without bodies) and the total count.
func (s *Store) ListLogs(ctx context.Context, f LogFilter) ([]*RequestLog, int, error) {
	var where []string
	var args []any
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
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+logCols+` FROM request_logs`+w+` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`,
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
	return scanLog(s.db.QueryRowContext(ctx, `SELECT `+logCols+`,request_body,response_body FROM request_logs WHERE id=?`, id), true)
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
	err := s.db.QueryRowContext(ctx, `SELECT SUM(cost_usd) FROM request_logs WHERE `+q+` AND ts>=?`, append(args, ms(since))...).Scan(&v)
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
func (s *Store) SpendSeries(ctx context.Context, since time.Time, bucket time.Duration) ([]SeriesPoint, error) {
	b := bucket.Milliseconds()
	rows, err := s.db.QueryContext(ctx, `SELECT (ts/?)*? AS b, provider, SUM(cost_usd), COUNT(*), SUM(prompt_tokens+completion_tokens)
FROM request_logs WHERE ts>=? GROUP BY b, provider ORDER BY b`, b, b, ms(since))
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
}

// SummarySince returns aggregate stats since t.
func (s *Store) SummarySince(ctx context.Context, since time.Time) (Summary, error) {
	var sm Summary
	var lat sql.NullFloat64
	var cost sql.NullFloat64
	var tok, errs, blocked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(status>=400), SUM(error_code IN ('budget_exceeded','key_killed','project_killed','rate_limit_exceeded')),
COALESCE(SUM(cost_usd),0), SUM(prompt_tokens+completion_tokens), AVG(CASE WHEN status<400 THEN latency_ms END) FROM request_logs WHERE ts>=?`, ms(since)).
		Scan(&sm.Requests, &errs, &blocked, &cost, &tok, &lat)
	sm.Errors, sm.Blocked, sm.CostUSD, sm.Tokens, sm.AvgLatencyMS = int(errs.Int64), int(blocked.Int64), cost.Float64, int(tok.Int64), lat.Float64
	return sm, err
}

// Breakdown row for top-N tables.
type Breakdown struct {
	Name     string  `json:"name"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int     `json:"requests"`
}

// BreakdownSince groups spend by "agent", "model" or "project" since t.
func (s *Store) BreakdownSince(ctx context.Context, by string, since time.Time, limit int) ([]Breakdown, error) {
	col := map[string]string{
		"agent":   "CASE WHEN agent='' THEN '(none)' ELSE agent END",
		"model":   "model",
		"project": "project_id",
		"key":     "key_id",
	}[by]
	if col == "" {
		return nil, errors.New("bad group")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+col+` AS g, SUM(cost_usd) c, COUNT(*) FROM request_logs WHERE ts>=? GROUP BY g ORDER BY c DESC LIMIT ?`, ms(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Breakdown{}
	for rows.Next() {
		var b Breakdown
		if err := rows.Scan(&b.Name, &b.CostUSD, &b.Requests); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkAlertSent records an alert; it returns false if it was already sent.
func (s *Store) MarkAlertSent(ctx context.Context, scope, scopeID, period string, periodStart time.Time, threshold float64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO alerts_sent(scope,scope_id,period,period_start,threshold,sent_at) VALUES(?,?,?,?,?,?)`,
		scope, scopeID, period, ms(periodStart), threshold, ms(time.Now()))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
