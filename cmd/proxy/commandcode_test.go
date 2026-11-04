package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCommandCodeProviderMergesAndPreserves(t *testing.T) {
	t.Parallel()
	models := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Errorf("path = %q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-5.6-sol","object":"model","owned_by":"codex"},
			{"id":"claude-sonnet-5","object":"model","owned_by":"claude","display_name":"Sonnet"}
		]}`))
	}))
	defer models.Close()

	path := filepath.Join(t.TempDir(), ".commandcode", "providers.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	seed := `{"provider":{"openrouter":{"baseURL":"https://openrouter.ai/api/v1","apiKey":"$OPENROUTER_API_KEY","models":{"x":{}}}}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	for range 2 {
		if err := ensureCommandCodeProviderAt(path, models.URL, io.Discard); err != nil {
			t.Fatalf("ensure provider: %v", err)
		}
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read providers: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode providers: %v", err)
	}
	providers, _ := document["provider"].(map[string]any)
	if _, ok := providers["openrouter"]; !ok {
		t.Fatal("openrouter provider was dropped")
	}
	rute, ok := providers["rute"].(map[string]any)
	if !ok {
		t.Fatal("rute provider was not added")
	}
	if rute["baseURL"] != models.URL+"/v1" {
		t.Errorf("baseURL = %v", rute["baseURL"])
	}
	if rute["apiKey"] != false {
		t.Errorf("apiKey = %v", rute["apiKey"])
	}
	entries, _ := rute["models"].(map[string]any)
	claude, _ := entries["claude-sonnet-5"].(map[string]any)
	if claude["api"] != "anthropic-messages" || claude["baseURL"] != models.URL {
		t.Errorf("claude model = %+v", claude)
	}
	codex, _ := entries["gpt-5.6-sol"].(map[string]any)
	if codex["api"] != "openai-responses" {
		t.Errorf("codex model = %+v", codex)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat providers: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("providers mode = %04o, want 0600", got)
	}
}

func TestEnsureCommandCodeProviderLeavesExistingEntry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".commandcode", "providers.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	seed := `{"provider":{"rute":{"baseURL":"http://custom","models":{"m":{}}}}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	// An unreachable proxy URL proves no model fetch happens for an existing entry.
	if err := ensureCommandCodeProviderAt(path, "http://127.0.0.1:1", io.Discard); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read providers: %v", err)
	}
	if string(body) != seed {
		t.Errorf("existing providers.json changed: %s", body)
	}
}
