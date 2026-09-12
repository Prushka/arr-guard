package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// RepositoryRoot supports opt-in live fixtures regardless of their Go package's
// working directory. It never reads or reports credential values.
func RepositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal("cannot locate repository for live verification")
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && info.Mode().IsRegular() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot locate go.mod for live verification")
		}
		dir = parent
	}
}
