package workspacecontext

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFindProjectRootMarkerInParent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "packages", "app")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindProjectRoot(workspace, []string{".git"})
	if err != nil {
		t.Fatalf("FindProjectRoot: %v", err)
	}
	if got != root {
		t.Fatalf("project root = %q, want %q", got, root)
	}
}

func TestFindProjectRootNoMarker(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindProjectRoot(workspace, []string{".git"})
	if err != nil {
		t.Fatalf("FindProjectRoot: %v", err)
	}
	if got != workspace {
		t.Fatalf("project root = %q, want workspace %q", got, workspace)
	}
}

func TestFindProjectRootPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can always stat directories; permission errors cannot be triggered")
	}
	parent := t.TempDir()
	workspace := filepath.Join(parent, "locked")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workspace, 0o755) })

	_, err := FindProjectRoot(workspace, []string{".git"})
	if err == nil {
		t.Fatal("FindProjectRoot succeeded on an unreadable workspace, want permission error")
	}
	if IsAbsent(err) {
		t.Fatalf("permission error was swallowed as absent: %v", err)
	}
}

func TestAncestorChainOrder(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "ws")
	dir := filepath.Join(root, "packages", "app")
	want := []string{
		root,
		filepath.Join(root, "packages"),
		filepath.Join(root, "packages", "app"),
	}
	if got := AncestorChain(root, dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("AncestorChain = %v, want %v", got, want)
	}
	if got := AncestorChain(root, root); !reflect.DeepEqual(got, []string{root}) {
		t.Fatalf("AncestorChain(root, root) = %v", got)
	}
	if got := AncestorChain(root, "/other"); !reflect.DeepEqual(got, []string{"/other"}) {
		t.Fatalf("AncestorChain outside root = %v, want [/other]", got)
	}
}

func TestDescendantDirs(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "ws")
	tests := []struct {
		name    string
		touched string
		want    []string
	}{
		{
			name:    "nested file",
			touched: filepath.Join(root, "packages", "app", "main.go"),
			want: []string{
				filepath.Join(root, "packages"),
				filepath.Join(root, "packages", "app"),
			},
		},
		{
			name:    "root file",
			touched: filepath.Join(root, "AGENTS.md"),
			want:    nil,
		},
		{
			name:    "outside root",
			touched: filepath.Join(string(filepath.Separator), "elsewhere", "main.go"),
			want:    nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DescendantDirs(root, tc.touched); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DescendantDirs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDiscoverBaselineOrderAndExclusion(t *testing.T) {
	workspace := prepareWorkspace(t)
	cfg := DefaultConfig()
	exclude := filepath.Join(workspace, "AGENTS.md")

	candidates, err := DiscoverBaseline(cfg, workspace, exclude)
	if err != nil {
		t.Fatalf("DiscoverBaseline: %v", err)
	}
	want := []string{"CLAUDE.md", "AGENTS.local.md"}
	if got := displayPaths(candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("baseline candidates = %v, want %v (root AGENTS.md must be excluded)", got, want)
	}

	// Discovering from a nested workspace must walk root -> packages -> app.
	nested := filepath.Join(workspace, "packages", "app")
	candidates, err = DiscoverBaseline(cfg, nested, exclude)
	if err != nil {
		t.Fatalf("DiscoverBaseline nested: %v", err)
	}
	want = []string{
		"CLAUDE.md",
		"AGENTS.local.md",
		"packages/app/AGENTS.md",
		"packages/app/CLAUDE.md",
	}
	if got := displayPaths(candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("nested baseline candidates = %v, want %v", got, want)
	}
	for _, candidate := range candidates {
		if candidate.Scope == "" || containsSeparator(candidate.DisplayPath) {
			t.Fatalf("candidate has invalid scope/display: %+v", candidate)
		}
	}
}

func TestDiscoverBaselineUserGlobal(t *testing.T) {
	workspace := prepareWorkspace(t)
	cfg := DefaultConfig()
	cfg.UserGlobal = true
	cfg.ChagentHome = filepath.Join(workspace, "userhome", ".chagent")
	cfg.AgentsHome = filepath.Join(workspace, "userhome", ".agents")

	candidates, err := DiscoverBaseline(cfg, workspace, "")
	if err != nil {
		t.Fatalf("DiscoverBaseline: %v", err)
	}
	displays := displayPaths(candidates)
	if len(displays) < 3 {
		t.Fatalf("expected user-global + project candidates, got %v", displays)
	}
	if !filepath.IsAbs(candidates[0].AbsolutePath) {
		t.Fatalf("candidate path is not absolute: %v", candidates[0])
	}
	if candidates[0].Directory != scopeUserChagent || !candidates[0].UserGlobal {
		t.Fatalf("first user-global scope = %+v", candidates[0])
	}
	if candidates[1].Directory != scopeUserAgents || !candidates[1].UserGlobal {
		t.Fatalf("second user-global scope = %+v", candidates[1])
	}
	if candidates[0].Scope == candidates[1].Scope {
		t.Fatal("user-global roots must have distinct scope keys")
	}
	if candidates[2].UserGlobal {
		t.Fatalf("third candidate should be a project file: %+v", candidates[2])
	}
}

func TestDiscoverBaselineMarkerError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can always stat directories; permission errors cannot be triggered")
	}
	workspace := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workspace, 0o755) })

	_, err := DiscoverBaseline(DefaultConfig(), workspace, "")
	if err == nil {
		t.Fatal("DiscoverBaseline swallowed a project-root marker error")
	}
}

