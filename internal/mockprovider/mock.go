// Package mockprovider is a fake upstream that speaks the OpenAI, Anthropic
// and Gemini wire formats. It powers the integration tests and the local demo
// (`opengate mock-provider`) so OpenGate can be exercised without real keys.
//
// Behaviour is selected by the model name:
//
//	*fail*       always HTTP 500
//	*ratelimit*  always HTTP 429
//	*badkey*     always HTTP 401
//	*flaky*      every other request fails with 503
//	*long*       streams 200 chunks, 20ms apart (for kill-switch tests)
//	*slow*       adds 300ms before responding
//	*cached*     reports prompt-cache usage (OpenAI cached_tokens, Anthropic
//	             cache_read/cache_creation_input_tokens, Gemini cachedContentTokenCount)
//
// It also serves /v1/embeddings, Gemini :batchEmbedContents and the OpenAI
// Responses API (/v1/responses), and remembers the last request body per
// provider for assertions (LastBody).
package mockprovider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Options tune the mock.
type Options struct {
	// Jitter adds a random 0..Jitter delay to each response (demo realism).
	Jitter time.Duration
	// ChunkDelay is the delay between streamed chunks (default 5ms).
	ChunkDelay time.Duration
}

// Mock is the fake provider.
type Mock struct {
	opts  Options
	mu    sync.Mutex
	hits  map[string]int // by "provider:model"
	flaky map[string]int
	last  map[string][]byte
}

// LastBody returns the most recent request body received for a provider.
func (m *Mock) LastBody(provider string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last[provider]
}

func (m *Mock) remember(provider string, body []byte) {
	m.mu.Lock()
	m.last[provider] = append([]byte(nil), body...)
	m.mu.Unlock()
}

// New returns a mock provider handler.
func New(o Options) *Mock {
	if o.ChunkDelay == 0 {
		o.ChunkDelay = 5 * time.Millisecond
	}
	return &Mock{opts: o, hits: map[string]int{}, flaky: map[string]int{}, last: map[string][]byte{}}
}

// Hits returns how many requests reached provider:model.
func (m *Mock) Hits(provider, model string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[provider+":"+model]
}

type chatMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	body, _ := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	prov := "openai"
	switch {
	case strings.Contains(path, "openrouter"):
		prov = "openrouter"
	case strings.HasSuffix(path, "/v1/messages"):
		prov = "anthropic"
	case strings.Contains(path, ":"):
		prov = "gemini"
	}
	m.remember(prov, body)
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		m.openai(w, r, body)
	case strings.HasSuffix(path, "/embeddings"):
		m.openaiEmbeddings(w, r, body, prov)
	case strings.HasSuffix(path, "/responses"):
		m.responses(w, r, body)
	case strings.HasSuffix(path, "/v1/messages"):
		m.anthropic(w, r, body)
	case strings.Contains(path, ":batchEmbedContents"):
		m.geminiEmbeddings(w, r, body)
	case strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent"):
		m.gemini(w, r, body)
	default:
		http.Error(w, `{"error":{"message":"mock: unknown endpoint"}}`, http.StatusNotFound)
	}
}

