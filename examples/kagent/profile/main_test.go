package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wrapper reads a missing or null governed stanza as observed mode, which
// brokers no Orka tools.
func TestOnlyAGovernedConfigurationListsTools(t *testing.T) {
	adapter := "sha256:" + strings.Repeat("a", 64)
	for _, config := range []string{`{"model":"m"}`, `{"model":"m","governed":null}`} {
		path := filepath.Join(t.TempDir(), "kagent.json")
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		err := run(adapter, path, []string{"probe"}, nil)
		if err == nil || !strings.Contains(err.Error(), "only a governed kagent configuration") {
			t.Errorf("%s: err = %v", config, err)
		}
	}
}
