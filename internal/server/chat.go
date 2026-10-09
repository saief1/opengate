package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opengate-dev/opengate/internal/budget"
	"github.com/opengate-dev/opengate/internal/provider"
	"github.com/opengate-dev/opengate/internal/secure"
	"github.com/opengate-dev/opengate/internal/store"
)

type targetSpec struct {
	Provider string
	Model    string
}

// resolve maps a requested model (alias or provider/model) to an ordered
// list of targets.
func (s *Server) resolve(model string) ([]targetSpec, error) {
	list := s.cfg.Routes[model]
	if len(list) == 0 {
		list = []string{model}
	}
	out := make([]targetSpec, 0, len(list))
	for _, t := range list {
		prov, m, ok := strings.Cut(t, "/")
		if !ok || m == "" {
			return nil, fmt.Errorf("model %q must be \"provider/model\" (e.g. openai/gpt-4o) or a configured alias", model)
		}
		if _, ok := s.cfg.Providers[prov]; !ok {
			return nil, fmt.Errorf("unknown provider %q in model %q", prov, t)
		}
		out = append(out, targetSpec{Provider: prov, Model: m})
	}
	return out, nil
}

// authenticate validates the virtual key.
func (s *Server) authenticate(r *http.Request) (*store.VirtualKey, *store.Project, int, string, string) {
	auth := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	tok = strings.TrimSpace(tok)
	if !ok || tok == "" {
		return nil, nil, http.StatusUnauthorized, "missing_api_key", "Missing OpenGate virtual key. Send 'Authorization: Bearer og-...'."
	}
	if !secure.LooksLikeVirtualKey(tok) {
		return nil, nil, http.StatusUnauthorized, "invalid_api_key", "Invalid OpenGate virtual key."
	}
	k, err := s.store.KeyByHash(r.Context(), secure.HashKey(tok))
	if err != nil || k.Revoked {
		return nil, nil, http.StatusUnauthorized, "invalid_api_key", "Invalid OpenGate virtual key."
	}
	p, err := s.store.GetProject(r.Context(), k.ProjectID)
	if err != nil {
		return nil, nil, http.StatusUnauthorized, "invalid_api_key", "Invalid OpenGate virtual key."
	}
	return k, p, 0, "", ""
}

// reqState carries a request through the pipeline.
type reqState struct {
	log    *store.RequestLog
	key    *store.VirtualKey
	proj   *store.Project
	scopes []budget.Scope
	start  time.Time
}

func (s *Server) truncate(b []byte) string {
	if len(b) > s.cfg.BodyLogMaxBytes {
		return string(b[:s.cfg.BodyLogMaxBytes]) + "…[truncated]"
	}
	return string(b)
}

// finish persists the log and records spend.
func (s *Server) finish(rs *reqState) {
	rs.log.LatencyMS = time.Since(rs.start).Milliseconds()
	// Use a fresh context: the client may already be gone, but the spend must be recorded.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.InsertLog(ctx, rs.log); err != nil {
		s.log.Error("insert request log", "err", err, "id", rs.log.ID)
	}
	if rs.scopes != nil {
		s.enforcer.Record(ctx, rs.scopes, rs.log.CostUSD)
	}
}

func (s *Server) reject(w http.ResponseWriter, rs *reqState, status int, typ, code, msg string, retryAfter time.Duration) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	}
	writeError(w, status, typ, code, msg)
	if rs != nil {
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = status, code, msg
		s.finish(rs)
	}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key, proj, st, code, msg := s.authenticate(r)
	if key == nil {
		s.reject(w, nil, st, "invalid_request_error", code, msg, 0)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	rs := &reqState{key: key, proj: proj, start: start, log: &store.RequestLog{
		ID: secure.RandomID("req_"), TS: start.UTC(), ProjectID: proj.ID, KeyID: key.ID, Agent: key.Agent, UserLabel: key.UserLabel,
	}}
	if err != nil {
		s.reject(w, rs, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "Request body too large or unreadable.", 0)
		return
	}
	var raw map[string]json.RawMessage
	var req provider.ChatRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Request body must be a JSON object.", 0)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "invalid_request", "Invalid request: "+err.Error(), 0)
		return
	}
	rs.log.Model, rs.log.Stream = req.Model, req.Stream
	if proj.LogBodies {
		rs.log.RequestBody = s.truncate(body)
	}
	if req.Model == "" || len(req.Messages) == 0 {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "invalid_request", "'model' and 'messages' are required.", 0)
		return
	}
	if key.Killed {
		s.reject(w, rs, http.StatusForbidden, "permission_error", "key_killed", "OpenGate: this key has been disabled by the kill switch.", 0)
		return
	}
	if proj.Killed {
		s.reject(w, rs, http.StatusForbidden, "permission_error", "project_killed", fmt.Sprintf("OpenGate: project %q has been disabled by the kill switch.", proj.Name), 0)
		return
	}
	targets, err := s.resolve(req.Model)
	if err != nil {
		s.reject(w, rs, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error(), 0)
		return
	}
	rs.scopes = budget.ScopesFor(key, proj)
	denial, err := s.enforcer.Check(r.Context(), rs.scopes)
	if err != nil {
		s.reject(w, rs, http.StatusInternalServerError, "server_error", "internal_error", "budget check failed", 0)
		return
	}
	if denial != nil {
		s.reject(w, rs, denial.Status, denial.Type, denial.Code, denial.Message, denial.RetryAfter)
		return
	}

	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	s.inflight.add(rs.log.ID, flight{keyID: key.ID, projectID: proj.ID, cancel: cancel})
	defer s.inflight.remove(rs.log.ID)

	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	mkCall := func(t targetSpec) (*provider.Call, provider.Adapter, error) {
		pc := s.cfg.Providers[t.Provider]
		apiKey, err := s.apiKeyFor(ctx, t.Provider)
		if err != nil {
			return nil, nil, err
		}
		return &provider.Call{Req: &req, Raw: raw, IncludeUsage: includeUsage, DefaultMaxTokens: s.cfg.DefaultMaxTokens,
			Target: provider.Target{Provider: t.Provider, Model: t.Model, Config: pc, APIKey: apiKey}}, s.adapters[pc.Type], nil
	}
	if req.Stream {
		s.streamChat(ctx, w, rs, &req, targets, mkCall)
	} else {
		s.unaryChat(ctx, w, rs, &req, targets, mkCall)
	}
}