// gate applies failure behaviours; it returns false if a response was written.
func (m *Mock) gate(w http.ResponseWriter, provider, model string, authed bool) bool {
	m.mu.Lock()
	m.hits[provider+":"+model]++
	m.mu.Unlock()
	if m.opts.Jitter > 0 {
		time.Sleep(time.Duration(rand.Int64N(int64(m.opts.Jitter))))
	}
	if strings.Contains(model, "slow") {
		time.Sleep(300 * time.Millisecond)
	}
	fail := func(code int, msg string) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"error":{"message":%q,"type":"mock_error"}}`, msg)
		return false
	}
	switch {
	case !authed || strings.Contains(model, "badkey"):
		return fail(401, "mock: invalid API key")
	case strings.Contains(model, "fail"):
		return fail(500, "mock: internal error")
	case strings.Contains(model, "ratelimit"):
		w.Header().Set("Retry-After", "0")
		return fail(429, "mock: rate limited")
	case strings.Contains(model, "flaky"):
		m.mu.Lock()
		m.flaky[provider+model]++
		n := m.flaky[provider+model]
		m.mu.Unlock()
		if n%2 == 1 {
			return fail(503, "mock: temporarily overloaded")
		}
	}
	return true
}

func reply(provider, model, prompt string) []string {
	p := strings.Fields(prompt)
	if len(p) > 12 {
		p = p[len(p)-12:]
	}
	words := []string{"Hello", "from", "the", "OpenGate", "mock", provider, "model", model + "."}
	if strings.Contains(model, "long") {
		for i := 0; i < 200; i++ {
			words = append(words, fmt.Sprintf("token%d", i))
		}
	} else {
		words = append(words, "You", "said:")
		words = append(words, p...)
	}
	return words
}

func promptTokens(s string) int {
	n := len(s)/4 + 8
	return n
}

func (m *Mock) sse(w http.ResponseWriter) (func(event string, v any), func()) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	return send, func() {
		if fl != nil {
			fl.Flush()
		}
	}
}

func (m *Mock) delay(model string) {
	d := m.opts.ChunkDelay
	if strings.Contains(model, "long") {
		d = 20 * time.Millisecond
	}
	time.Sleep(d)
}

// ---- OpenAI ----

func (m *Mock) openai(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Model         string    `json:"model"`
		Messages      []chatMsg `json:"messages"`
		Stream        bool      `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":{"message":"bad json"}}`, 400)
		return
	}
	prov := "openai"
	if strings.Contains(r.URL.Path, "openrouter") {
		prov = "openrouter"
	}
	if !m.gate(w, prov, req.Model, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")) {
		return
	}
	var all strings.Builder
	last := ""
	for _, msg := range req.Messages {
		t := textOf(msg.Content)
		all.WriteString(t)
		if msg.Role == "user" {
			last = t
		}
	}
	words := reply(prov, req.Model, last)
	pt, ct := promptTokens(all.String()), len(words)
	usage := map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
	if strings.Contains(req.Model, "cached") {
		pt += 2000
		usage = map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct, "prompt_tokens_details": map[string]int{"cached_tokens": 2000}}
	}
	id := fmt.Sprintf("chatcmpl-mock%d", rand.IntN(1e9))
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": strings.Join(words, " ")}, "finish_reason": "stop"}},
			"usage":   usage,
		})
		return
	}
	send, _ := m.sse(w)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	send("", chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	for i, wd := range words {
		if r.Context().Err() != nil {
			return
		}
		if i > 0 {
			wd = " " + wd
		}
		send("", chunk(map[string]any{"content": wd}, nil))
		m.delay(req.Model)
	}
	send("", chunk(map[string]any{}, "stop"))
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		send("", map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": req.Model, "choices": []any{},
			"usage": usage})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// ---- Anthropic ----

func (m *Mock) anthropic(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Model     string    `json:"model"`
		System    string    `json:"system"`
		Messages  []chatMsg `json:"messages"`
		MaxTokens int       `json:"max_tokens"`
		Stream    bool      `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.MaxTokens == 0 || r.Header.Get("anthropic-version") == "" {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"mock: bad anthropic request (max_tokens and anthropic-version are required)"}}`)
		return
	}
	if !m.gate(w, "anthropic", req.Model, r.Header.Get("x-api-key") != "") {
		return
	}
	all := req.System
	last := ""
	for _, msg := range req.Messages {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(msg.Content, &blocks)
		for _, b := range blocks {
			all += b.Text
			if msg.Role == "user" && b.Type == "text" {
				last = b.Text
			}
		}
	}
	words := reply("anthropic", req.Model, last)
	pt, ct := promptTokens(all), len(words)
	usage := map[string]int{"input_tokens": pt, "output_tokens": ct}
	startUsage := map[string]int{"input_tokens": pt, "output_tokens": 1}
	if strings.Contains(req.Model, "cached") {
		for _, u := range []map[string]int{usage, startUsage} {
			u["cache_read_input_tokens"], u["cache_creation_input_tokens"] = 9000, 1000
		}
	}
	id := fmt.Sprintf("msg_mock%d", rand.IntN(1e9))
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": req.Model, "stop_reason": "end_turn",
			"content": []any{map[string]any{"type": "text", "text": strings.Join(words, " ")}},
			"usage":   usage,
		})
		return
	}
	send, _ := m.sse(w)
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": req.Model, "content": []any{}, "usage": startUsage}})
	send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	send("ping", map[string]any{"type": "ping"})
	for i, wd := range words {
		if r.Context().Err() != nil {
			return
		}
		if i > 0 {
			wd = " " + wd
		}
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": wd}})
		m.delay(req.Model)
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": ct}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// ---- Gemini ----

func (m *Mock) gemini(w http.ResponseWriter, r *http.Request, body []byte) {
	path := r.URL.Path
	model := path[strings.LastIndex(path, "/")+1:]
	model, _, _ = strings.Cut(model, ":")
	stream := strings.Contains(path, ":streamGenerateContent")
	var req struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Contents) == 0 {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"code":400,"message":"mock: contents required"}}`)
		return
	}
	if !m.gate(w, "gemini", model, r.Header.Get("x-goog-api-key") != "") {
		return
	}
	all, last := "", ""
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			all += p.Text
			if c.Role == "user" {
				last = p.Text
			}
		}
	}
	words := reply("gemini", model, last)
	pt, ct := promptTokens(all), len(words)
	usage := map[string]int{"promptTokenCount": pt, "candidatesTokenCount": ct, "totalTokenCount": pt + ct}
	if strings.Contains(model, "cached") {
		usage["promptTokenCount"] += 4000
		usage["cachedContentTokenCount"] = 4000
	}
	cand := func(text, finish string) map[string]any {
		c := map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]string{"text": text}}}}
		if finish != "" {
			c["finishReason"] = finish
		}
		return c
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"candidates": []any{cand(strings.Join(words, " "), "STOP")}, "usageMetadata": usage, "modelVersion": model, "responseId": "mockresp"})
		return
	}
	send, _ := m.sse(w)
	for i, wd := range words {
		if r.Context().Err() != nil {
			return
		}
		if i > 0 {
			wd = " " + wd
		}
		ev := map[string]any{"candidates": []any{cand(wd, "")}, "modelVersion": model}
		if i == len(words)-1 {
			ev = map[string]any{"candidates": []any{cand(wd, "STOP")}, "usageMetadata": usage, "modelVersion": model}
		}
		send("", ev)
		m.delay(model)
	}
}

// ---- Embeddings ----

func vector(text string, dims int) []float64 {
	if dims <= 0 {
		dims = 8
	}
	v := make([]float64, dims)
	h := uint32(2166136261)
	for i := 0; i < len(text); i++ {
		h = (h ^ uint32(text[i])) * 16777619
	}
	for i := range v {
		h = h*1664525 + 1013904223
		v[i] = float64(h%2000)/1000 - 1
	}
	return v
}

func (m *Mock) openaiEmbeddings(w http.ResponseWriter, r *http.Request, body []byte, prov string) {
	var req struct {
		Model      string          `json:"model"`
		Input      json.RawMessage `json:"input"`
		Dimensions int             `json:"dimensions"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
		http.Error(w, `{"error":{"message":"mock: input required"}}`, 400)
		return
	}
	if !m.gate(w, prov, req.Model, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")) {
		return
	}
	var inputs []string
	var one string
	if json.Unmarshal(req.Input, &one) == nil {
		inputs = []string{one}
	} else {
		_ = json.Unmarshal(req.Input, &inputs)
	}
	data := []any{}
	tokens := 0
	for i, in := range inputs {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vector(in, req.Dimensions)})
		tokens += len(in)/4 + 1
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": req.Model,
		"usage": map[string]int{"prompt_tokens": tokens, "total_tokens": tokens}})
}

