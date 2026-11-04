package routingfile

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSaveKeepsOtherProviders(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "routing.json")
	file := New(path)
	if routing, err := file.Load(); err != nil || len(routing) != 0 {
		t.Fatalf("missing file = %v, %v", routing, err)
	}
	claude := Routing{Balance: "priority", Order: []string{"default", "personal"}, MaxInFlight: 6}
	if err := file.Save("claude", claude); err != nil {
		t.Fatal(err)
	}
	if err := file.Save("codex", Routing{Balance: "spread"}); err != nil {
		t.Fatal(err)
	}
	routing, err := New(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(routing["claude"], claude) || routing["codex"].Balance != "spread" {
		t.Errorf("routing = %+v", routing)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", info.Mode().Perm(), err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Load(); err == nil {
		t.Error("invalid file loaded")
	}
}
