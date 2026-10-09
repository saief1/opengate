package guard

import (
	"bytes"
	"encoding/json"
)

// textKeys are JSON keys whose string values (recursively) are prose that
// may carry PII. Structural keys (role, type, ids, URLs, tool names) are skipped.
var textKeys = map[string]bool{"content": true, "text": true, "input": true, "instructions": true, "prompt": true, "output": true, "arguments": true, "messages": true, "choices": true, "message": true, "delta": true}

var skipKeys = map[string]bool{"role": true, "type": true, "id": true, "call_id": true, "tool_call_id": true, "image_url": true, "url": true,
	"file_id": true, "file_data": true, "detail": true, "name": true, "status": true, "model": true, "object": true, "finish_reason": true, "logprobs": true, "usage": true}

// StringFunc transforms one prose string; role is the nearest enclosing
// message role ("" if unknown).
type StringFunc func(role, s string) string

// Walk applies fn to every prose string in the given top-level fields of a
// JSON object and re-encodes changed fields in place. It reports whether
// anything changed.
func Walk(raw map[string]json.RawMessage, fields []string, fn StringFunc) (bool, error) {
	changed := false
	for _, f := range fields {
		v, ok := raw[f]
		if !ok || len(v) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.UseNumber()
		var val any
		if err := dec.Decode(&val); err != nil {
			return changed, err
		}
		role := ""
		if f == "instructions" || f == "system" {
			role = "system"
		}
		nv, ch := walk(val, f, role, fn)
		if ch {
			b, err := marshal(nv)
			if err != nil {
				return changed, err
			}
			raw[f], changed = b, true
		}
	}
	return changed, nil
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func walk(v any, key, role string, fn StringFunc) (any, bool) {
	switch t := v.(type) {
	case string:
		if !textKeys[key] {
			return v, false
		}
		ns := fn(role, t)
		return ns, ns != t
	case []any:
		changed := false
		for i, e := range t {
			ne, ch := walk(e, key, role, fn)
			if ch {
				t[i], changed = ne, true
			}
		}
		return t, changed
	case map[string]any:
		if r, ok := t["role"].(string); ok {
			role = r
		}
		changed := false
		for k, e := range t {
			if skipKeys[k] {
				continue
			}
			ne, ch := walk(e, k, role, fn)
			if ch {
				t[k], changed = ne, true
			}
		}
		return t, changed
	}
	return v, false
}
