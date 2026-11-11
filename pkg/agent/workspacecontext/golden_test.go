package workspacecontext

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden rewrites testdata/golden/*.txt. Golden updates must be explicit:
//
//	go test ./pkg/agent/workspacecontext -run TestGolden -update
var updateGolden = flag.Bool("update", false, "update golden files under testdata/golden")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test ./pkg/agent/workspacecontext -run TestGolden -update)", path, err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// fixtureBaselineCandidates returns the deduped, read baseline candidates for
// the standard fixture. Display paths are project-relative, so goldens never
// contain absolute temp paths.
func fixtureBaselineCandidates(t *testing.T) []Candidate {
	t.Helper()
	workspace := prepareWorkspace(t)
	candidates, err := DiscoverBaseline(DefaultConfig(), workspace, "")
	if err != nil {
		t.Fatalf("DiscoverBaseline: %v", err)
	}
	read := readCandidates(t, candidates)
	kept, _ := DedupeByCandidateGroup(read)
	return kept
}

func TestGoldenBaseline(t *testing.T) {
	rendered := RenderBaseline(fixtureBaselineCandidates(t), DefaultConfig(), false)
	if rendered.Text == "" {
		t.Fatal("baseline rendered empty")
	}
	assertGolden(t, "baseline.txt", rendered.Text)
}

func TestGoldenBaselineReplace(t *testing.T) {
	rendered := RenderBaseline(fixtureBaselineCandidates(t), DefaultConfig(), true)
	assertGolden(t, "baseline_replace.txt", rendered.Text)
}

func TestGoldenEmptyReplacement(t *testing.T) {
	rendered := RenderBaseline(nil, DefaultConfig(), true)
	assertGolden(t, "baseline_empty_replacement.txt", rendered.Text)
}

func TestGoldenAdditional(t *testing.T) {
	rendered := RenderChanges([]ChangeItem{{
		Action:    ChangeActionSet,
		Scope:     scopeKey("packages/app", "AGENTS.md"),
		Directory: "packages/app",
		Path:      "packages/app/AGENTS.md",
		Digest:    ContentDigestString("# App 指令\n\n- 修改 `packages/app` 时同步更新文档。\n"),
		Content:   "# App 指令\n\n- 修改 `packages/app` 时同步更新文档。\n",
	}}, DefaultConfig())
	assertGolden(t, "additional.txt", rendered.Text)
}

func TestGoldenUpdated(t *testing.T) {
	rendered := RenderChanges([]ChangeItem{{
		Action:    ChangeActionReplace,
		Scope:     scopeKey("packages/app", "AGENTS.md"),
		Directory: "packages/app",
		Path:      "packages/app/AGENTS.md",
		Digest:    ContentDigestString("# App 指令 v2\n\n- 新版本。\n"),
		Content:   "# App 指令 v2\n\n- 新版本。\n",
	}}, DefaultConfig())
	assertGolden(t, "updated.txt", rendered.Text)
}

func TestGoldenRemoved(t *testing.T) {
	rendered := RenderChanges([]ChangeItem{{
		Action:    ChangeActionRemove,
		Scope:     scopeKey("packages/app", "AGENTS.md"),
		Directory: "packages/app",
		Path:      "packages/app/AGENTS.md",
	}}, DefaultConfig())
	assertGolden(t, "removed.txt", rendered.Text)
}

func TestGoldenBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxBytes = 220
	broad := makeCandidate("AGENTS.md", ".", strings.Repeat("宽文件", 100), false)
	specific := makeCandidate("packages/app/AGENTS.md", "packages/app", "具体指令", false)
	rendered := RenderBaseline([]Candidate{broad, specific}, cfg, false)
	assertGolden(t, "budget.txt", rendered.Text)
}

func TestGoldenEscape(t *testing.T) {
	workspace := prepareWorkspace(t)
	cfg := DefaultConfig()
	candidates, err := DiscoverBaseline(cfg, workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	read := readCandidates(t, candidates)
	var escape []Candidate
	for _, candidate := range read {
		if candidate.DisplayPath == "escape/AGENTS.md" {
			escape = append(escape, candidate)
		}
	}
	// escape/ is not on the ancestor chain of the workspace root; read it
	// explicitly to cover the escaping golden.
	escapePath := filepath.Join(workspace, "escape", "AGENTS.md")
	content, _, readErr := ReadBounded(escapePath, DefaultMaxSourceBytes)
	if readErr != nil {
		t.Fatalf("read escape fixture: %v", readErr)
	}
	escapeCandidate := makeCandidate("escape/AGENTS.md", "escape", string(content), false)
	rendered := RenderBaseline([]Candidate{escapeCandidate}, cfg, false)
	assertGolden(t, "escape.txt", rendered.Text)
	if len(escape) != 0 {
		t.Fatalf("escape/AGENTS.md must not be part of the root baseline chain: %v", displayPaths(escape))
	}
}

func TestGoldenTwoChains(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UserGlobal = true
	files := []Candidate{
		makeCandidate("~/.chagent/AGENTS.md", scopeUserChagent, "# 用户级 Chagent 指令\n\n- 用户级规则适用于所有工作。\n", true),
		makeCandidate("AGENTS.md", ".", "# 根指令\n\n- 始终使用中文回复。\n", false),
	}
	rendered := RenderBaseline(files, cfg, false)
	assertGolden(t, "two_chains.txt", rendered.Text)
}
