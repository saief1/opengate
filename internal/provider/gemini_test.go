package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToGeminiRequest(t *testing.T) {
	r := mustReq(t, `{"model":"gemini/gemini-2.5-flash","max_tokens":50,"temperature":0.5,
	 "messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},
	  {"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
	  {"role":"tool","tool_call_id":"c1","content":"plain result"}],
	 "tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","additionalProperties":false,"properties":{"a":{"type":"integer"}}}}}],
	 "tool_choice":"auto"}`)
	g, err := ToGeminiRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if g.SystemInstruction.Parts[0].Text != "sys" || g.GenerationConfig.MaxOutputTokens != 50 {
		t.Fatalf("system/config: %+v", g)
	}
	if len(g.Contents) != 3 || g.Contents[1].Role != "model" || g.Contents[1].Parts[0].FunctionCall.Name != "f" {
		b, _ := json.Marshal(g.Contents)
		t.Fatalf("contents: %s", b)
	}
	fr := g.Contents[2].Parts[0].FunctionResponse
	if fr.Name != "f" || string(fr.Response) != `{"result":"plain result"}` {
		t.Fatalf("function response: %+v", fr)
	}
	if strings.Contains(string(g.Tools[0].FunctionDeclarations[0].Parameters), "additionalProperties") {
		t.Fatal("schema not cleaned")
	}
	if g.ToolConfig.FunctionCallingConfig.Mode != "AUTO" {
		t.Fatal("tool config")
	}
}
