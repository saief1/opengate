package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/secure"
)

// MCPServer is an upstream MCP (Model Context Protocol) server reachable
// through the gateway at /mcp/{name}. The upstream secret is stored
// encrypted and never returned by the API (only SecretHint).
type MCPServer struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"org_id"`
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	Transport  string    `json:"transport"` // "http" (streamable HTTP) or "sse"
	AuthHeader string    `json:"auth_header"`
	SecretCT   string    `json:"-"`
	SecretHint string    `json:"secret_hint"`
	HasSecret  bool      `json:"has_secret"`
	DefaultRPM int       `json:"default_rpm"` // per agent+tool, 0 = unlimited
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// MCPCredential is a per-agent upstream secret for one server; it overrides
// the server-wide secret for that agent.
type MCPCredential struct {
	ID         string    `json:"id"`
	ServerID   string    `json:"server_id"`
	Agent      string    `json:"agent"`
	Header     string    `json:"header"`
	SecretCT   string    `json:"-"`
	SecretHint string    `json:"secret_hint"`
	CreatedAt  time.Time `json:"created_at"`
}

// MCPPolicy is one allow/deny rule for tools on a server. Agent "" applies
// to every agent; Tool may end in "*". RPM > 0 rate-limits matching calls
// per agent and tool.
type MCPPolicy struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	ServerID  string    `json:"server_id"`
	Agent     string    `json:"agent"`
	Tool      string    `json:"tool"`
	Action    string    `json:"action"` // allow | deny
	RPM       int       `json:"rpm"`
	CreatedAt time.Time `json:"created_at"`
}

// MCPCall is one logged MCP request (tool call or other JSON-RPC method).
type MCPCall struct {
	ID           string    `json:"id"`
	OrgID        string    `json:"org_id"`
	TS           time.Time `json:"ts"`
	ProjectID    string    `json:"project_id"`
	KeyID        string    `json:"key_id"`
	Agent        string    `json:"agent"`
	ServerID     string    `json:"server_id"`
	ServerName   string    `json:"server_name"`
	Method       string    `json:"method"`
	Tool         string    `json:"tool"`
	Args         string    `json:"args"` // redacted, truncated JSON
	Redacted     bool      `json:"redacted"`
	Status       int       `json:"status"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	LatencyMS    int64     `json:"latency_ms"`
}

var mcpNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidateMCPServer checks name, URL and transport.
func ValidateMCPServer(m *MCPServer, allowInsecure bool) error {
	m.Name = strings.TrimSpace(strings.ToLower(m.Name))
	if !mcpNameRe.MatchString(m.Name) {
		return errors.New("name must be 1-63 chars of a-z, 0-9, '-' or '_'")
	}
	u, err := url.Parse(strings.TrimSpace(m.URL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("url must be an absolute http(s) URL")
	}
	if u.User != nil {
		return errors.New("url must not embed credentials; set a secret instead")
	}
	if u.Scheme == "http" && !allowInsecure {
		h := u.Hostname()
		if h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return errors.New("plain http upstreams are only allowed for loopback hosts (set allow_insecure_upstream to override)")
		}
	}
	m.URL = u.String()
	if m.Transport == "" {
		m.Transport = "http"
	}
	if m.Transport != "http" && m.Transport != "sse" {
		return errors.New(`transport must be "http" (streamable HTTP) or "sse"`)
	}
	if m.AuthHeader == "" {
		m.AuthHeader = "Authorization"
	}
	if !validHeaderName(m.AuthHeader) {
		return errors.New("auth_header is not a valid header name")
	}
	if m.DefaultRPM < 0 {
		return errors.New("default_rpm must be >= 0")
	}
	return nil
}

