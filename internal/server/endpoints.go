package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/opengate-dev/opengate/internal/config"
	"github.com/opengate-dev/opengate/internal/provider"
)

// ---------- /v1/chat/completions ----------

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	rs := s.begin(w, r, EndpointChat)
	if rs == nil {
		return
	}
	var req provider.ChatRequest
	if err := json.Unmarshal(rs.body, &req); err != nil {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "invalid_request", "Invalid request: "+err.Error(), 0)
		return
	}
	if len(req.Messages) == 0 {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "invalid_request", "'model' and 'messages' are required.", 0)
		return
	}
	promptEst := estimateTokens(promptChars(&req))
	estOut := req.MaxOutputTokens()
	if estOut <= 0 {
		estOut = s.cfg.DefaultMaxTokens
	}
	plan, err := s.planChat(rs, &req, promptEst, estOut)
	if err != nil {
		s.reject(w, rs, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error(), 0)
		return
	}
	targets := plan.targets
	if plan.route != "" {
		// Routing must never let a key escape its model policy: drop routed
		// targets it may not use, and fall back to the requested model.
		var allowed []targetSpec
		for _, t := range targets {
			name := t.String()
			if s.autoEnabled(req.Model) && plan.route == "auto:t"+strconv.Itoa(int(plan.class.Tier)) {
				name = req.Model // allowing "auto" allows what it picks
			}
			if policyAllows(rs.key.ModelPolicy, name, t) && policyAllows(rs.proj.ModelPolicy, name, t) {
				allowed = append(allowed, t)
			}
		}
		if len(allowed) == 0 && !s.autoEnabled(req.Model) {
			plan.route, plan.baseline = "", req.Model
			if allowed, err = s.resolve(req.Model); err != nil {
				s.reject(w, rs, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error(), 0)
				return
			}
		}
		targets = allowed
	}
	rs.log.Route = plan.route
	targets, ok := s.filterTargets(w, rs, req.Model, targets)
	if !ok {
		return
	}
	if plan.route != "" {
		w.Header().Set("X-OpenGate-Route", plan.route)
	}
	cap := tokenCap(rs)
	if cap > 0 {
		if mo := req.MaxOutputTokens(); mo > cap {
			s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "max_tokens_exceeded",
				fmt.Sprintf("OpenGate: max_tokens %d exceeds this key's limit of %d.", mo, cap), 0)
			return
		} else if mo <= 0 {
			req.MaxCompletionTokens = &cap
		}
	}
	outTokens := req.MaxOutputTokens()
	if outTokens <= 0 {
		outTokens = s.cfg.DefaultMaxTokens
	}
	ttl := s.cacheTTL(rs)
	cacheKey := ""
	if ttl > 0 {
		cacheKey = s.cacheKey(rs)
		if s.serveCached(w, rs, cacheKey) {
			return
		}
		w.Header().Set("X-OpenGate-Cache", "MISS")
	}
	if !s.reserve(w, rs, s.estimateCost(targets, promptEst, outTokens)) {
		return
	}
	rs.baseline = plan.baseline
	rs.cacheKey, rs.cacheTTL = cacheKey, ttl
	ctx, done := s.withInflight(rs)
	defer done()

	includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	mk := func(t provider.Target) (*provider.Call, provider.Adapter) {
		return &provider.Call{Req: &req, Raw: rs.raw, IncludeUsage: includeUsage, DefaultMaxTokens: s.cfg.DefaultMaxTokens,
			CapTokens: cap, Target: t}, s.adapters[t.Config.Type]
	}
	if req.Stream {
		s.streamChat(ctx, w, rs, promptEst, targets, mk)
	} else {
		s.unaryChat(ctx, w, rs, promptEst, targets, mk)
	}
}

type callFactory func(t provider.Target) (*provider.Call, provider.Adapter)

