package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/budget"
	"github.com/saief1/opengate/internal/guard"
	"github.com/saief1/opengate/internal/secure"
	"github.com/saief1/opengate/internal/store"
)

// MCP gateway: /mcp/{server} proxies Model Context Protocol traffic
// (streamable HTTP, or the older HTTP+SSE transport) to an upstream server
// configured by an admin. Agents authenticate with their OpenGate virtual
// key; OpenGate enforces the kill switch, budgets, per-agent tool
// allow/deny policies and per-tool rate limits, injects the upstream
// credential (never exposed to the agent) and logs every tool call with
// redacted arguments.

// JSON-RPC error codes OpenGate returns for MCP requests it refuses.
const (
	mcpErrDenied  = -32001 // tool not allowed by policy
	mcpErrLimited = -32002 // rate limited
	mcpErrBlocked = -32003 // kill switch / budget
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type toolCall struct {
	id   string // raw JSON id
	name string
	args json.RawMessage
	log  *store.MCPCall
}

// parseRPC accepts one JSON-RPC message or a batch.
func parseRPC(body []byte) ([]rpcMsg, bool, error) {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return nil, false, errors.New("empty body")
	}
	if t[0] == '[' {
		var ms []rpcMsg
		err := json.Unmarshal(t, &ms)
		return ms, true, err
	}
	var m rpcMsg
	err := json.Unmarshal(t, &m)
	return []rpcMsg{m}, false, err
}

func rpcError(id json.RawMessage, code int, msg string, data any) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": e}
}

// mcpDecision is the policy outcome for one tool.
type mcpDecision struct {
	Allowed bool
	RPM     int
	Rule    *store.MCPPolicy
}

// decideTool picks the most specific matching rule: an agent-specific rule
// beats an all-agents rule, an exact tool name beats a wildcard, and deny
// beats allow at equal specificity. No rule = allowed unless defaultDeny.
func decideTool(rules []*store.MCPPolicy, agent, tool string, defaultDeny bool, defaultRPM int) mcpDecision {
	var best *store.MCPPolicy
	bestScore := -1
	for _, r := range rules {
		if r.Agent != "" && r.Agent != agent {
			continue
		}
		if !matchModel(r.Tool, tool) {
			continue
		}
		score := 0
		if r.Agent != "" {
			score += 4
		}
		if !strings.HasSuffix(r.Tool, "*") {
			score += 2
		} else if r.Tool != "*" {
			score++
		}
		if score > bestScore || (score == bestScore && r.Action == "deny" && best.Action != "deny") {
			best, bestScore = r, score
		}
	}
	if best == nil {
		return mcpDecision{Allowed: !defaultDeny, RPM: defaultRPM}
	}
	d := mcpDecision{Allowed: best.Action == "allow", RPM: best.RPM, Rule: best}
	if d.RPM == 0 {
		d.RPM = defaultRPM
	}
	return d
}