type callFactory func(t targetSpec) (*provider.Call, provider.Adapter, error)

func (s *Server) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := s.cfg.Retry.InitialBackoff << (attempt - 1)
	if d > s.cfg.Retry.MaxBackoff || d <= 0 {
		d = s.cfg.Retry.MaxBackoff
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

// attemptLoop tries targets in order with per-target retries. do returns an
// error for a failed attempt; the loop decides whether to retry, fail over or stop.
func (s *Server) attemptLoop(ctx context.Context, rs *reqState, targets []targetSpec, mk callFactory,
	do func(c *provider.Call, a provider.Adapter) error) error {
	var lastErr error
	for _, t := range targets {
		call, adapter, err := mk(t)
		if err != nil {
			lastErr = err
			continue
		}
		rs.log.Provider, rs.log.ProviderModel = t.Provider, t.Model
		var ue *provider.UpstreamError
		for attempt := 0; attempt < s.cfg.Retry.MaxAttempts; attempt++ {
			if attempt > 0 {
				wait := s.backoff(attempt, ue.RetryAfter)
				if wait > s.cfg.Retry.MaxBackoff {
					break // provider asked us to wait too long: fail over instead
				}
				if err := s.sleep(ctx, wait); err != nil {
					return err
				}
			}
			rs.log.Attempts++
			err := do(call, adapter)
			if err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			if !errors.As(err, &ue) {
				return err
			}
			s.log.Warn("upstream attempt failed", "req", rs.log.ID, "provider", t.Provider, "model", t.Model, "attempt", attempt+1, "err", ue.Error())
			if !ue.Retryable() {
				break
			}
		}
		if ue != nil && !ue.Failover() {
			return lastErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no targets available")
	}
	return lastErr
}

// writeUpstreamFailure maps a final error onto the response and log.
func (s *Server) writeUpstreamFailure(ctx context.Context, w http.ResponseWriter, rs *reqState, err error) {
	if cause := context.Cause(ctx); errors.Is(cause, errKilled) {
		s.reject(w, rs, http.StatusForbidden, "permission_error", "killed", "OpenGate: request aborted by the kill switch.", 0)
		return
	}
	if ctx.Err() != nil {
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = 499, "client_closed", "client closed request"
		s.finish(rs)
		return
	}
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		status := ue.Status
		if status == 0 || status >= 500 {
			status = http.StatusBadGateway
		}
		typ := "upstream_error"
		if status == http.StatusBadRequest {
			typ = "invalid_request_error"
		}
		s.reject(w, rs, status, typ, "upstream_error", fmt.Sprintf("All upstream attempts failed (%d). Last error: %s", rs.log.Attempts, ue.Error()), 0)
		return
	}
	s.reject(w, rs, http.StatusBadGateway, "upstream_error", "upstream_error", err.Error(), 0)
}

func estimateTokens(chars int) int { return (chars + 3) / 4 }

func promptChars(req *provider.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += len(m.Content) + len(m.Role)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments) + len(tc.Function.Name)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Function.Parameters) + len(t.Function.Description)
	}
	return n
}

// price finalizes token counts and cost for the log.
func (s *Server) price(rs *reqState, req *provider.ChatRequest, u provider.Usage, reported bool, outChars int) {
	if !reported {
		u.PromptTokens = estimateTokens(promptChars(req))
		u.CompletionTokens = estimateTokens(outChars)
		rs.log.UsageEstimated = true
	} else if u.CompletionTokens == 0 && outChars > 0 {
		u.CompletionTokens = estimateTokens(outChars)
		rs.log.UsageEstimated = true
	}
	rs.log.PromptTokens, rs.log.CompletionTokens = u.PromptTokens, u.CompletionTokens
	rs.log.CostUSD, _ = s.pricing.Table().Cost(rs.log.Provider, rs.log.ProviderModel, u.PromptTokens, u.CompletionTokens)
}

