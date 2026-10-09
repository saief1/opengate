package provider

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"strings"
)

// EmbedCall is one /v1/embeddings attempt.
type EmbedCall struct {
	Raw    map[string]json.RawMessage // original body
	Inputs []string                   // parsed "input" (strings only)
	Target Target
}

// Embedder is implemented by adapters that support embeddings.
type Embedder interface {
	Embed(ctx context.Context, c *EmbedCall) (*Result, error)
}

// ErrUnsupported marks an endpoint a provider type cannot serve.
var ErrUnsupported = errors.New("not supported by this provider")

// ParseEmbeddingInput accepts a string or an array of strings. Token-array
// inputs are passed through to OpenAI-compatible upstreams (ok=false).
func ParseEmbeddingInput(raw json.RawMessage) (inputs []string, ok bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, true
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many, true
	}
	return nil, false
}

// Embed forwards to an OpenAI-compatible /embeddings endpoint.
func (o *OpenAI) Embed(ctx context.Context, c *EmbedCall) (*Result, error) {
	m := make(map[string]json.RawMessage, len(c.Raw))
	for k, v := range c.Raw {
		m[k] = v
	}
	m["model"], _ = json.Marshal(c.Target.Model)
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	resp, wd, err := o.http.do(ctx, c.Target.Provider, c.Target.Config.BaseURL+"/embeddings", o.headers(c.Target), body, c.Target.Config.Timeout, false)
	if err != nil {
		return nil, err
	}
	b, err := readAll(c.Target.Provider, resp, wd)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, &UpstreamError{Provider: c.Target.Provider, Status: 502, Message: "invalid JSON from upstream"}
	}
	res := &Result{Body: b}
	if parsed.Usage != nil {
		res.Usage, res.UsageReported = *parsed.Usage, true
	}
	return res, nil
}

// Embed translates to Gemini batchEmbedContents and back to OpenAI format.
func (g *Gemini) Embed(ctx context.Context, c *EmbedCall) (*Result, error) {
	if c.Inputs == nil {
		return nil, &UpstreamError{Provider: c.Target.Provider, Status: 400, Message: "gemini embeddings need string inputs"}
	}
	var opts struct {
		Dimensions     int    `json:"dimensions"`
		EncodingFormat string `json:"encoding_format"`
	}
	if b, err := json.Marshal(c.Raw); err == nil {
		_ = json.Unmarshal(b, &opts)
	}
	model := strings.TrimPrefix(c.Target.Model, "models/")
	type part struct {
		Text string `json:"text"`
	}
	type req struct {
		Model   string `json:"model"`
		Content struct {
			Parts []part `json:"parts"`
		} `json:"content"`
		OutputDimensionality int `json:"outputDimensionality,omitempty"`
	}
	var body struct {
		Requests []req `json:"requests"`
	}
	for _, in := range c.Inputs {
		r := req{Model: "models/" + model, OutputDimensionality: opts.Dimensions}
		r.Content.Parts = []part{{Text: in}}
		body.Requests = append(body.Requests, r)
	}
	bb, _ := json.Marshal(body)
	u := c.Target.Config.BaseURL + "/v1beta/models/" + url.PathEscape(model) + ":batchEmbedContents"
	resp, wd, err := g.http.do(ctx, c.Target.Provider, u, g.headers(c.Target), bb, c.Target.Config.Timeout, false)
	if err != nil {
		return nil, err
	}
	b, err := readAll(c.Target.Provider, resp, wd)
	if err != nil {
		return nil, err
	}
	var gr struct {
		Embeddings []struct {
			Values []float64 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(b, &gr); err != nil {
		return nil, &UpstreamError{Provider: c.Target.Provider, Status: 502, Message: "invalid JSON from upstream"}
	}
	type item struct {
		Object    string `json:"object"`
		Index     int    `json:"index"`
		Embedding any    `json:"embedding"`
	}
	out := struct {
		Object string `json:"object"`
		Data   []item `json:"data"`
		Model  string `json:"model"`
		Usage  Usage  `json:"usage"`
	}{Object: "list", Model: c.Target.Provider + "/" + model, Data: []item{}}
	for i, e := range gr.Embeddings {
		var emb any = e.Values
		if opts.EncodingFormat == "base64" {
			buf := make([]byte, 4*len(e.Values))
			for j, v := range e.Values {
				binary.LittleEndian.PutUint32(buf[4*j:], math.Float32bits(float32(v)))
			}
			emb = base64.StdEncoding.EncodeToString(buf)
		}
		out.Data = append(out.Data, item{Object: "embedding", Index: i, Embedding: emb})
	}
	// Gemini does not report token usage for embeddings: estimate (~4 chars/token).
	n := 0
	for _, in := range c.Inputs {
		n += (len(in) + 3) / 4
	}
	out.Usage = Usage{PromptTokens: n, TotalTokens: n}
	enc, _ := json.Marshal(out)
	return &Result{Body: enc, Usage: out.Usage, UsageReported: false}, nil
}
