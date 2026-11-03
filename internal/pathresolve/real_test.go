package pathresolve

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSymlinkIsResolvedBeforeParentTraversal(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base")
	external := filepath.Join(root, "other", "child")
	for _, p := range []string{base, external} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(external, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := Real(base + "/link/../missing/deeper")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "other", "missing", "deeper")
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}

func TestMissingComponentsCanBeCancelledByParentTraversal(t *testing.T) {
	root := t.TempDir()
	got, err := Real(root + "/missing/../another")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "another") {
		t.Fatalf("resolved %q", got)
	}
}
