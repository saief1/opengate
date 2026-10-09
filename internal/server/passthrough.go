package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/opengate-dev/opengate/internal/provider"
	"github.com/opengate-dev/opengate/internal/store"
)

// Raw OpenAI-compatible requests are forwarded with unknown fields intact.
// A few fields make upstream spend that OpenGate cannot bound or price from
// token usage, so they are refused unless the project allows them
// (projects.allowed_features).

// normalizeMaxTokens collapses max_tokens / max_completion_tokens into a
// single max_completion_tokens field holding the larger value.
func normalizeMaxTokens(req *provider.ChatRequest, raw map[string]json.RawMessage) {
	if req.MaxTokens == nil && req.MaxCompletionTokens == nil {
		return
	}
	mx := req.MaxOutputTokens()
	req.MaxTokens, req.MaxCompletionTokens = nil, &mx
	delete(raw, "max_tokens")
	raw["max_completion_tokens"], _ = json.Marshal(mx)
}

// hostedTool reports whether a tool type is provider-hosted (billed per call
// outside token usage) rather than a client-executed function.
func hostedTool(t string) bool {
	switch t {
	case "", "function", "custom":
		return false
	}
	return true
}

func toolTypes(raw json.RawMessage) []string {
	var tools []struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &tools)
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Type)
	}
	return out
}

func isTrue(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "true" }

func (s *Server) denyFeature(w http.ResponseWriter, rs *reqState, feature, what string) bool {
	if rs.proj.HasFeature(feature) {
		return false
	}
	s.reject(w, rs, http.StatusBadRequest, "invalid_request_error", "feature_not_allowed",
		fmt.Sprintf("OpenGate: %s is disabled for this project because its cost cannot be budgeted; an admin can allow feature %q.", what, feature), 0)
	return true
}

// checkChatFeatures returns false (after writing a 400) if the request uses
// a passthrough feature the project has not allowed.
func (s *Server) checkChatFeatures(w http.ResponseWriter, rs *reqState, req *provider.ChatRequest) bool {
	if req.N != nil && *req.N > 1 && s.denyFeature(w, rs, store.FeatureMultiChoice, "n > 1") {
		return false
	}
	hosted := rs.raw["web_search_options"] != nil || rs.raw["audio"] != nil ||
		strings.Contains(string(rs.raw["modalities"]), "audio")
	for _, t := range toolTypes(rs.raw["tools"]) {
		hosted = hosted || hostedTool(t)
	}
	if hosted && s.denyFeature(w, rs, store.FeatureBuiltinTools, "provider-hosted tools / audio output") {
		return false
	}
	if isTrue(rs.raw["store"]) && s.denyFeature(w, rs, store.FeatureStore, "store: true") {
		return false
	}
	return true
}

func (s *Server) checkResponsesFeatures(w http.ResponseWriter, rs *reqState) bool {
	if isTrue(rs.raw["background"]) && s.denyFeature(w, rs, store.FeatureBackground, "background mode") {
		return false
	}
	for _, t := range toolTypes(rs.raw["tools"]) {
		if hostedTool(t) && s.denyFeature(w, rs, store.FeatureBuiltinTools, "built-in tool "+t) {
			return false
		}
	}
	if isTrue(rs.raw["store"]) && s.denyFeature(w, rs, store.FeatureStore, "store: true") {
		return false
	}
	return true
}
