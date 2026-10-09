package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// AnthropicVersion is the Messages API version header OpenGate sends.
const AnthropicVersion = "2023-06-01"

// Anthropic translates OpenAI chat requests to the Anthropic Messages API.
type Anthropic struct {
	http *HTTP
}

type anthRequest struct {
	Model         string          `json:"model"`
	System        string          `json:"system,omitempty"`
	Messages      []anthMessage   `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []anthTool      `json:"tools,omitempty"`
	ToolChoice    *anthToolChoice `json:"tool_choice,omitempty"`
	Metadata      *anthMetadata   `json:"metadata,omitempty"`
}

type anthMetadata struct {
	UserID string `json:"user_id"`
}

type anthMessage struct {
	Role    string      `json:"role"`
	Content []anthBlock `json:"content"`
}

type anthBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	Source    *anthImageSource `json:"source,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     json.RawMessage  `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   string           `json:"content,omitempty"`
}

type anthImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

type anthResponse struct {
	ID         string      `json:"id"`
	Model      string      `json:"model"`
	Content    []anthBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      anthUsage   `json:"usage"`
}

type anthUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u anthUsage) prompt() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// parseDataURL splits "data:<mime>;base64,<data>".
func parseDataURL(u string) (mime, data string, ok bool) {
	rest, found := strings.CutPrefix(u, "data:")
	if !found {
		return "", "", false
	}
	meta, data, found := strings.Cut(rest, ",")
	if !found || !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(meta, ";base64"), data, true
}

// ToAnthropicRequest converts an OpenAI chat request into a Messages API request.
func ToAnthropicRequest(r *ChatRequest, model string, defaultMaxTokens int) (*anthRequest, error) {
	out := &anthRequest{Model: model, Temperature: r.Temperature, TopP: r.TopP, StopSequences: r.StopSequences(), Stream: r.Stream}
	out.MaxTokens = r.MaxOutputTokens()
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultMaxTokens
	}
	if r.User != "" {
		out.Metadata = &anthMetadata{UserID: r.User}
	}
	var system []string
	appendBlocks := func(role string, blocks []anthBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
			return
		}
		out.Messages = append(out.Messages, anthMessage{Role: role, Content: blocks})
	}
	for i, m := range r.Messages {
		switch m.Role {
		case "system", "developer":
			if t := m.Text(); t != "" {
				system = append(system, t)
			}
		case "user", "assistant":
			parts, err := m.Parts()
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			var blocks []anthBlock
			for _, p := range parts {
				switch p.Type {
				case "text":
					if p.Text != "" {
						blocks = append(blocks, anthBlock{Type: "text", Text: p.Text})
					}
				case "image_url":
					if p.ImageURL == nil {
						continue
					}
					if mime, data, ok := parseDataURL(p.ImageURL.URL); ok {
						blocks = append(blocks, anthBlock{Type: "image", Source: &anthImageSource{Type: "base64", MediaType: mime, Data: data}})
					} else {
						blocks = append(blocks, anthBlock{Type: "image", Source: &anthImageSource{Type: "url", URL: p.ImageURL.URL}})
					}
				default:
					return nil, fmt.Errorf("messages[%d]: content type %q is not supported for anthropic", i, p.Type)
				}
			}
			if m.Role == "assistant" {
				for _, tc := range m.ToolCalls {
					input := json.RawMessage(strings.TrimSpace(tc.Function.Arguments))
					if len(input) == 0 || !json.Valid(input) {
						input = json.RawMessage("{}")
					}
					blocks = append(blocks, anthBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
				}
			}
			appendBlocks(m.Role, blocks)
		case "tool":
			appendBlocks("user", []anthBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Text()}})
		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	if len(out.Messages) == 0 {
		return nil, errors.New("at least one user message is required")
	}
	out.System = strings.Join(system, "\n\n")
	for _, t := range r.Tools {
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	if len(r.ToolChoice) > 0 && len(out.Tools) > 0 {
		var s string
		if json.Unmarshal(r.ToolChoice, &s) == nil {
			switch s {
			case "auto":
				out.ToolChoice = &anthToolChoice{Type: "auto"}
			case "required":
				out.ToolChoice = &anthToolChoice{Type: "any"}
			case "none":
				out.ToolChoice = &anthToolChoice{Type: "none"}
			}
		} else {
			var obj struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(r.ToolChoice, &obj) == nil && obj.Function.Name != "" {
				out.ToolChoice = &anthToolChoice{Type: "tool", Name: obj.Function.Name}
			}
		}
	}
	return out, nil
}

// MapAnthropicStopReason converts Anthropic stop reasons to OpenAI finish reasons.
func MapAnthropicStopReason(s string) string {
	switch s {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	default: // end_turn, stop_sequence, pause_turn
		return "stop"
	}
}

// FromAnthropicResponse converts a Messages API response into an OpenAI chat.completion.
func FromAnthropicResponse(a *anthResponse, provider string) *ChatResponse {
	var text strings.Builder
	var calls []ToolCall
	for _, b := range a.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, ToolCall{ID: b.ID, Type: "function", Function: FunctionCall{Name: b.Name, Arguments: args}})
		}
	}
	msg := &RespMessage{Role: "assistant", ToolCalls: calls}
	if text.Len() > 0 || len(calls) == 0 {
		msg.Content = strPtr(text.String())
	}
	fr := MapAnthropicStopReason(a.StopReason)
	u := Usage{PromptTokens: a.Usage.prompt(), CompletionTokens: a.Usage.OutputTokens}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return &ChatResponse{
		ID: a.ID, Object: "chat.completion", Created: time.Now().Unix(), Model: provider + "/" + a.Model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: &fr}},
		Usage:   &u,
	}
}