func (m *Mock) geminiEmbeddings(w http.ResponseWriter, r *http.Request, body []byte) {
	path := r.URL.Path
	model := path[strings.LastIndex(path, "/")+1:]
	model, _, _ = strings.Cut(model, ":")
	var req struct {
		Requests []struct {
			Model   string `json:"model"`
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			OutputDimensionality int `json:"outputDimensionality"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Requests) == 0 {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"code":400,"message":"mock: requests required"}}`)
		return
	}
	if !m.gate(w, "gemini", model, r.Header.Get("x-goog-api-key") != "") {
		return
	}
	embs := []any{}
	for _, rq := range req.Requests {
		text := ""
		for _, p := range rq.Content.Parts {
			text += p.Text
		}
		embs = append(embs, map[string]any{"values": vector(text, rq.OutputDimensionality)})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"embeddings": embs})
}

// ---- OpenAI Responses API ----

func (m *Mock) responses(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Model  string          `json:"model"`
		Input  json.RawMessage `json:"input"`
		Stream bool            `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
		http.Error(w, `{"error":{"message":"mock: input required"}}`, 400)
		return
	}
	if !m.gate(w, "openai", req.Model, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")) {
		return
	}
	var in string
	if json.Unmarshal(req.Input, &in) != nil {
		in = string(req.Input)
	}
	words := reply("openai", req.Model, in)
	text := strings.Join(words, " ")
	pt, ct := promptTokens(in), len(words)
	id := fmt.Sprintf("resp_mock%d", rand.IntN(1e9))
	usage := map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": pt + ct,
		"input_tokens_details": map[string]int{"cached_tokens": 0}, "output_tokens_details": map[string]int{"reasoning_tokens": 0}}
	resp := func(status string, withOutput bool) map[string]any {
		out := []any{}
		if withOutput {
			out = append(out, map[string]any{"type": "message", "id": "msg_" + id, "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
		}
		rr := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status, "model": req.Model, "output": out}
		if status == "completed" {
			rr["usage"] = usage
		}
		return rr
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp("completed", true))
		return
	}
	send, _ := m.sse(w)
	seq := 0
	ev := func(typ string, v map[string]any) {
		v["type"], v["sequence_number"] = typ, seq
		seq++
		send(typ, v)
	}
	ev("response.created", map[string]any{"response": resp("in_progress", false)})
	for i, wd := range words {
		if r.Context().Err() != nil {
			return
		}
		if i > 0 {
			wd = " " + wd
		}
		ev("response.output_text.delta", map[string]any{"item_id": "msg_" + id, "output_index": 0, "content_index": 0, "delta": wd})
		m.delay(req.Model)
	}
	ev("response.output_text.done", map[string]any{"item_id": "msg_" + id, "output_index": 0, "content_index": 0, "text": text})
	ev("response.completed", map[string]any{"response": resp("completed", true)})
}