func (s *Server) gatewayHeaders(w http.ResponseWriter, rs *reqState) {
	h := w.Header()
	h.Set("X-OpenGate-Request-Id", rs.log.ID)
	h.Set("X-OpenGate-Provider", rs.log.Provider)
	h.Set("X-OpenGate-Model", rs.log.Provider+"/"+rs.log.ProviderModel)
	h.Set("X-OpenGate-Attempts", strconv.Itoa(rs.log.Attempts))
}

func (s *Server) unaryChat(ctx context.Context, w http.ResponseWriter, rs *reqState, req *provider.ChatRequest, targets []targetSpec, mk callFactory) {
	var res *provider.Result
	err := s.attemptLoop(ctx, rs, targets, mk, func(c *provider.Call, a provider.Adapter) error {
		var err error
		res, err = a.Chat(ctx, c)
		return err
	})
	if err != nil {
		s.writeUpstreamFailure(ctx, w, rs, err)
		return
	}
	outChars := 0
	if !res.UsageReported {
		var cr provider.ChatResponse
		if json.Unmarshal(res.Body, &cr) == nil {
			for _, c := range cr.Choices {
				if c.Message != nil && c.Message.Content != nil {
					outChars += len(*c.Message.Content)
				}
			}
		}
	}
	s.price(rs, req, res.Usage, res.UsageReported, outChars)
	rs.log.Status = http.StatusOK
	if rs.proj.LogBodies {
		rs.log.ResponseBody = s.truncate(res.Body)
	}
	s.gatewayHeaders(w, rs)
	w.Header().Set("X-OpenGate-Cost-USD", strconv.FormatFloat(rs.log.CostUSD, 'f', 8, 64))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Body)
	s.finish(rs)
}

func (s *Server) streamChat(ctx context.Context, w http.ResponseWriter, rs *reqState, req *provider.ChatRequest, targets []targetSpec, mk callFactory) {
	flusher, _ := w.(http.Flusher)
	var stream provider.Stream
	var first []byte
	// The first chunk is read inside the attempt loop so that a stream which
	// fails before producing output can still be retried or failed over.
	err := s.attemptLoop(ctx, rs, targets, mk, func(c *provider.Call, a provider.Adapter) error {
		st, err := a.ChatStream(ctx, c)
		if err != nil {
			return err
		}
		b, err := st.Next()
		if err != nil && !errors.Is(err, io.EOF) {
			st.Close()
			return err
		}
		stream, first = st, b
		return nil
	})
	if err != nil {
		s.writeUpstreamFailure(ctx, w, rs, err)
		return
	}
	defer stream.Close()

	s.gatewayHeaders(w, rs)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var text strings.Builder
	outChars := 0
	writeChunk := func(b []byte) error {
		var cr provider.ChatResponse
		if json.Unmarshal(b, &cr) == nil {
			for _, c := range cr.Choices {
				if c.Delta != nil {
					if c.Delta.Content != nil {
						outChars += len(*c.Delta.Content)
						if rs.proj.LogBodies && text.Len() < s.cfg.BodyLogMaxBytes {
							text.WriteString(*c.Delta.Content)
						}
					}
					for _, tc := range c.Delta.ToolCalls {
						outChars += len(tc.Function.Arguments)
					}
				}
			}
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	var streamErr error
	if first != nil {
		streamErr = writeChunk(first)
	}
	for streamErr == nil {
		b, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			streamErr = err
			break
		}
		streamErr = writeChunk(b)
	}

	u, reported := stream.Usage()
	s.price(rs, req, u, reported, outChars)
	if rs.proj.LogBodies {
		rs.log.ResponseBody = s.truncate([]byte(text.String()))
	}
	switch {
	case streamErr == nil:
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		rs.log.Status = http.StatusOK
	case errors.Is(context.Cause(ctx), errKilled):
		b, _ := json.Marshal(errorBody("permission_error", "killed", "OpenGate: stream aborted by the kill switch."))
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = http.StatusForbidden, "killed_mid_stream", "stream aborted by the kill switch"
	case ctx.Err() != nil:
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = 499, "client_closed", "client closed stream"
	default:
		b, _ := json.Marshal(errorBody("upstream_error", "stream_interrupted", "Upstream stream failed: "+streamErr.Error()))
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = http.StatusBadGateway, "stream_interrupted", streamErr.Error()
	}
	s.finish(rs)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if k, _, st, code, msg := s.authenticate(r); k == nil {
		writeError(w, st, "invalid_request_error", code, msg)
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	seen := map[string]bool{}
	data := []model{}
	add := func(id, owner string) {
		if !seen[id] {
			seen[id] = true
			data = append(data, model{ID: id, Object: "model", Created: 0, OwnedBy: owner})
		}
	}
	aliases := make([]string, 0, len(s.cfg.Routes))
	for alias := range s.cfg.Routes {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		add(alias, "opengate-alias")
	}
	for _, m := range s.pricing.Table().Models() {
		prov, _, _ := strings.Cut(m, "/")
		if _, ok := s.cfg.Providers[prov]; ok {
			add(m, prov)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