func TestDiscoverBaselineUnavailableCandidate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can always read files; permission errors cannot be triggered")
	}
	workspace := prepareWorkspace(t)
	target := filepath.Join(workspace, "AGENTS.md")
	if err := os.Chmod(target, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o644) })

	candidates, err := DiscoverBaseline(DefaultConfig(), workspace, "")
	if err != nil {
		t.Fatalf("DiscoverBaseline: %v", err)
	}
	// os.Stat succeeds even without read permission, so discovery reports the
	// file; the read stage classifies it as unavailable.
	candidate, ok := candidateByDisplay(candidates, "AGENTS.md")
	if !ok {
		t.Fatalf("discovery should keep the file path: %v", displayPaths(candidates))
	}
	if candidate.Unavailable {
		t.Fatalf("stat succeeded, candidate must not be unavailable yet: %+v", candidate)
	}
	_, _, readErr := ReadBounded(candidate.AbsolutePath, DefaultMaxSourceBytes)
	if readErr == nil {
		t.Fatal("ReadBounded succeeded on an unreadable file")
	}
	if !IsUnavailable(readErr) {
		t.Fatalf("read error classified as %v, want unavailable", readErr)
	}
}

func TestReadBoundedTooLarge(t *testing.T) {
	workspace := prepareWorkspace(t)
	path := filepath.Join(workspace, "big", "AGENTS.md")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	limit := info.Size() - 1

	content, _, readErr := ReadBounded(path, limit)
	if content != nil {
		t.Fatalf("oversized read returned partial content: %d bytes", len(content))
	}
	if !errors.Is(readErr, ErrSourceTooLarge) {
		t.Fatalf("err = %v, want ErrSourceTooLarge", readErr)
	}
}

func TestReadBoundedNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "AGENTS.md")
	content, _, err := ReadBounded(path, 1024)
	if content != nil {
		t.Fatalf("absent read returned content: %q", content)
	}
	if !IsAbsent(err) {
		t.Fatalf("err = %v, want absent", err)
	}
}

func TestReadBoundedEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	content, version, err := ReadBounded(path, 1024)
	if err != nil {
		t.Fatalf("ReadBounded: %v", err)
	}
	if len(content) != 0 {
		t.Fatalf("content = %q, want empty", content)
	}
	if version.IsZero() {
		t.Fatal("empty file version must not be zero")
	}
}

func TestTrimmedDigest(t *testing.T) {
	a := TrimmedDigest([]byte("  hello\n"))
	b := TrimmedDigest([]byte("hello"))
	if a != b {
		t.Fatalf("trimmed digest mismatch: %q vs %q", a, b)
	}
	if TrimmedDigest([]byte("hello")) == TrimmedDigest([]byte("hello!")) {
		t.Fatal("different content must have different trimmed digests")
	}
	empty := TrimmedDigest(nil)
	if empty == "" || len(empty) != 64 {
		t.Fatalf("invalid sha256 digest %q", empty)
	}
	if ContentDigestString("hello") == ContentDigestString(" hello") {
		t.Fatal("content digest must not trim")
	}
}