func (s *Server) unaryChat(ctx context.Context, w http.ResponseWriter, rs *reqState, promptEst int, targets []targetSpec, mk callFactory) {
	var res *provider.Result
	err := s.attemptLoop(ctx, rs, targets, func(t provider.Target) error {
		c, a := mk(t)
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
	s.price(rs, res.Usage, res.UsageReported, promptEst, outChars)
	body, withhold := s.guardResponse(rs, res.Body)
	if withhold {
		s.reject(w, rs, http.StatusBadGateway, "upstream_error", "guardrail_blocked",
			"OpenGate: the upstream response was withheld by the data guardrail (found: "+rs.log.Guard+").", 0)
		return
	}
	res.Body = body
	rs.log.Status = http.StatusOK
	if rs.proj.LogBodies {
		rs.log.ResponseBody = s.truncate(res.Body)
	}
	if rs.cacheTTL > 0 {
		s.storeCached(rs, rs.cacheKey, rs.cacheTTL, res.Body)
	}
	s.gatewayHeaders(w, rs)
	w.Header().Set("X-OpenGate-Cost-USD", strconv.FormatFloat(rs.log.CostUSD, 'f', 8, 64))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Body)
	s.finish(rs)
}

func (s *Server) streamChat(ctx context.Context, w http.ResponseWriter, rs *reqState, promptEst int, targets []targetSpec, mk callFactory) {
	flusher, _ := w.(http.Flusher)
	var stream provider.Stream
	var first []byte
	// The first chunk is read inside the attempt loop so that a stream which
	// fails before producing output can still be retried or failed over.
	err := s.attemptLoop(ctx, rs, targets, func(t provider.Target) error {
		c, a := mk(t)
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
	sseHeaders(w)
	w.WriteHeader(http.StatusOK)

	var text strings.Builder
	outChars := 0
	guardStream := s.guardSettings(rs.proj).Responses
	writeChunk := func(b []byte) error {
		var cr provider.ChatResponse
		if json.Unmarshal(b, &cr) == nil {
			for _, c := range cr.Choices {
				if c.Delta != nil {
					if c.Delta.Content != nil {
						outChars += len(*c.Delta.Content)
						if (rs.proj.LogBodies || guardStream) && text.Len() < 256<<10 {
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
	s.price(rs, u, reported, promptEst, outChars)
	s.guardStreamText(rs, text.String())
	if rs.proj.LogBodies {
		rs.log.ResponseBody = s.truncate([]byte(text.String()))
	}
	s.endStream(ctx, w, rs, streamErr, func(b []byte) { fmt.Fprintf(w, "data: %s\n\n", b) }, func() { fmt.Fprint(w, "data: [DONE]\n\n") })
	s.finish(rs)
}

// endStream writes the terminal SSE frame and sets the log status.
func (s *Server) endStream(ctx context.Context, w http.ResponseWriter, rs *reqState, streamErr error, writeErr func([]byte), writeDone func()) {
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	switch {
	case streamErr == nil:
		if writeDone != nil {
			writeDone()
		}
		flush()
		rs.log.Status = http.StatusOK
	case errors.Is(context.Cause(ctx), errKilled):
		b, _ := json.Marshal(errorBody("permission_error", "killed", "OpenGate: stream aborted by the kill switch."))
		writeErr(b)
		flush()
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = http.StatusForbidden, "killed_mid_stream", "stream aborted by the kill switch"
	case ctx.Err() != nil:
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = 499, "client_closed", "client closed stream"
	default:
		b, _ := json.Marshal(errorBody("upstream_error", "stream_interrupted", "Upstream stream failed: "+streamErr.Error()))
		writeErr(b)
		flush()
		rs.log.Status, rs.log.ErrorCode, rs.log.ErrorMessage = http.StatusBadGateway, "stream_interrupted", streamErr.Error()
	}
}

// ---------- /v1/embeddings ----------

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	rs := s.begin(w, r, EndpointEmbeddings)
	if rs == nil {
		return
	}
	rs.log.Stream = false
	if len(rs.raw["input"]) == 0 {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "invalid_request", "'input' is required.", 0)
		return
	}
	inputs, _ := provider.ParseEmbeddingInput(rs.raw["input"])
	targets, err := s.resolve(rs.log.Model)
	if err != nil {
		s.reject(w, rs, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error(), 0)
		return
	}
	var capable []targetSpec
	for _, t := range targets {
		if _, ok := s.adapters[s.cfg.Providers[t.Provider].Type].(provider.Embedder); ok {
			capable = append(capable, t)
		}
	}
	if len(capable) == 0 {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "unsupported_endpoint",
			fmt.Sprintf("OpenGate: %q has no provider that supports embeddings (supported: openai, openrouter, gemini).", rs.log.Model), 0)
		return
	}
	targets, ok := s.filterTargets(w, rs, rs.log.Model, capable)
	if !ok {
		return
	}
	chars := len(rs.raw["input"])
	if inputs != nil {
		chars = 0
		for _, in := range inputs {
			chars += len(in)
		}
	}
	promptEst := estimateTokens(chars)
	ttl := s.cacheTTL(rs)
	cacheKey := ""
	if ttl > 0 {
		cacheKey = s.cacheKey(rs)
		if s.serveCached(w, rs, cacheKey) {
			return
		}
		w.Header().Set("X-OpenGate-Cache", "MISS")
	}
	if !s.reserve(w, rs, s.estimateCost(targets, promptEst, 0)) {
		return
	}
	ctx, done := s.withInflight(rs)
	defer done()
	var res *provider.Result
	err = s.attemptLoop(ctx, rs, targets, func(t provider.Target) error {
		emb := s.adapters[t.Config.Type].(provider.Embedder)
		var err error
		res, err = emb.Embed(ctx, &provider.EmbedCall{Raw: rs.raw, Inputs: inputs, Target: t})
		return err
	})
	if err != nil {
		s.writeUpstreamFailure(ctx, w, rs, err)
		return
	}
	u := res.Usage
	s.price(rs, u, res.UsageReported, promptEst, 0)
	if !res.UsageReported && u.PromptTokens > 0 {
		rs.log.PromptTokens = u.PromptTokens
	}
	rs.log.Status = http.StatusOK
	if ttl > 0 {
		s.storeCached(rs, cacheKey, ttl, res.Body)
	}
	s.gatewayHeaders(w, rs)
	w.Header().Set("X-OpenGate-Cost-USD", strconv.FormatFloat(rs.log.CostUSD, 'f', 8, 64))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Body)
	s.finish(rs)
}

// ---------- /v1/responses (OpenAI Responses API passthrough) ----------

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	rs := s.begin(w, r, EndpointResponses)
	if rs == nil {
		return
	}
	if len(rs.raw["input"]) == 0 {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "invalid_request", "'input' is required.", 0)
		return
	}
	targets, err := s.resolve(rs.log.Model)
	if err != nil {
		s.reject(w, rs, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error(), 0)
		return
	}
	var capable []targetSpec
	for _, t := range targets {
		if s.cfg.Providers[t.Provider].Type == config.TypeOpenAI {
			capable = append(capable, t)
		}
	}
	if len(capable) == 0 {
		s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "unsupported_endpoint",
			fmt.Sprintf("OpenGate: the Responses API is only passed through to OpenAI-type providers; %q has none.", rs.log.Model), 0)
		return
	}
	targets, ok := s.filterTargets(w, rs, rs.log.Model, capable)
	if !ok {
		return
	}
	var maxOut int
	_ = json.Unmarshal(rs.raw["max_output_tokens"], &maxOut)
	cap := tokenCap(rs)
	if cap > 0 {
		if maxOut > cap {
			s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "max_tokens_exceeded",
				fmt.Sprintf("OpenGate: max_output_tokens %d exceeds this key's limit of %d.", maxOut, cap), 0)
			return
		}
		if maxOut <= 0 {
			maxOut = cap
			rs.raw["max_output_tokens"], _ = json.Marshal(cap)
		}
	}
	if maxOut <= 0 {
		maxOut = s.cfg.DefaultMaxTokens
	}
	promptEst := estimateTokens(len(rs.raw["input"]) + len(rs.raw["instructions"]) + len(rs.raw["tools"]))
	if !s.reserve(w, rs, s.estimateCost(targets, promptEst, maxOut)) {
		return
	}
	ctx, done := s.withInflight(rs)
	defer done()
	api := func(t provider.Target) provider.ResponsesAPI {
		return s.adapters[t.Config.Type].(provider.ResponsesAPI)
	}
	call := func(t provider.Target) *provider.Call { return &provider.Call{Raw: rs.raw, Target: t} }

	if !rs.log.Stream {
		var res *provider.Result
		err := s.attemptLoop(ctx, rs, targets, func(t provider.Target) error {
			var err error
			res, err = api(t).Responses(ctx, call(t))
			return err
		})
		if err != nil {
			s.writeUpstreamFailure(ctx, w, rs, err)
			return
		}
		s.price(rs, res.Usage, res.UsageReported, promptEst, 0)
		body, withhold := s.guardResponse(rs, res.Body)
		if withhold {
			s.reject(w, rs, http.StatusBadGateway, "upstream_error", "guardrail_blocked",
				"OpenGate: the upstream response was withheld by the data guardrail (found: "+rs.log.Guard+").", 0)
			return
		}
		res.Body = body
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
		return
	}

	var stream provider.EventStream
	var first provider.Event
	err = s.attemptLoop(ctx, rs, targets, func(t provider.Target) error {
		st, err := api(t).ResponsesStream(ctx, call(t))
		if err != nil {
			return err
		}
		ev, err := st.Next()
		if err != nil && !errors.Is(err, io.EOF) {
			st.Close()
			return err
		}
		stream, first = st, ev
		return nil
	})
	if err != nil {
		s.writeUpstreamFailure(ctx, w, rs, err)
		return
	}
	defer stream.Close()
	s.gatewayHeaders(w, rs)
	sseHeaders(w)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	outChars := 0
	write := func(ev provider.Event) error {
		var peek struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(ev.Data, &peek) == nil {
			outChars += len(peek.Delta)
		}
		if ev.Name != "" {
			if _, err := fmt.Fprintf(w, "event: %s\n", ev.Name); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", ev.Data); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	var streamErr error
	if first.Data != nil {
		streamErr = write(first)
	}
	for streamErr == nil {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			streamErr = err
			break
		}
		streamErr = write(ev)
	}
	u, reported := stream.Usage()
	s.price(rs, u, reported, promptEst, outChars)
	s.endStream(ctx, w, rs, streamErr, func(b []byte) { fmt.Fprintf(w, "event: error\ndata: %s\n\n", b) }, nil)
	s.finish(rs)
}

// ---------- /v1/models ----------

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	k, p, st, code, msg := s.authenticate(r)
	if k == nil {
		writeError(w, st, "invalid_request_error", code, msg)
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	allowed := func(name string) bool {
		t, ok := parseTarget(name)
		if !ok {
			t = targetSpec{Provider: "opengate", Model: name}
		}
		return policyAllows(k.ModelPolicy, name, t) && policyAllows(p.ModelPolicy, name, t)
	}
	seen := map[string]bool{}
	data := []model{}
	add := func(id, owner string) {
		if !seen[id] && allowed(id) {
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
	if a := s.cfg.SmartRouting.Auto; a.Enabled {
		add(a.Alias, "opengate-auto")
	}
	for _, m := range s.pricing.Table().Models() {
		prov, _, _ := strings.Cut(m, "/")
		if _, ok := s.cfg.Providers[prov]; ok {
			add(m, prov)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
