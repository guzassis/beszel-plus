package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeDrainRecognizesLiveAndStaleOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-in-progress")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"pid":%d}`, os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if !upgradeDrainActiveAt(path) {
		t.Fatal("live installer drain was ignored")
	}
	if err := os.WriteFile(path, []byte(`{"pid":2147483647}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if upgradeDrainActiveAt(path) {
		t.Fatal("stale installer drain remained active")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale drain was not cleaned: %v", err)
	}
}
