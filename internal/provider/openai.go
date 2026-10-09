package provider

import (
	"context"
	"encoding/json"
	"io"
)

// OpenAI adapts OpenAI and OpenAI-compatible APIs (incl. OpenRouter). The
// client's JSON body is forwarded with only "model" (and stream usage
// options) rewritten, so every OpenAI feature passes through untouched.
type OpenAI struct {
	http       *HTTP
	openRouter bool
}

func (o *OpenAI) headers(t Target) map[string]string {
	h := map[string]string{}
	if o.openRouter {
		h["HTTP-Referer"] = "https://github.com/opengate-dev/opengate"
		h["X-Title"] = "OpenGate"
	}
	for k, v := range t.Config.Headers {
		h[k] = v
	}
	if t.APIKey != "" {
		h["Authorization"] = "Bearer " + t.APIKey
	}
	return h
}

func (o *OpenAI) body(c *Call, stream bool) ([]byte, error) {
	m := make(map[string]json.RawMessage, len(c.Raw)+2)
	for k, v := range c.Raw {
		m[k] = v
	}
	model, _ := json.Marshal(c.Target.Model)
	m["model"] = model
	if stream {
		m["stream"] = json.RawMessage("true")
		// Always ask for usage so budgets are exact; the extra chunk is
		// filtered out again if the client did not request it.
		m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	} else {
		delete(m, "stream")
		delete(m, "stream_options")
	}
	return json.Marshal(m)
}

// Chat performs a non-streaming completion.
func (o *OpenAI) Chat(ctx context.Context, c *Call) (*Result, error) {
	body, err := o.body(c, false)
	if err != nil {
		return nil, err
	}
	resp, wd, err := o.http.do(ctx, c.Target.Provider, c.Target.Config.BaseURL+"/chat/completions", o.headers(c.Target), body, c.Target.Config.Timeout, false)
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

// ChatStream performs a streaming completion.
func (o *OpenAI) ChatStream(ctx context.Context, c *Call) (Stream, error) {
	body, err := o.body(c, true)
	if err != nil {
		return nil, err
	}
	resp, wd, err := o.http.do(ctx, c.Target.Provider, c.Target.Config.BaseURL+"/chat/completions", o.headers(c.Target), body, c.Target.Config.Timeout, true)
	if err != nil {
		return nil, err
	}
	return &openAIStream{streamBase: newStreamBase(c.Target.Provider, resp, wd), includeUsage: c.IncludeUsage}, nil
}

type openAIStream struct {
	streamBase
	includeUsage bool
}

func (s *openAIStream) Next() ([]byte, error) {
	for {
		ev, err := s.next()
		if err != nil {
			return nil, err
		}
		if string(ev.Data) == "[DONE]" {
			return nil, io.EOF
		}
		var peek struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   *Usage            `json:"usage"`
			Error   json.RawMessage   `json:"error"`
		}
		if err := json.Unmarshal(ev.Data, &peek); err != nil {
			continue // ignore malformed keep-alives
		}
		if len(peek.Error) > 0 && string(peek.Error) != "null" {
			return nil, &UpstreamError{Provider: s.provider, Status: 502, Message: extractMessage(ev.Data)}
		}
		if peek.Usage != nil {
			s.usage, s.hasUsage = *peek.Usage, true
			if len(peek.Choices) == 0 && !s.includeUsage {
				continue
			}
		}
		return ev.Data, nil
	}
}
