package workspacecontext

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"
)

// copyTree copies src into dst recursively, preserving file contents.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture %s -> %s: %v", src, dst, err)
	}
}

// prepareWorkspace copies testdata/workspace into a fresh temp dir and adds the
// .git project-root marker. The .git directory is created at runtime because git
// refuses to track paths under a directory named .git.
func prepareWorkspace(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "ws")
	copyTree(t, filepath.Join("testdata", "workspace"), dst)
	if err := os.MkdirAll(filepath.Join(dst, ".git"), 0o755); err != nil {
		t.Fatalf("create fixture .git marker: %v", err)
	}
	return dst
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

func candidateByDisplay(candidates []Candidate, display string) (Candidate, bool) {
	for _, candidate := range candidates {
		if candidate.DisplayPath == display {
			return candidate, true
		}
	}
	return Candidate{}, false
}

func displayPaths(candidates []Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.DisplayPath)
	}
	return out
}

// makeCandidate builds a fully populated candidate for pure rendering tests.
func makeCandidate(display, directory, content string, userGlobal bool) Candidate {
	name := path.Base(display)
	return Candidate{
		AbsolutePath:  "/fixture/" + display,
		DisplayPath:   display,
		Directory:     directory,
		Name:          name,
		Scope:         scopeKey(directory, name),
		Content:       content,
		Digest:        ContentDigestString(content),
		TrimmedDigest: TrimmedDigestString(content),
		UserGlobal:    userGlobal,
	}
}
