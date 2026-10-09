// Package provider implements upstream adapters. Every adapter speaks the
// OpenAI chat-completions shape on the OpenGate side and translates to the
// provider's native API where needed.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/opengate-dev/opengate/internal/config"
)

// ChatRequest is the subset of the OpenAI chat request OpenGate understands.
// For OpenAI-compatible upstreams the original JSON is forwarded verbatim
// (see Call.Raw), so unknown fields are preserved.
type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	User                string          `json:"user,omitempty"`
}

// StreamOptions mirrors OpenAI's stream_options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Message is an OpenAI chat message.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// ContentPart is one element of an array-style message content.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is an image reference (http(s) or data: URL).
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// Parts normalizes string / array / null content into parts.
func (m Message) Parts() ([]ContentPart, error) {
	c := strings.TrimSpace(string(m.Content))
	if c == "" || c == "null" {
		return nil, nil
	}
	if c[0] == '"' {
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			return nil, err
		}
		return []ContentPart{{Type: "text", Text: s}}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil, fmt.Errorf("invalid message content: %w", err)
	}
	return parts, nil
}

// Text concatenates all text parts.
func (m Message) Text() string {
	parts, _ := m.Parts()
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// StopSequences decodes the polymorphic "stop" field.
func (r *ChatRequest) StopSequences() []string {
	if len(r.Stop) == 0 || string(r.Stop) == "null" {
		return nil
	}
	var one string
	if json.Unmarshal(r.Stop, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(r.Stop, &many)
	return many
}

// MaxOutputTokens returns max_completion_tokens or max_tokens (0 if unset).
func (r *ChatRequest) MaxOutputTokens() int {
	if r.MaxCompletionTokens != nil {
		return *r.MaxCompletionTokens
	}
	if r.MaxTokens != nil {
		return *r.MaxTokens
	}
	return 0
}

// Tool is an OpenAI function tool.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a callable function.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is an assistant tool invocation.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall carries a function name and JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// Usage is OpenAI-style token usage.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is an OpenAI chat.completion or chat.completion.chunk.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice is one completion choice.
type Choice struct {
	Index        int          `json:"index"`
	Message      *RespMessage `json:"message,omitempty"`
	Delta        *Delta       `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

// RespMessage is a non-streaming assistant message.
type RespMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Delta is a streaming message fragment.
type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Target is one concrete upstream to try.
type Target struct {
	Provider string // OpenGate provider name, e.g. "anthropic"
	Model    string // upstream model id, e.g. "claude-sonnet-4-5"
	Config   config.ProviderConfig
	APIKey   string
}

// Call bundles everything an adapter needs for one attempt.
type Call struct {
	Req              *ChatRequest
	Raw              map[string]json.RawMessage // original request body fields
	Target           Target
	IncludeUsage     bool // client asked for stream_options.include_usage
	DefaultMaxTokens int
}

// Result is a non-streaming response already in OpenAI format.
type Result struct {
	Body          []byte
	Usage         Usage
	UsageReported bool
}

// Stream yields OpenAI chat.completion.chunk JSON objects.
type Stream interface {
	// Next returns the next chunk, or io.EOF when the stream completed.
	Next() ([]byte, error)
	// Usage returns the usage reported by the provider so far.
	Usage() (Usage, bool)
	Close() error
}

// Adapter talks to one provider type.
type Adapter interface {
	Chat(ctx context.Context, c *Call) (*Result, error)
	ChatStream(ctx context.Context, c *Call) (Stream, error)
}

// ForType returns the adapter for a provider type.
func ForType(t string, h *HTTP) (Adapter, error) {
	switch t {
	case config.TypeOpenAI:
		return &OpenAI{http: h}, nil
	case config.TypeOpenRouter:
		return &OpenAI{http: h, openRouter: true}, nil
	case config.TypeAnthropic:
		return &Anthropic{http: h}, nil
	case config.TypeGemini:
		return &Gemini{http: h}, nil
	}
	return nil, errors.New("unknown provider type " + t)
}

func strPtr(s string) *string { return &s }