func validHeaderName(h string) bool {
	if h == "" || len(h) > 64 {
		return false
	}
	for _, c := range h {
		if !(c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	switch strings.ToLower(h) {
	case "host", "content-length", "transfer-encoding", "connection", "cookie", "mcp-session-id":
		return false
	}
	return true
}

// MCPSecretAAD binds a ciphertext to its owner row.
func MCPSecretAAD(id string) []byte { return []byte("mcp-secret:" + id) }

const mcpServerCols = `id,org_id,name,url,transport,auth_header,secret_ct,secret_hint,default_rpm,enabled,created_at,updated_at`

func scanMCPServer(sc interface{ Scan(...any) error }) (*MCPServer, error) {
	var m MCPServer
	var en int
	var c, u int64
	if err := sc.Scan(&m.ID, &m.OrgID, &m.Name, &m.URL, &m.Transport, &m.AuthHeader, &m.SecretCT, &m.SecretHint, &m.DefaultRPM, &en, &c, &u); err != nil {
		return nil, err
	}
	m.Enabled, m.CreatedAt, m.UpdatedAt, m.HasSecret = en != 0, fromMS(c), fromMS(u), m.SecretCT != ""
	return &m, nil
}

// NewMCPServerID returns a fresh server ID (so callers can encrypt the
// secret with the ID as AAD before inserting).
func NewMCPServerID() string { return secure.RandomID("mcps_") }

// CreateMCPServer inserts a server (ID must be set).
func (s *Store) CreateMCPServer(ctx context.Context, m *MCPServer) error {
	if m.OrgID == "" {
		m.OrgID = DefaultOrgID
	}
	now := time.Now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	_, err := s.exec(ctx, `INSERT INTO mcp_servers(`+mcpServerCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.OrgID, m.Name, m.URL, m.Transport, m.AuthHeader, m.SecretCT, m.SecretHint, m.DefaultRPM, b2i(m.Enabled), ms(now), ms(now))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return fmt.Errorf("an MCP server named %q already exists", m.Name)
	}
	m.HasSecret = m.SecretCT != ""
	return err
}

// UpdateMCPServer rewrites a server's mutable fields.
func (s *Store) UpdateMCPServer(ctx context.Context, m *MCPServer) error {
	m.UpdatedAt = time.Now().UTC()
	return s.execOne(ctx, `UPDATE mcp_servers SET url=?, transport=?, auth_header=?, secret_ct=?, secret_hint=?, default_rpm=?, enabled=?, updated_at=? WHERE id=? AND org_id=?`,
		m.URL, m.Transport, m.AuthHeader, m.SecretCT, m.SecretHint, m.DefaultRPM, b2i(m.Enabled), ms(m.UpdatedAt), m.ID, m.OrgID)
}

// GetMCPServer loads a server by ID within an org.
func (s *Store) GetMCPServer(ctx context.Context, orgID, id string) (*MCPServer, error) {
	m, err := scanMCPServer(s.queryRow(ctx, `SELECT `+mcpServerCols+` FROM mcp_servers WHERE id=? AND org_id=?`, id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// MCPServerByName loads a server by its name within an org.
func (s *Store) MCPServerByName(ctx context.Context, orgID, name string) (*MCPServer, error) {
	m, err := scanMCPServer(s.queryRow(ctx, `SELECT `+mcpServerCols+` FROM mcp_servers WHERE org_id=? AND name=?`, orgID, strings.ToLower(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// ListMCPServers lists an org's servers by name.
func (s *Store) ListMCPServers(ctx context.Context, orgID string) ([]*MCPServer, error) {
	rows, err := s.query(ctx, `SELECT `+mcpServerCols+` FROM mcp_servers WHERE org_id=? ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MCPServer{}
	for rows.Next() {
		m, err := scanMCPServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMCPServer removes a server with its policies and credentials (the
// call log is kept).
func (s *Store) DeleteMCPServer(ctx context.Context, orgID, id string) error {
	if err := s.execOne(ctx, `DELETE FROM mcp_servers WHERE id=? AND org_id=?`, id, orgID); err != nil {
		return err
	}
	_, _ = s.exec(ctx, `DELETE FROM mcp_policies WHERE server_id=?`, id)
	_, _ = s.exec(ctx, `DELETE FROM mcp_credentials WHERE server_id=?`, id)
	return nil
}

// ---------- credentials ----------

// NewMCPCredentialID returns a fresh credential ID.
func NewMCPCredentialID() string { return secure.RandomID("mcpc_") }

// PutMCPCredential creates or replaces an agent's credential for a server.
func (s *Store) PutMCPCredential(ctx context.Context, c *MCPCredential) error {
	if c.Header == "" {
		c.Header = "Authorization"
	}
	if !validHeaderName(c.Header) {
		return errors.New("header is not a valid header name")
	}
	if strings.TrimSpace(c.Agent) == "" {
		return errors.New("agent is required")
	}
	c.CreatedAt = time.Now().UTC()
	_, _ = s.exec(ctx, `DELETE FROM mcp_credentials WHERE server_id=? AND agent=?`, c.ServerID, c.Agent)
	_, err := s.exec(ctx, `INSERT INTO mcp_credentials(id,server_id,agent,header,secret_ct,secret_hint,created_at) VALUES(?,?,?,?,?,?,?)`,
		c.ID, c.ServerID, c.Agent, c.Header, c.SecretCT, c.SecretHint, ms(c.CreatedAt))
	return err
}

// ListMCPCredentials lists a server's per-agent credentials (no secrets).
func (s *Store) ListMCPCredentials(ctx context.Context, serverID string) ([]*MCPCredential, error) {
	rows, err := s.query(ctx, `SELECT id,server_id,agent,header,secret_ct,secret_hint,created_at FROM mcp_credentials WHERE server_id=? ORDER BY agent`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MCPCredential{}
	for rows.Next() {
		var c MCPCredential
		var t int64
		if err := rows.Scan(&c.ID, &c.ServerID, &c.Agent, &c.Header, &c.SecretCT, &c.SecretHint, &t); err != nil {
			return nil, err
		}
		c.CreatedAt = fromMS(t)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// MCPCredentialFor returns the agent's credential for a server (ErrNotFound if none).
func (s *Store) MCPCredentialFor(ctx context.Context, serverID, agent string) (*MCPCredential, error) {
	var c MCPCredential
	var t int64
	err := s.queryRow(ctx, `SELECT id,server_id,agent,header,secret_ct,secret_hint,created_at FROM mcp_credentials WHERE server_id=? AND agent=?`, serverID, agent).
		Scan(&c.ID, &c.ServerID, &c.Agent, &c.Header, &c.SecretCT, &c.SecretHint, &t)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	c.CreatedAt = fromMS(t)
	return &c, err
}

// DeleteMCPCredential removes one credential.
func (s *Store) DeleteMCPCredential(ctx context.Context, serverID, id string) error {
	return s.execOne(ctx, `DELETE FROM mcp_credentials WHERE id=? AND server_id=?`, id, serverID)
}

// ---------- policies ----------

// AddMCPPolicy inserts a tool policy.
func (s *Store) AddMCPPolicy(ctx context.Context, p *MCPPolicy) error {
	p.Tool, p.Agent = strings.TrimSpace(p.Tool), strings.TrimSpace(p.Agent)
	if p.Tool == "" {
		return errors.New("tool is required (use * for every tool)")
	}
	if p.Action != "allow" && p.Action != "deny" {
		return errors.New(`action must be "allow" or "deny"`)
	}
	if p.RPM < 0 {
		return errors.New("rpm must be >= 0")
	}
	p.ID, p.CreatedAt = secure.RandomID("mcpp_"), time.Now().UTC()
	_, err := s.exec(ctx, `INSERT INTO mcp_policies(id,org_id,server_id,agent,tool,action,rpm,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, p.OrgID, p.ServerID, p.Agent, p.Tool, p.Action, p.RPM, ms(p.CreatedAt))
	return err
}

// ListMCPPolicies lists policies for a server ("" = every server in the org).
func (s *Store) ListMCPPolicies(ctx context.Context, orgID, serverID string) ([]*MCPPolicy, error) {
	q, args := `SELECT id,org_id,server_id,agent,tool,action,rpm,created_at FROM mcp_policies WHERE org_id=?`, []any{orgID}
	if serverID != "" {
		q, args = q+` AND server_id=?`, append(args, serverID)
	}
	rows, err := s.query(ctx, q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MCPPolicy{}
	for rows.Next() {
		var p MCPPolicy
		var t int64
		if err := rows.Scan(&p.ID, &p.OrgID, &p.ServerID, &p.Agent, &p.Tool, &p.Action, &p.RPM, &t); err != nil {
			return nil, err
		}
		p.CreatedAt = fromMS(t)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// GetMCPPolicy loads one policy.
func (s *Store) GetMCPPolicy(ctx context.Context, orgID, id string) (*MCPPolicy, error) {
	ps, err := s.ListMCPPolicies(ctx, orgID, "")
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

// DeleteMCPPolicy removes a policy.
func (s *Store) DeleteMCPPolicy(ctx context.Context, orgID, id string) error {
	return s.execOne(ctx, `DELETE FROM mcp_policies WHERE id=? AND org_id=?`, id, orgID)
}

// ---------- call log ----------

// InsertMCPCall appends a call log row.
func (s *Store) InsertMCPCall(ctx context.Context, c *MCPCall) error {
	if c.ID == "" {
		c.ID = secure.RandomID("mcp_")
	}
	if c.TS.IsZero() {
		c.TS = time.Now().UTC()
	}
	_, err := s.exec(ctx, `INSERT INTO mcp_calls(id,org_id,ts,project_id,key_id,agent,server_id,server_name,method,tool,args,redacted,status,error_code,error_message,latency_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.OrgID, ms(c.TS), c.ProjectID, c.KeyID, c.Agent, c.ServerID, c.ServerName, c.Method, c.Tool, c.Args, b2i(c.Redacted), c.Status, c.ErrorCode, c.ErrorMessage, c.LatencyMS)
	return err
}

// MCPCallFilter narrows ListMCPCalls.
type MCPCallFilter struct {
	OrgID, ServerID, Agent, Tool string
	ErrorsOnly                   bool
	Limit, Offset                int
}

// ListMCPCalls returns matching calls, newest first, and the match count.
func (s *Store) ListMCPCalls(ctx context.Context, f MCPCallFilter) ([]*MCPCall, int, error) {
	where, args := `org_id=?`, []any{f.OrgID}
	if f.ServerID != "" {
		where, args = where+` AND server_id=?`, append(args, f.ServerID)
	}
	if f.Agent != "" {
		where, args = where+` AND agent=?`, append(args, f.Agent)
	}
	if f.Tool != "" {
		where, args = where+` AND tool=?`, append(args, f.Tool)
	}
	if f.ErrorsOnly {
		where += ` AND (status>=400 OR error_code<>'')`
	}
	var total int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM mcp_calls WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	rows, err := s.query(ctx, `SELECT id,org_id,ts,project_id,key_id,agent,server_id,server_name,method,tool,args,redacted,status,error_code,error_message,latency_ms FROM mcp_calls WHERE `+where+` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`, append(args, f.Limit, max(0, f.Offset))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*MCPCall{}
	for rows.Next() {
		var c MCPCall
		var t int64
		var red int
		if err := rows.Scan(&c.ID, &c.OrgID, &t, &c.ProjectID, &c.KeyID, &c.Agent, &c.ServerID, &c.ServerName, &c.Method, &c.Tool, &c.Args, &red, &c.Status, &c.ErrorCode, &c.ErrorMessage, &c.LatencyMS); err != nil {
			return nil, 0, err
		}
		c.TS, c.Redacted = fromMS(t), red != 0
		out = append(out, &c)
	}
	return out, total, rows.Err()
}