func (a *Anthropic) headers(t Target) map[string]string {
	h := map[string]string{"anthropic-version": AnthropicVersion}
	for k, v := range t.Config.Headers {
		h[k] = v
	}
	if t.APIKey != "" {
		h["x-api-key"] = t.APIKey
	}
	return h
}

func (a *Anthropic) request(c *Call, stream bool) ([]byte, error) {
	req, err := ToAnthropicRequest(c.Req, c.Target.Model, c.DefaultMaxTokens)
	if err != nil {
		return nil, &UpstreamError{Provider: c.Target.Provider, Status: 400, Message: err.Error()}
	}
	req.Stream = stream
	return json.Marshal(req)
}

// Chat performs a non-streaming completion.
func (a *Anthropic) Chat(ctx context.Context, c *Call) (*Result, error) {
	body, err := a.request(c, false)
	if err != nil {
		return nil, err
	}
	resp, wd, err := a.http.do(ctx, c.Target.Provider, c.Target.Config.BaseURL+"/v1/messages", a.headers(c.Target), body, c.Target.Config.Timeout, false)
	if err != nil {
		return nil, err
	}
	b, err := readAll(c.Target.Provider, resp, wd)
	if err != nil {
		return nil, err
	}
	var ar anthResponse
	if err := json.Unmarshal(b, &ar); err != nil {
		return nil, &UpstreamError{Provider: c.Target.Provider, Status: 502, Message: "invalid JSON from upstream"}
	}
	out := FromAnthropicResponse(&ar, c.Target.Provider)
	enc, _ := json.Marshal(out)
	return &Result{Body: enc, Usage: *out.Usage, UsageReported: true}, nil
}

// ChatStream performs a streaming completion, translating Anthropic events to OpenAI chunks.
func (a *Anthropic) ChatStream(ctx context.Context, c *Call) (Stream, error) {
	body, err := a.request(c, true)
	if err != nil {
		return nil, err
	}
	resp, wd, err := a.http.do(ctx, c.Target.Provider, c.Target.Config.BaseURL+"/v1/messages", a.headers(c.Target), body, c.Target.Config.Timeout, true)
	if err != nil {
		return nil, err
	}
	s := newAnthropicStream(c.Target.Provider, c.IncludeUsage)
	s.streamBase = newStreamBase(c.Target.Provider, resp, wd)
	return s, nil
}

