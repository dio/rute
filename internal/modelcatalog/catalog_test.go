package modelcatalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogResolveAndList(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "models.json")
	body := `[
	  {"id":"worker-sonnet","provider":"claude","upstream_model":"claude-sonnet-5","display_name":"Sonnet"},
	  {"id":"coding","provider":"codex","upstream_model":"gpt-5.6-sol"}
	]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	catalog, err := Load(path)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	tests := []struct {
		name     string
		model    string
		provider string
		upstream string
	}{
		{
			name:     "mapped Claude",
			model:    "worker-sonnet",
			provider: "claude",
			upstream: "claude-sonnet-5",
		},
		{
			name:     "Claude namespace",
			model:    "claude/claude-opus-5",
			provider: "claude",
			upstream: "claude-opus-5",
		},
		{
			name:     "unprefixed defaults to Codex",
			model:    "gpt-test",
			provider: "codex",
			upstream: "gpt-test",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := catalog.Resolve(test.model, "gpt-default")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.Provider != test.provider ||
				resolution.UpstreamModel != test.upstream {
				t.Errorf("resolution = %+v", resolution)
			}
		})
	}
	models := catalog.Models("gpt-default")
	if len(models) != 4 {
		t.Fatalf("models = %+v", models)
	}
}

func TestCatalogRoutesCommandCode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "models.json")
	body := `[{"id":"cc-sonnet","provider":"commandcode","upstream_model":"claude-sonnet-4-6"}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	catalog, err := Load(path)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	tests := []struct {
		model    string
		upstream string
	}{
		{model: "cc-sonnet", upstream: "claude-sonnet-4-6"},
		{model: "commandcode/deepseek/deepseek-v4-flash", upstream: "deepseek/deepseek-v4-flash"},
	}
	for _, test := range tests {
		resolution, err := catalog.Resolve(test.model, "gpt-default")
		if err != nil {
			t.Fatalf("resolve %q: %v", test.model, err)
		}
		if resolution.Provider != "commandcode" || resolution.UpstreamModel != test.upstream {
			t.Errorf("resolution for %q = %+v", test.model, resolution)
		}
	}
}

func TestCatalogRejectsInvalidMappings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{
			name: "duplicate",
			body: `[{"id":"a","provider":"codex","upstream_model":"gpt"},
				{"id":"a","provider":"claude","upstream_model":"claude-sonnet-5"}]`,
		},
		{
			name: "unknown field",
			body: `[{"id":"a","provider":"codex","upstream_model":"gpt","url":"https://example.com"}]`,
		},
		{
			name: "upstream namespace",
			body: `[{"id":"a","provider":"claude","upstream_model":"claude/claude-sonnet-5"}]`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "models.json")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatalf("write catalog: %v", err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("load succeeded")
			}
		})
	}
}
