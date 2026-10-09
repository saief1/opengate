package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func mustReq(t *testing.T, s string) *ChatRequest {
	t.Helper()
	var r ChatRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestToAnthropicRequest(t *testing.T) {
	r := mustReq(t, `{
	  "model":"anthropic/claude-sonnet-4-5",
	  "max_tokens": 300, "temperature": 0.2, "stop": "END", "user": "u-1",
	  "messages":[
	    {"role":"system","content":"You are terse."},
	    {"role":"developer","content":[{"type":"text","text":"Answer in English."}]},
	    {"role":"user","content":"What's the weather in Paris?"},
	    {"role":"user","content":[{"type":"text","text":"And show this:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
	    {"role":"tool","tool_call_id":"toolu_1","content":"18C sunny"},
	    {"role":"user","content":"thanks"}
	  ],
	  "tools":[{"type":"function","function":{"name":"get_weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
	  "tool_choice":"required"
	}`)
	a, err := ToAnthropicRequest(r, "claude-sonnet-4-5", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if a.Model != "claude-sonnet-4-5" || a.MaxTokens != 300 || *a.Temperature != 0.2 {
		t.Fatalf("basic fields: %+v", a)
	}
	if a.System != "You are terse.\n\nAnswer in English." {
		t.Fatalf("system: %q", a.System)
	}
	if len(a.StopSequences) != 1 || a.StopSequences[0] != "END" || a.Metadata.UserID != "u-1" {
		t.Fatal("stop/user")
	}
	// user+user merged; assistant; tool_result + user merged into one user turn.
	if len(a.Messages) != 3 {
		b, _ := json.MarshalIndent(a.Messages, "", " ")
		t.Fatalf("want 3 alternating messages, got %d: %s", len(a.Messages), b)
	}
	u := a.Messages[0]
	if u.Role != "user" || len(u.Content) != 3 || u.Content[2].Type != "image" || u.Content[2].Source.Type != "base64" || u.Content[2].Source.MediaType != "image/png" {
		t.Fatalf("user turn: %+v", u)
	}
	as := a.Messages[1]
	if as.Role != "assistant" || len(as.Content) != 1 || as.Content[0].Type != "tool_use" || as.Content[0].ID != "toolu_1" || string(as.Content[0].Input) != `{"city":"Paris"}` {
		t.Fatalf("assistant turn: %+v", as)
	}
	tr := a.Messages[2]
	if tr.Role != "user" || tr.Content[0].Type != "tool_result" || tr.Content[0].ToolUseID != "toolu_1" || tr.Content[0].Content != "18C sunny" || tr.Content[1].Text != "thanks" {
		t.Fatalf("tool result turn: %+v", tr)
	}
	if len(a.Tools) != 1 || a.Tools[0].Name != "get_weather" || a.ToolChoice.Type != "any" {
		t.Fatalf("tools: %+v %+v", a.Tools, a.ToolChoice)
	}
	// Default max tokens applies when unset.
	r2 := mustReq(t, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	a2, _ := ToAnthropicRequest(r2, "m", 1234)
	if a2.MaxTokens != 1234 || a2.System != "" || a2.ToolChoice != nil {
		t.Fatalf("defaults: %+v", a2)
	}
	if _, err := ToAnthropicRequest(mustReq(t, `{"model":"x","messages":[{"role":"system","content":"only system"}]}`), "m", 1); err == nil {
		t.Fatal("expected error for no user messages")
	}
}

func TestFromAnthropicResponse(t *testing.T) {
	var ar anthResponse
	json.Unmarshal([]byte(`{"id":"msg_1","model":"claude-sonnet-4-5","stop_reason":"tool_use",
	  "content":[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{"city":"Oslo"}}],
	  "usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":50}}`), &ar)
	out := FromAnthropicResponse(&ar, "anthropic")
	c := out.Choices[0]
	if *c.FinishReason != "tool_calls" || *c.Message.Content != "Let me check." || c.Message.ToolCalls[0].Function.Arguments != `{"city":"Oslo"}` {
		t.Fatalf("bad translation: %+v", c.Message)
	}
	if out.Usage.PromptTokens != 150 || out.Usage.CompletionTokens != 20 || out.Usage.TotalTokens != 170 || out.Model != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("usage/model: %+v %s", out.Usage, out.Model)
	}
	for in, want := range map[string]string{"end_turn": "stop", "max_tokens": "length", "stop_sequence": "stop", "tool_use": "tool_calls"} {
		if got := MapAnthropicStopReason(in); got != want {
			t.Errorf("%s -> %s want %s", in, got, want)
		}
	}
}

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_s","model":"claude-haiku-4-5","content":[],"usage":{"input_tokens":25,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_x","name":"lookup","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"go\"}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func fakeStreamBase(body string) streamBase {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	return newStreamBase("anthropic", resp, newWatchdog(time.Minute, func() {}))
}

func TestAnthropicStreamTranslation(t *testing.T) {
	s := newAnthropicStream("anthropic", true)
	s.streamBase = fakeStreamBase(anthropicSSE)
	var text, args, finish, toolID string
	var usage *Usage
	for {
		b, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var c ChatResponse
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		if c.Object != "chat.completion.chunk" || c.Model != "anthropic/claude-haiku-4-5" {
			t.Fatalf("chunk header: %s", b)
		}
		if c.Usage != nil {
			usage = c.Usage
			if len(c.Choices) != 0 {
				t.Fatal("usage chunk must have no choices")
			}
			continue
		}
		d := c.Choices[0].Delta
		if d.Content != nil {
			text += *d.Content
		}
		for _, tc := range d.ToolCalls {
			if tc.ID != "" {
				toolID = tc.ID
			}
			args += tc.Function.Arguments
		}
		if c.Choices[0].FinishReason != nil {
			finish = *c.Choices[0].FinishReason
		}
	}
	if text != "Hello world" || args != `{"q":"go"}` || toolID != "toolu_x" || finish != "tool_calls" {
		t.Fatalf("text=%q args=%q tool=%q finish=%q", text, args, toolID, finish)
	}
	if usage == nil || usage.PromptTokens != 25 || usage.CompletionTokens != 42 || usage.TotalTokens != 67 {
		t.Fatalf("usage: %+v", usage)
	}
	if u, ok := s.Usage(); !ok || u.CompletionTokens != 42 {
		t.Fatal("stream Usage()")
	}
}

func TestAnthropicStreamNoUsageChunkUnlessAsked(t *testing.T) {
	s := newAnthropicStream("anthropic", false)
	s.streamBase = fakeStreamBase(anthropicSSE)
	for {
		b, err := s.Next()
		if err == io.EOF {
			break
		}
		if strings.Contains(string(b), `"usage"`) {
			t.Fatal("usage chunk emitted without include_usage")
		}
	}
	if u, ok := s.Usage(); !ok || u.PromptTokens != 25 {
		t.Fatal("usage should still be tracked internally")
	}
}

func TestAnthropicStreamError(t *testing.T) {
	s := newAnthropicStream("anthropic", false)
	s.streamBase = fakeStreamBase("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	_, err := s.Next()
	ue, ok := err.(*UpstreamError)
	if !ok || !strings.Contains(ue.Message, "Overloaded") {
		t.Fatalf("expected upstream error, got %v", err)
	}
}