func mcpAgent(k *store.VirtualKey) string {
	if k.Agent != "" {
		return k.Agent
	}
	return k.ID
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key, proj, st, code, msg := s.authenticate(r)
	if key == nil {
		writeError(w, st, "invalid_request_error", code, msg)
		return
	}
	srv, err := s.store.MCPServerByName(r.Context(), proj.OrgID, r.PathValue("server"))
	if err != nil || !srv.Enabled {
		writeError(w, http.StatusNotFound, "invalid_request_error", "mcp_server_not_found", "OpenGate: unknown or disabled MCP server.")
		return
	}
	agent := mcpAgent(key)
	base := &store.MCPCall{OrgID: proj.OrgID, ProjectID: proj.ID, KeyID: key.ID, Agent: agent, ServerID: srv.ID, ServerName: srv.Name}

	var body []byte
	var msgs []rpcMsg
	var calls []*toolCall
	if r.Method == http.MethodPost {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "Request body too large or unreadable.")
			return
		}
		if msgs, _, err = parseRPC(body); err != nil {
			writeJSON(w, http.StatusBadRequest, rpcError(nil, -32700, "Parse error", nil))
			return
		}
		for _, m := range msgs {
			if m.Method != "tools/call" {
				continue
			}
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(m.Params, &p)
			c := *base
			c.ID, c.TS, c.Method, c.Tool = secure.RandomID("mcp_"), start.UTC(), m.Method, p.Name
			c.Args, c.Redacted = s.redactArgs(p.Arguments)
			calls = append(calls, &toolCall{id: string(m.ID), name: p.Name, args: p.Arguments, log: &c})
		}
	}
	finish := func(status int, code, msg string) {
		for _, c := range calls {
			if c.log.Status == 0 {
				c.log.Status = status
			}
			if c.log.ErrorCode == "" && code != "" {
				c.log.ErrorCode, c.log.ErrorMessage = code, msg
			}
			c.log.LatencyMS = time.Since(start).Milliseconds()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.store.InsertMCPCall(ctx, c.log); err != nil {
				s.log.Error("insert mcp call", "err", err)
			}
			cancel()
		}
	}
	refuse := func(status, rpcCode int, code, msg string) {
		var id json.RawMessage
		if len(msgs) == 1 {
			id = msgs[0].ID
		}
		writeJSON(w, status, rpcError(id, rpcCode, msg, map[string]string{"opengate_code": code}))
		finish(status, code, msg)
	}

	// Kill switch and budget apply to every MCP request of the agent.
	if key.Killed {
		refuse(http.StatusForbidden, mcpErrBlocked, "key_killed", "OpenGate: this key has been disabled by the kill switch.")
		return
	}
	if proj.Killed {
		refuse(http.StatusForbidden, mcpErrBlocked, "project_killed", fmt.Sprintf("OpenGate: project %q has been disabled by the kill switch.", proj.Name))
		return
	}
	// Tool calls count against the key/agent/project budgets and request
	// rate limits like model requests do (an agent over budget cannot keep
	// acting through tools).
	if len(calls) > 0 {
		if d, err := s.enforcer.Check(r.Context(), budget.ScopesFor(key, proj)); err != nil {
			refuse(http.StatusServiceUnavailable, mcpErrBlocked, "budget_unavailable", "OpenGate: budget check failed.")
			return
		} else if d != nil {
			if d.RetryAfter > 0 {
				w.Header().Set("Retry-After", itoa(int(d.RetryAfter.Seconds())+1))
			}
			code := mcpErrBlocked
			if d.Code == "rate_limit_exceeded" {
				code = mcpErrLimited
			}
			refuse(d.Status, code, d.Code, d.Message)
			return
		}
	}

	rules, err := s.store.ListMCPPolicies(r.Context(), proj.OrgID, srv.ID)
	if err != nil {
		refuse(http.StatusInternalServerError, -32603, "internal_error", "OpenGate: policy lookup failed.")
		return
	}
	for _, c := range calls {
		dec := decideTool(rules, agent, c.name, s.cfg.MCP.DefaultDeny, srv.DefaultRPM)
		if !dec.Allowed {
			c.log.Status, c.log.ErrorCode, c.log.ErrorMessage = http.StatusForbidden, "tool_not_allowed", fmt.Sprintf("OpenGate: tool %q is not allowed for agent %q on %s.", c.name, agent, srv.Name)
			refuse(http.StatusForbidden, mcpErrDenied, "tool_not_allowed", c.log.ErrorMessage)
			return
		}
		if dec.RPM > 0 {
			ok, wait, err := s.mcpRates.Allow(r.Context(), "mcp|"+srv.ID+"|"+agent+"|"+c.name, dec.RPM, time.Now())
			if err == nil && !ok {
				w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
				c.log.Status, c.log.ErrorCode = http.StatusTooManyRequests, "rate_limit_exceeded"
				refuse(http.StatusTooManyRequests, mcpErrLimited, "rate_limit_exceeded", fmt.Sprintf("OpenGate: tool %q is limited to %d calls/min for this agent.", c.name, dec.RPM))
				return
			}
		}
	}

	// Forward upstream.
	target, err := s.mcpTarget(srv, r)
	if err != nil {
		refuse(http.StatusBadRequest, -32600, "invalid_request", err.Error())
		return
	}
	timeout := s.cfg.MCP.Timeout
	if r.Method == http.MethodGet {
		timeout = s.cfg.MCP.StreamTimeout
	}
	ctx, cancelCause := context.WithCancelCause(r.Context())
	defer cancelCause(nil)
	ctx, cancelT := context.WithTimeout(ctx, timeout)
	defer cancelT()
	flightID := secure.RandomID("mcpf_")
	s.inflight.add(flightID, flight{keyID: key.ID, projectID: proj.ID, cancel: cancelCause})
	defer s.inflight.remove(flightID)

	var rb io.Reader
	if body != nil {
		rb = bytes.NewReader(body)
	}
	up, _ := http.NewRequestWithContext(ctx, r.Method, target, rb)
	for _, h := range []string{"Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id"} {
		if v := r.Header.Get(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	if up.Header.Get("Accept") == "" {
		up.Header.Set("Accept", "application/json, text/event-stream")
	}
	if err := s.injectMCPCredential(r.Context(), up, srv, agent); err != nil {
		s.log.Error("mcp credential", "err", err, "server", srv.Name)
		refuse(http.StatusBadGateway, -32603, "credential_error", "OpenGate: upstream credential could not be decrypted.")
		return
	}
	resp, err := s.mcpClient.Do(up)
	if err != nil {
		if errors.Is(context.Cause(ctx), errKilled) {
			refuse(http.StatusForbidden, mcpErrBlocked, "killed", "OpenGate: request aborted by the kill switch.")
			return
		}
		refuse(http.StatusBadGateway, -32603, "upstream_error", "OpenGate: MCP upstream unreachable.")
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("X-OpenGate-Agent", agent)
	byID := map[string]*toolCall{}
	for _, c := range calls {
		byID[c.id] = c
	}
	// rewrite inspects one upstream JSON-RPC payload: it filters tools/list
	// results by policy and records tool-call outcomes.
	rewrite := func(payload []byte) []byte {
		var one rpcMsg
		var many []rpcMsg
		batch := false
		if t := bytes.TrimSpace(payload); len(t) > 0 && t[0] == '[' {
			if json.Unmarshal(t, &many) != nil {
				return payload
			}
			batch = true
		} else if json.Unmarshal(payload, &one) != nil {
			return payload
		} else {
			many = []rpcMsg{one}
		}
		changed := false
		for i := range many {
			m := &many[i]
			if c := byID[string(m.ID)]; c != nil && len(m.ID) > 0 {
				if len(m.Error) > 0 {
					c.log.ErrorCode, c.log.ErrorMessage = "tool_error", truncateStr(string(m.Error), 300)
				} else {
					var res struct {
						IsError bool `json:"isError"`
					}
					if json.Unmarshal(m.Result, &res) == nil && res.IsError {
						c.log.ErrorCode = "tool_error"
					}
				}
			}
			if len(m.Result) > 0 {
				if nr, ok := s.filterToolList(m.Result, rules, agent, srv.DefaultRPM); ok {
					m.Result, changed = nr, true
				}
			}
		}
		if !changed {
			return payload
		}
		var out []byte
		if batch {
			out, _ = json.Marshal(many)
		} else {
			out, _ = json.Marshal(many[0])
		}
		return out
	}
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		s.pipeSSE(w, resp.Body, srv, rewrite)
	case strings.HasPrefix(ct, "application/json"):
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		b = rewrite(b)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(b)
	default:
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
	if errors.Is(context.Cause(ctx), errKilled) {
		finish(http.StatusForbidden, "killed", "OpenGate: aborted by the kill switch.")
		return
	}
	finish(resp.StatusCode, "", "")
}

// pipeSSE streams an upstream event stream, rewriting JSON data lines and
// the legacy-transport "endpoint" event so follow-up POSTs come back
// through OpenGate.
func (s *Server) pipeSSE(w http.ResponseWriter, body io.Reader, srv *store.MCPServer, rewrite func([]byte) []byte) {
	fl, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if event == "endpoint" {
				line = "data: " + s.rewriteEndpoint(srv, data)
			} else if nd := rewrite([]byte(data)); !bytes.Equal(nd, []byte(data)) {
				line = "data: " + string(nd)
			}
		case line == "":
			event = ""
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return
		}
		if line == "" && fl != nil {
			fl.Flush()
		}
	}
	if fl != nil {
		fl.Flush()
	}
}

