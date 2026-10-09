package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPICoversAllRoutes keeps docs/openapi.yaml in sync with the admin
// routes registered in this package (regenerate with tools/gen_openapi.py).
func TestOpenAPICoversAllRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	documented := map[string]bool{}
	for p, ops := range spec.Paths {
		for m := range ops {
			documented[strings.ToUpper(m)+" "+p] = true
		}
	}
	re := regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE) (/admin/api/[^"]+)"`)
	files, _ := filepath.Glob("*.go")
	registered := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			registered[m[1]+" "+m[2]] = true
		}
	}
	if len(registered) < 50 {
		t.Fatalf("found only %d routes; did registration move?", len(registered))
	}
	for r := range registered {
		if !documented[r] {
			t.Errorf("route %s is not in docs/openapi.yaml", r)
		}
	}
	for d := range documented {
		if !registered[d] {
			t.Errorf("docs/openapi.yaml documents %s, which is not registered", d)
		}
	}
}
