package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// registryEntries reads tools/registry.yaml. The file keeps to a small
// fixed shape (a "tools:" list of flat entries), so it is read line by
// line and anything outside that shape fails the test.
func registryEntries(t *testing.T) []map[string]string {
	t.Helper()
	raw, err := os.ReadFile("registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]string
	seenTools := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line == "tools:" {
			seenTools = true
			continue
		}
		if !seenTools {
			t.Fatalf("unexpected line before tools: %q", line)
		}
		if strings.HasPrefix(line, "  - ") {
			entries = append(entries, map[string]string{})
			trimmed = strings.TrimPrefix(trimmed, "- ")
		} else if !strings.HasPrefix(line, "    ") || len(entries) == 0 {
			t.Fatalf("unexpected line: %q", line)
		}
		k, v, ok := strings.Cut(trimmed, ": ")
		if !ok {
			t.Fatalf("unexpected line: %q", line)
		}
		entries[len(entries)-1][k] = v
	}
	return entries
}

// TestRegistry holds tools/registry.yaml to the repository: every entry is
// a name from component.json with its three parts in place, and every
// folder under tools/ is listed.
func TestRegistry(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "component.json"))
	if err != nil {
		t.Fatal(err)
	}
	var component struct {
		SubTools []string `json:"subTools"`
	}
	if err := json.Unmarshal(raw, &component); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, s := range component.SubTools {
		allowed[s] = true
	}

	listed := map[string]bool{}
	for _, e := range registryEntries(t) {
		name := e["name"]
		if !allowed[name] {
			t.Errorf("%q is not a sub-tool named in component.json", name)
		}
		if listed[name] {
			t.Errorf("%q is listed twice", name)
		}
		listed[name] = true
		if want := "tools/" + name; e["folder"] != want {
			t.Errorf("%s: folder is %q, want %q", name, e["folder"], want)
		}
		for _, part := range []string{"folder", "match", "logic", "fixtures"} {
			if e[part] == "" {
				t.Errorf("%s: no %s", name, part)
				continue
			}
			if _, err := os.Stat(filepath.Join("..", filepath.FromSlash(e[part]))); err != nil {
				t.Errorf("%s: %s %q does not exist", name, part, e[part])
			}
		}
		fixtures, _ := filepath.Glob(filepath.Join("..", filepath.FromSlash(e["fixtures"]), "*", "want.json"))
		if len(fixtures) == 0 {
			t.Errorf("%s: no saved examples", name)
		}
	}

	dirs, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if d.IsDir() && !listed[d.Name()] {
			t.Errorf("folder tools/%s is not listed in registry.yaml", d.Name())
		}
	}
}
