package provider

import (
	"context"
	"encoding/json"
	"io"
)

// ResponsesAPI is implemented by adapters that can pass through the OpenAI
// Responses API (POST /v1/responses).
type ResponsesAPI interface {
	Responses(ctx context.Context, c *Call) (*Result, error)
	ResponsesStream(ctx context.Context, c *Call) (EventStream, error)
}

// Event is one raw server-sent event.
type Event struct {
	Name string
	Data []byte
}

// EventStream yields raw SSE events (used for passthrough endpoints).
type EventStream interface {
	Next() (Event, error)
	Usage() (Usage, bool)
	Close() error
}

// responsesUsage is the Responses API usage object.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u *responsesUsage) toUsage() Usage {
	out := Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.InputTokens + u.OutputTokens}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		out.PromptTokensDetails = &PromptDetails{CachedTokens: u.InputTokensDetails.CachedTokens}
	}
	return out
}

func (o *OpenAI) responsesBody(c *Call, stream bool) ([]byte, error) {
	m := make(map[string]json.RawMessage, len(c.Raw)+1)
	for k, v := range c.Raw {
		m[k] = v
	}
	m["model"], _ = json.Marshal(c.Target.Model)
	if stream {
		m["stream"] = json.RawMessage("true")
	} else {
		delete(m, "stream")
	}
	return json.Marshal(m)
}

// Responses forwards a non-streaming Responses API call.
func (o *OpenAI) Responses(ctx context.Context, c *Call) (*Result, error) {
	body, err := o.responsesBody(c, false)
	if err != nil {
		return nil, err
	}
	resp, wd, err := o.http.do(ctx, c.Target.Provider, c.Target.Config.BaseURL+"/responses", o.headers(c.Target), body, c.Target.Config.Timeout, false)
	if err != nil {
		return nil, err
	}
	b, err := readAll(c.Target.Provider, resp, wd)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Usage *responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, &UpstreamError{Provider: c.Target.Provider, Status: 502, Message: "invalid JSON from upstream"}
	}
	res := &Result{Body: b}
	if parsed.Usage != nil {
		res.Usage, res.UsageReported = parsed.Usage.toUsage(), true
	}
	return res, nil
}

// ResponsesStream forwards a streaming Responses API call event by event.
func (o *OpenAI) ResponsesStream(ctx context.Context, c *Call) (EventStream, error) {
	body, err := o.responsesBody(c, true)
	if err != nil {
		return nil, err
	}
	resp, wd, err := o.http.do(ctx, c.Target.Provider, c.Target.Config.BaseURL+"/responses", o.headers(c.Target), body, c.Target.Config.Timeout, true)
	if err != nil {
		return nil, err
	}
	return &responsesStream{streamBase: newStreamBase(c.Target.Provider, resp, wd)}, nil
}

type responsesStream struct{ streamBase }

func (s *responsesStream) Next() (Event, error) {
	ev, err := s.next()
	if err != nil {
		return Event{}, err
	}
	var peek struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(ev.Data, &peek) == nil && peek.Response != nil && peek.Response.Usage != nil {
		s.usage, s.hasUsage = peek.Response.Usage.toUsage(), true
	}
	if peek.Type == "error" {
		return Event{}, &UpstreamError{Provider: s.provider, Status: 502, Message: extractMessage(ev.Data)}
	}
	if string(ev.Data) == "[DONE]" {
		return Event{}, io.EOF
	}
	return Event{Name: ev.Event, Data: ev.Data}, nil
}