// anthropicStream converts Anthropic SSE events into OpenAI chunks.
type anthropicStream struct {
	streamBase
	providerName string
	includeUsage bool
	id, model    string
	created      int64
	toolIdx      map[int]int // anthropic content block index -> openai tool_call index
	nextTool     int
	pending      [][]byte
	done         bool
}

func newAnthropicStream(provider string, includeUsage bool) *anthropicStream {
	return &anthropicStream{providerName: provider, includeUsage: includeUsage, created: time.Now().Unix(), toolIdx: map[int]int{}}
}

func (s *anthropicStream) chunk(delta *Delta, finish *string) []byte {
	c := ChatResponse{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []Choice{{Index: 0, Delta: delta, FinishReason: finish}}}
	b, _ := json.Marshal(c)
	return b
}

func (s *anthropicStream) Next() ([]byte, error) {
	for {
		if len(s.pending) > 0 {
			c := s.pending[0]
			s.pending = s.pending[1:]
			return c, nil
		}
		if s.done {
			return nil, io.EOF
		}
		ev, err := s.next()
		if err != nil {
			return nil, err
		}
		if err := s.handle(ev.Data); err != nil {
			return nil, err
		}
	}
}

// handle translates one Anthropic event payload, queueing OpenAI chunks.
func (s *anthropicStream) handle(data []byte) error {
	var e struct {
		Type    string        `json:"type"`
		Index   int           `json:"index"`
		Message *anthResponse `json:"message"`
		Block   *anthBlock    `json:"content_block"`
		Delta   *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *anthUsage `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return nil
	}
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			s.id, s.model = e.Message.ID, s.providerName+"/"+e.Message.Model
			s.usage.PromptTokens = e.Message.Usage.prompt()
			s.usage.CompletionTokens = e.Message.Usage.OutputTokens
			s.hasUsage = true
		}
		s.pending = append(s.pending, s.chunk(&Delta{Role: "assistant", Content: strPtr("")}, nil))
	case "content_block_start":
		if e.Block != nil && e.Block.Type == "tool_use" {
			idx := s.nextTool
			s.nextTool++
			s.toolIdx[e.Index] = idx
			s.pending = append(s.pending, s.chunk(&Delta{ToolCalls: []ToolCall{{Index: &idx, ID: e.Block.ID, Type: "function",
				Function: FunctionCall{Name: e.Block.Name, Arguments: ""}}}}, nil))
		} else if e.Block != nil && e.Block.Type == "text" && e.Block.Text != "" {
			s.pending = append(s.pending, s.chunk(&Delta{Content: strPtr(e.Block.Text)}, nil))
		}
	case "content_block_delta":
		if e.Delta == nil {
			return nil
		}
		switch e.Delta.Type {
		case "text_delta":
			s.pending = append(s.pending, s.chunk(&Delta{Content: strPtr(e.Delta.Text)}, nil))
		case "input_json_delta":
			if idx, ok := s.toolIdx[e.Index]; ok {
				s.pending = append(s.pending, s.chunk(&Delta{ToolCalls: []ToolCall{{Index: &idx, Function: FunctionCall{Arguments: e.Delta.PartialJSON}}}}, nil))
			}
		}
	case "message_delta":
		if e.Usage != nil {
			s.usage.CompletionTokens = e.Usage.OutputTokens
			if p := e.Usage.prompt(); p > 0 {
				s.usage.PromptTokens = p
			}
			s.hasUsage = true
		}
		if e.Delta != nil && e.Delta.StopReason != "" {
			fr := MapAnthropicStopReason(e.Delta.StopReason)
			s.pending = append(s.pending, s.chunk(&Delta{}, &fr))
		}
	case "message_stop":
		s.done = true
		if s.includeUsage {
			u := s.usage
			u.TotalTokens = u.PromptTokens + u.CompletionTokens
			c := ChatResponse{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model, Choices: []Choice{}, Usage: &u}
			b, _ := json.Marshal(c)
			s.pending = append(s.pending, b)
		}
	case "error":
		msg := "upstream stream error"
		if e.Error != nil {
			msg = e.Error.Type + ": " + e.Error.Message
		}
		return &UpstreamError{Provider: s.providerName, Status: 502, Message: msg}
	}
	return nil
}