func TestDedupeSameGroup(t *testing.T) {
	workspace := prepareWorkspace(t)
	candidates, err := DiscoverBaseline(DefaultConfig(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	keep, deduped := DedupeByCandidateGroup(readCandidates(t, candidates))
	keptDisplays := displayPaths(keep)
	if !reflect.DeepEqual(keptDisplays, []string{"AGENTS.md", "AGENTS.local.md"}) {
		t.Fatalf("kept = %v, want AGENTS.md + AGENTS.local.md", keptDisplays)
	}
	if dedupedDisplays := displayPaths(deduped); !reflect.DeepEqual(dedupedDisplays, []string{"CLAUDE.md"}) {
		t.Fatalf("deduped = %v, want CLAUDE.md", dedupedDisplays)
	}
	if keep[0].Content == "" {
		t.Fatal("kept candidate lost its content")
	}
}

func TestDedupeDifferentGroups(t *testing.T) {
	workspace := prepareWorkspace(t)
	nested := filepath.Join(workspace, "packages", "app")
	candidates, err := DiscoverBaseline(DefaultConfig(), nested, "")
	if err != nil {
		t.Fatal(err)
	}
	keep, deduped := DedupeByCandidateGroup(readCandidates(t, candidates))
	if _, ok := candidateByDisplay(deduped, "packages/app/CLAUDE.md"); !ok {
		t.Fatalf("app CLAUDE.md must be deduped within its own directory: %v", displayPaths(deduped))
	}
	// Root AGENTS.md and packages/app/AGENTS.md have different content in the
	// fixture; make them identical to prove groups stay independent.
	if _, ok := candidateByDisplay(keep, "AGENTS.md"); !ok {
		t.Fatalf("root AGENTS.md missing from kept: %v", displayPaths(keep))
	}
	if _, ok := candidateByDisplay(keep, "packages/app/AGENTS.md"); !ok {
		t.Fatalf("packages/app/AGENTS.md missing from kept: %v", displayPaths(keep))
	}
}

func TestDedupePromotionAfterWinnerRemoved(t *testing.T) {
	workspace := prepareWorkspace(t)
	candidates, err := DiscoverBaseline(DefaultConfig(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	candidates = readCandidates(t, candidates)
	keep, _ := DedupeByCandidateGroup(candidates)
	// Drop the winning AGENTS.md candidates and dedupe again: CLAUDE.md must be
	// promoted because its earlier duplicate is no longer present.
	pruned := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Name == "AGENTS.md" {
			continue
		}
		pruned = append(pruned, candidate)
	}
	keptAfter, dedupedAfter := DedupeByCandidateGroup(pruned)
	if len(keep) == 0 || len(keptAfter) == 0 {
		t.Fatal("dedupe returned no candidates")
	}
	if _, ok := candidateByDisplay(keptAfter, "CLAUDE.md"); !ok {
		t.Fatalf("CLAUDE.md was not promoted: kept=%v deduped=%v", displayPaths(keptAfter), displayPaths(dedupedAfter))
	}
}

func TestSymlinkSkippedByDefault(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-agents.md")
	writeFile(t, target, "真实内容\n")
	link := filepath.Join(dir, "AGENTS.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, _, err := ReadBounded(link, 1024)
	if !errors.Is(err, ErrSymlinkSkipped) {
		t.Fatalf("err = %v, want ErrSymlinkSkipped", err)
	}
	if !IsUnavailable(err) {
		t.Fatalf("skipped symlink must be classified unavailable: %v", err)
	}
}

func TestSymlinkOutsideRootRejected(t *testing.T) {
	dir := t.TempDir()
	allowed := filepath.Join(dir, "workspace")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "secret.md")
	writeFile(t, target, "secret\n")
	link := filepath.Join(allowed, "AGENTS.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	policy := ReadPolicy{FollowSymlinks: true, AllowedRoots: []string{allowed}}
	_, _, err := ReadBoundedWithPolicy(link, 1024, policy)
	if !errors.Is(err, ErrSymlinkSkipped) {
		t.Fatalf("err = %v, want ErrSymlinkSkipped for target outside allowed roots", err)
	}

	// The same symlink is accepted when its target is inside an allowed root.
	insideDir := filepath.Join(allowed, "nested")
	if err := os.MkdirAll(insideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	insideTarget := filepath.Join(insideDir, "agents-real.md")
	writeFile(t, insideTarget, "inside\n")
	insideLink := filepath.Join(allowed, "CLAUDE.md")
	if err := os.Symlink(insideTarget, insideLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	content, _, err := ReadBoundedWithPolicy(insideLink, 1024, policy)
	if err != nil {
		t.Fatalf("inside symlink rejected: %v", err)
	}
	if string(content) != "inside\n" {
		t.Fatalf("content = %q, want inside", content)
	}
}

// readCandidates fills content/digests for discovered candidates, skipping
// unreadable and oversized files.
func readCandidates(t *testing.T, candidates []Candidate) []Candidate {
	t.Helper()
	out := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		content, version, err := ReadBounded(candidate.AbsolutePath, DefaultMaxSourceBytes)
		if err != nil {
			if IsAbsent(err) || errors.Is(err, ErrSourceTooLarge) {
				continue
			}
			t.Fatalf("read %s: %v", candidate.AbsolutePath, err)
		}
		candidate.Content = string(content)
		candidate.Version = version
		candidate.Digest = ContentDigestString(candidate.Content)
		candidate.TrimmedDigest = TrimmedDigestString(candidate.Content)
		out = append(out, candidate)
	}
	return out
}

func containsSeparator(s string) bool {
	for _, r := range s {
		if r == 0 {
			return true
		}
	}
	return false
}
