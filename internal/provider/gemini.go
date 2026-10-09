package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/opengate-dev/opengate/internal/secure"
)

// Gemini translates OpenAI chat requests to Google's generateContent API.
type Gemini struct {
	http *HTTP
}

type gemRequest struct {
	Contents          []gemContent   `json:"contents"`
	SystemInstruction *gemContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *gemGenConfig  `json:"generationConfig,omitempty"`
	Tools             []gemTool      `json:"tools,omitempty"`
	ToolConfig        *gemToolConfig `json:"toolConfig,omitempty"`
}

type gemContent struct {
	Role  string    `json:"role,omitempty"`
	Parts []gemPart `json:"parts"`
}

type gemPart struct {
	Text             string           `json:"text,omitempty"`
	InlineData       *gemBlob         `json:"inlineData,omitempty"`
	FunctionCall     *gemFuncCall     `json:"functionCall,omitempty"`
	FunctionResponse *gemFuncResponse `json:"functionResponse,omitempty"`
	Thought          bool             `json:"thought,omitempty"`
}

type gemBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type gemFuncCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type gemFuncResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type gemGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type gemTool struct {
	FunctionDeclarations []gemFuncDecl `json:"functionDeclarations"`
}

type gemFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type gemToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type gemResponse struct {
	Candidates []struct {
		Content      gemContent `json:"content"`
		FinishReason string     `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
	ResponseID   string `json:"responseId"`
}

// cleanSchema removes JSON-schema keywords Gemini rejects.
func cleanSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for _, k := range []string{"additionalProperties", "$schema", "strict", "$id", "examples"} {
				delete(t, k)
			}
			for k, val := range t {
				t[k] = walk(val)
			}
			return t
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		}
		return x
	}
	b, _ := json.Marshal(walk(v))
	return b
}

// ToGeminiRequest converts an OpenAI chat request into a generateContent request.
func ToGeminiRequest(r *ChatRequest) (*gemRequest, error) {
	out := &gemRequest{}
	var system []gemPart
	toolNames := map[string]string{} // tool_call_id -> function name
	add := func(role string, parts []gemPart) {
		if len(parts) == 0 {
			return
		}
		if n := len(out.Contents); n > 0 && out.Contents[n-1].Role == role {
			out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, parts...)
			return
		}
		out.Contents = append(out.Contents, gemContent{Role: role, Parts: parts})
	}
	for i, m := range r.Messages {
		switch m.Role {
		case "system", "developer":
			if t := m.Text(); t != "" {
				system = append(system, gemPart{Text: t})
			}
		case "user", "assistant":
			parts, err := m.Parts()
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			var gp []gemPart
			for _, p := range parts {
				switch p.Type {
				case "text":
					if p.Text != "" {
						gp = append(gp, gemPart{Text: p.Text})
					}
				case "image_url":
					if p.ImageURL == nil {
						continue
					}
					mime, data, ok := parseDataURL(p.ImageURL.URL)
					if !ok {
						return nil, fmt.Errorf("messages[%d]: gemini adapter only supports data: image URLs", i)
					}
					gp = append(gp, gemPart{InlineData: &gemBlob{MimeType: mime, Data: data}})
				default:
					return nil, fmt.Errorf("messages[%d]: content type %q is not supported for gemini", i, p.Type)
				}
			}
			role := "user"
			if m.Role == "assistant" {
				role = "model"
				for _, tc := range m.ToolCalls {
					toolNames[tc.ID] = tc.Function.Name
					args := json.RawMessage(strings.TrimSpace(tc.Function.Arguments))
					if len(args) == 0 || !json.Valid(args) {
						args = json.RawMessage("{}")
					}
					gp = append(gp, gemPart{FunctionCall: &gemFuncCall{Name: tc.Function.Name, Args: args}})
				}
			}
			add(role, gp)
		case "tool":
			name := toolNames[m.ToolCallID]
			if name == "" {
				name = m.Name
			}
			text := m.Text()
			resp := json.RawMessage(text)
			if !strings.HasPrefix(strings.TrimSpace(text), "{") || !json.Valid(resp) {
				resp, _ = json.Marshal(map[string]string{"result": text})
			}
			add("user", []gemPart{{FunctionResponse: &gemFuncResponse{Name: name, Response: resp}}})
		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	if len(out.Contents) == 0 {
		return nil, fmt.Errorf("at least one user message is required")
	}
	if len(system) > 0 {
		out.SystemInstruction = &gemContent{Parts: system}
	}
	gc := &gemGenConfig{Temperature: r.Temperature, TopP: r.TopP, MaxOutputTokens: r.MaxOutputTokens(), StopSequences: r.StopSequences()}
	if gc.Temperature != nil || gc.TopP != nil || gc.MaxOutputTokens > 0 || len(gc.StopSequences) > 0 {
		out.GenerationConfig = gc
	}
	if len(r.Tools) > 0 {
		var decls []gemFuncDecl
		for _, t := range r.Tools {
			decls = append(decls, gemFuncDecl{Name: t.Function.Name, Description: t.Function.Description, Parameters: cleanSchema(t.Function.Parameters)})
		}
		out.Tools = []gemTool{{FunctionDeclarations: decls}}
		if len(r.ToolChoice) > 0 {
			tc := &gemToolConfig{}
			var s string
			if json.Unmarshal(r.ToolChoice, &s) == nil {
				tc.FunctionCallingConfig.Mode = map[string]string{"auto": "AUTO", "none": "NONE", "required": "ANY"}[s]
			} else {
				var obj struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				if json.Unmarshal(r.ToolChoice, &obj) == nil && obj.Function.Name != "" {
					tc.FunctionCallingConfig.Mode = "ANY"
					tc.FunctionCallingConfig.AllowedFunctionNames = []string{obj.Function.Name}
				}
			}
			if tc.FunctionCallingConfig.Mode != "" {
				out.ToolConfig = tc
			}
		}
	}
	return out, nil
}

func mapGeminiFinish(s string, hasCalls bool) string {
	if hasCalls {
		return "tool_calls"
	}
	switch s {
	case "":
		return ""
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		return "stop"
	}
}

// geminiParts extracts text and tool calls from a candidate's parts.
func geminiParts(parts []gemPart) (string, []ToolCall) {
	var text strings.Builder
	var calls []ToolCall
	for _, p := range parts {
		if p.Thought {
			continue
		}
		if p.Text != "" {
			text.WriteString(p.Text)
		}
		if p.FunctionCall != nil {
			args := string(p.FunctionCall.Args)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, ToolCall{ID: secure.RandomID("call_"), Type: "function", Function: FunctionCall{Name: p.FunctionCall.Name, Arguments: args}})
		}
	}
	return text.String(), calls
}

func (g *Gemini) url(t Target, stream bool) string {
	m := url.PathEscape(t.Model)
	if stream {
		return t.Config.BaseURL + "/v1beta/models/" + m + ":streamGenerateContent?alt=sse"
	}
	return t.Config.BaseURL + "/v1beta/models/" + m + ":generateContent"
}

func (g *Gemini) headers(t Target) map[string]string {
	h := map[string]string{}
	for k, v := range t.Config.Headers {
		h[k] = v
	}
	if t.APIKey != "" {
		h["x-goog-api-key"] = t.APIKey
	}
	return h
}

func (g *Gemini) body(c *Call) ([]byte, error) {
	req, err := ToGeminiRequest(c.Req)
	if err != nil {
		return nil, &UpstreamError{Provider: c.Target.Provider, Status: 400, Message: err.Error()}
	}
	return json.Marshal(req)
}

// Chat performs a non-streaming completion.
func (g *Gemini) Chat(ctx context.Context, c *Call) (*Result, error) {
	body, err := g.body(c)
	if err != nil {
		return nil, err
	}
	resp, wd, err := g.http.do(ctx, c.Target.Provider, g.url(c.Target, false), g.headers(c.Target), body, c.Target.Config.Timeout, false)
	if err != nil {
		return nil, err
	}
	b, err := readAll(c.Target.Provider, resp, wd)
	if err != nil {
		return nil, err
	}
	var gr gemResponse
	if err := json.Unmarshal(b, &gr); err != nil {
		return nil, &UpstreamError{Provider: c.Target.Provider, Status: 502, Message: "invalid JSON from upstream"}
	}
	var text string
	var calls []ToolCall
	var finish string
	if len(gr.Candidates) > 0 {
		text, calls = geminiParts(gr.Candidates[0].Content.Parts)
		finish = mapGeminiFinish(gr.Candidates[0].FinishReason, len(calls) > 0)
	}
	msg := &RespMessage{Role: "assistant", ToolCalls: calls}
	if text != "" || len(calls) == 0 {
		msg.Content = strPtr(text)
	}
	res := &Result{}
	out := ChatResponse{ID: "chatcmpl-" + gr.ResponseID, Object: "chat.completion", Created: time.Now().Unix(),
		Model: c.Target.Provider + "/" + c.Target.Model, Choices: []Choice{{Index: 0, Message: msg, FinishReason: &finish}}}
	if gr.UsageMetadata != nil {
		res.Usage = Usage{PromptTokens: gr.UsageMetadata.PromptTokenCount, CompletionTokens: gr.UsageMetadata.CandidatesTokenCount + gr.UsageMetadata.ThoughtsTokenCount}
		res.Usage.TotalTokens = res.Usage.PromptTokens + res.Usage.CompletionTokens
		res.UsageReported = true
		u := res.Usage
		out.Usage = &u
	}
	res.Body, _ = json.Marshal(out)
	return res, nil
}

// ChatStream performs a streaming completion.
func (g *Gemini) ChatStream(ctx context.Context, c *Call) (Stream, error) {
	body, err := g.body(c)
	if err != nil {
		return nil, err
	}
	resp, wd, err := g.http.do(ctx, c.Target.Provider, g.url(c.Target, true), g.headers(c.Target), body, c.Target.Config.Timeout, true)
	if err != nil {
		return nil, err
	}
	return &geminiStream{streamBase: newStreamBase(c.Target.Provider, resp, wd), includeUsage: c.IncludeUsage,
		model: c.Target.Provider + "/" + c.Target.Model, created: time.Now().Unix(), id: secure.RandomID("chatcmpl-")}, nil
}

type geminiStream struct {
	streamBase
	includeUsage bool
	model, id    string
	created      int64
	started      bool
	nextTool     int
	finished     bool
	pending      [][]byte
	eof          bool
}

func (s *geminiStream) emit(d *Delta, finish *string) {
	c := ChatResponse{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model, Choices: []Choice{{Delta: d, FinishReason: finish}}}
	b, _ := json.Marshal(c)
	s.pending = append(s.pending, b)
}

func (s *geminiStream) Next() ([]byte, error) {
	for {
		if len(s.pending) > 0 {
			c := s.pending[0]
			s.pending = s.pending[1:]
			return c, nil
		}
		if s.eof {
			return nil, io.EOF
		}
		ev, err := s.next()
		if err == io.EOF {
			s.eof = true
			if s.includeUsage && s.hasUsage {
				u := s.usage
				u.TotalTokens = u.PromptTokens + u.CompletionTokens
				c := ChatResponse{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model, Choices: []Choice{}, Usage: &u}
				b, _ := json.Marshal(c)
				s.pending = append(s.pending, b)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		var gr gemResponse
		if json.Unmarshal(ev.Data, &gr) != nil {
			continue
		}
		if gr.UsageMetadata != nil {
			s.usage = Usage{PromptTokens: gr.UsageMetadata.PromptTokenCount, CompletionTokens: gr.UsageMetadata.CandidatesTokenCount + gr.UsageMetadata.ThoughtsTokenCount}
			s.hasUsage = true
		}
		if !s.started {
			s.started = true
			s.emit(&Delta{Role: "assistant", Content: strPtr("")}, nil)
		}
		if len(gr.Candidates) == 0 {
			continue
		}
		cand := gr.Candidates[0]
		text, calls := geminiParts(cand.Content.Parts)
		if text != "" {
			s.emit(&Delta{Content: strPtr(text)}, nil)
		}
		for i := range calls {
			idx := s.nextTool
			s.nextTool++
			calls[i].Index = &idx
		}
		if len(calls) > 0 {
			s.emit(&Delta{ToolCalls: calls}, nil)
		}
		if cand.FinishReason != "" && !s.finished {
			s.finished = true
			fr := mapGeminiFinish(cand.FinishReason, s.nextTool > 0)
			s.emit(&Delta{}, &fr)
		}
	}
}