// rewriteEndpoint maps an upstream message endpoint onto /mcp/{name}/...
func (s *Server) rewriteEndpoint(srv *store.MCPServer, data string) string {
	b, err := url.Parse(srv.URL)
	if err != nil {
		return data
	}
	u, err := b.Parse(data)
	if err != nil || u.Host != b.Host {
		return "/mcp/" + srv.Name // foreign host: never let the client bypass the gateway
	}
	out := "/mcp/" + srv.Name + "/" + strings.TrimPrefix(u.Path, "/")
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// mcpTarget is the upstream URL for a request: the server URL itself, or
// (for /mcp/{name}/{rest}) a path on the same upstream host.
func (s *Server) mcpTarget(srv *store.MCPServer, r *http.Request) (string, error) {
	rest := r.PathValue("rest")
	if rest == "" {
		u, _ := url.Parse(srv.URL)
		if r.URL.RawQuery != "" {
			q := u.Query()
			for k, v := range r.URL.Query() {
				q[k] = v
			}
			u.RawQuery = q.Encode()
		}
		return u.String(), nil
	}
	if strings.Contains(rest, "..") {
		return "", errors.New("invalid path")
	}
	b, err := url.Parse(srv.URL)
	if err != nil {
		return "", err
	}
	u := &url.URL{Scheme: b.Scheme, Host: b.Host, Path: "/" + rest, RawQuery: r.URL.RawQuery}
	return u.String(), nil
}

// injectMCPCredential sets the agent's credential, else the server's.
func (s *Server) injectMCPCredential(ctx context.Context, up *http.Request, srv *store.MCPServer, agent string) error {
	if c, err := s.store.MCPCredentialFor(ctx, srv.ID, agent); err == nil {
		pt, err := s.cipher.Decrypt(c.SecretCT, store.MCPSecretAAD(c.ID))
		if err != nil {
			return err
		}
		up.Header.Set(c.Header, string(pt))
		return nil
	}
	if srv.SecretCT == "" {
		return nil
	}
	pt, err := s.cipher.Decrypt(srv.SecretCT, store.MCPSecretAAD(srv.ID))
	if err != nil {
		return err
	}
	up.Header.Set(srv.AuthHeader, string(pt))
	return nil
}

// filterToolList removes tools the agent may not call from a tools/list
// result. ok is false when the result is not a tool list or unchanged.
func (s *Server) filterToolList(result json.RawMessage, rules []*store.MCPPolicy, agent string, rpm int) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(result, &obj) != nil || obj["tools"] == nil {
		return nil, false
	}
	var tools []json.RawMessage
	if json.Unmarshal(obj["tools"], &tools) != nil {
		return nil, false
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var n struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &n)
		if decideTool(rules, agent, n.Name, s.cfg.MCP.DefaultDeny, rpm).Allowed {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(tools) {
		return nil, false
	}
	obj["tools"], _ = json.Marshal(kept)
	out, _ := json.Marshal(obj)
	return out, true
}

// redactArgs serializes tool arguments with PII/secrets replaced by the
// guardrail placeholders, truncated to mcp.args_log_max_bytes.
func (s *Server) redactArgs(args json.RawMessage) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	var v any
	if json.Unmarshal(args, &v) != nil {
		return "", false
	}
	sc := guard.NewScanner(nil)
	redacted := false
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			out, f := sc.Scan(t, true)
			if len(f.Categories()) > 0 {
				redacted = true
			}
			return out
		case map[string]any:
			for k, vv := range t {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "password") || strings.Contains(lk, "secret") || strings.Contains(lk, "token") || lk == "api_key" || lk == "apikey" || lk == "authorization" {
					if _, isStr := vv.(string); isStr {
						t[k] = guard.Placeholder(guard.Secret)
						redacted = true
						continue
					}
				}
				t[k] = walk(vv)
			}
			return t
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		}
		return x
	}
	b, _ := json.Marshal(walk(v))
	return truncateStr(string(b), s.cfg.MCP.ArgsLogMaxBytes), redacted
}

func truncateStr(s string, n int) string {
	if n > 0 && len(s) > n {
		return s[:n] + "…[truncated]"
	}
	return s
}
