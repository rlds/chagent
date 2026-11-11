package fstools

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestExternalPathRequiresExplicitPermission(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "external")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(outside, "data.txt")
	if err := os.WriteFile(file, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidatePathWithAllowPaths(file, workspace, true, nil); err == nil {
		t.Fatal("external path must be denied without allow_read_paths")
	}
	resolvedFile, resolveErr := filepath.EvalSymlinks(file)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile("^" + regexp.QuoteMeta(file) + "$"),
		regexp.MustCompile("^" + regexp.QuoteMeta(resolvedFile) + "$"),
	}
	got, err := ValidatePathWithAllowPaths(file, workspace, true, patterns)
	if err != nil {
		t.Fatalf("explicitly allowed external path denied: %v", err)
	}
	if got != file {
		t.Fatalf("resolved path = %q, want %q", got, file)
	}
	// Traversal still has to be denied even when an allow pattern exists for
	// another path.
	other := filepath.Join(outside, "other.txt")
	if _, err := ValidatePathWithAllowPaths(other, workspace, true, patterns); err == nil {
		t.Fatal("non-matching external path must remain denied")
	}
}
