package goal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustManager(t *testing.T) (*Manager, string) {
	t.Helper()
	ws := t.TempDir()
	return NewManager(ws), ws
}

func mustCreate(t *testing.T, m *Manager, objective, note string) Goal {
	t.Helper()
	goal, err := m.Create(objective, note)
	if err != nil {
		t.Fatalf("Create(%q): %v", objective, err)
	}
	return goal
}

func TestNewManagerRejectsEmptyWorkspace(t *testing.T) {
	m := NewManager("")
	if m.Dir() != "" {
		t.Fatalf("Dir() = %q, want empty", m.Dir())
	}
	if _, err := m.Create("objective", ""); ErrorCode(err) != "goal_no_workspace" {
		t.Fatalf("Create error = %v, want code goal_no_workspace", err)
	}
	if goals, skipped := m.LoadAll(); len(goals) != 0 || len(skipped) != 1 {
		t.Fatalf("LoadAll = (%v, %v), want empty goals + one diagnostic", goals, skipped)
	}
}

func TestNewManagerRejectsFileWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	if _, err := m.Create("objective", ""); ErrorCode(err) != "goal_bad_workspace" {
		t.Fatalf("Create error = %v, want code goal_bad_workspace", err)
	}
}

func TestCreatePersistsGoalFile(t *testing.T) {
	m, ws := mustManager(t)
	goal := mustCreate(t, m, "  完成上下文差异分析  ", "  备注  ")

	if !idPattern.MatchString(goal.ID) {
		t.Fatalf("id %q does not match %s", goal.ID, idPattern)
	}
	if goal.Status != StatusActive {
		t.Fatalf("status = %q, want active", goal.Status)
	}
	if goal.Objective != "完成上下文差异分析" {
		t.Fatalf("objective = %q, want trimmed", goal.Objective)
	}
	if goal.Note != "备注" {
		t.Fatalf("note = %q, want trimmed", goal.Note)
	}
	if goal.Evidence == nil || len(goal.Evidence) != 0 {
		t.Fatalf("evidence = %#v, want empty non-nil slice", goal.Evidence)
	}
	if _, err := time.Parse(time.RFC3339Nano, goal.CreatedAt); err != nil {
		t.Fatalf("created_at %q: %v", goal.CreatedAt, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, goal.UpdatedAt); err != nil {
		t.Fatalf("updated_at %q: %v", goal.UpdatedAt, err)
	}

	wantDir := filepath.Join(ws, "state", "goals")
	if m.Dir() != wantDir {
		t.Fatalf("Dir() = %q, want %q", m.Dir(), wantDir)
	}
	dirInfo, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf("stat goals dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("goals dir perm = %o, want 0700", perm)
	}
	path := filepath.Join(wantDir, goal.ID+".json")
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat goal file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("goal file perm = %o, want 0600", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read goal file: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("goal file is not JSON: %v\n%s", err, raw)
	}
	for _, key := range []string{"id", "objective", "status", "created_at", "updated_at", "evidence"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("persisted JSON missing key %q: %s", key, raw)
		}
	}
	if _, ok := decoded["evidence"].([]any); !ok {
		t.Fatalf("persisted evidence = %T, want array", decoded["evidence"])
	}

	reloaded := NewManager(ws)
	got, err := reloaded.Get(goal.ID)
	if err != nil {
		t.Fatalf("reload Get: %v", err)
	}
	if got.ID != goal.ID || got.Objective != goal.Objective || got.Status != goal.Status ||
		got.Note != goal.Note || got.CreatedAt != goal.CreatedAt || got.UpdatedAt != goal.UpdatedAt {
		t.Fatalf("reloaded goal = %+v, want %+v", got, goal)
	}
}

func TestCreateRejectsEmptyObjective(t *testing.T) {
	m, _ := mustManager(t)
	if _, err := m.Create("   ", ""); ErrorCode(err) != "goal_invalid_argument" {
		t.Fatalf("Create error = %v, want goal_invalid_argument", err)
	}
}

func TestIDSequenceContinuesAfterRestart(t *testing.T) {
	m, ws := mustManager(t)
	first := mustCreate(t, m, "first", "")
	second := mustCreate(t, m, "second", "")
	if first.ID == second.ID {
		t.Fatalf("duplicate id %q", first.ID)
	}
	firstSeq, _ := parseSeqFromID(first.ID)
	secondSeq, _ := parseSeqFromID(second.ID)
	if secondSeq <= firstSeq {
		t.Fatalf("sequence did not increase: %d then %d", firstSeq, secondSeq)
	}

	restarted := NewManager(ws)
	third := mustCreate(t, restarted, "third", "")
	thirdSeq, _ := parseSeqFromID(third.ID)
	if thirdSeq <= secondSeq {
		t.Fatalf("restarted manager sequence = %d, want > %d", thirdSeq, secondSeq)
	}
}

func TestUpdateLifecycleAndEvidence(t *testing.T) {
	m, _ := mustManager(t)
	goal := mustCreate(t, m, "目标", "")

	if _, err := m.Update(goal.ID, StatusComplete, "", nil, true, false, false); ErrorCode(err) != "goal_missing_evidence" {
		t.Fatalf("complete without evidence error = %v, want goal_missing_evidence", err)
	}

	updated, err := m.Update(goal.ID, "", "补充备注", []string{"evidence-1", "evidence-1"}, false, true, true)
	if err != nil {
		t.Fatalf("note+evidence update: %v", err)
	}
	if updated.Note != "补充备注" {
		t.Fatalf("note = %q", updated.Note)
	}
	if len(updated.Evidence) != 1 || updated.Evidence[0] != "evidence-1" {
		t.Fatalf("evidence = %#v, want deduped single entry", updated.Evidence)
	}
	if _, err := m.Update(goal.ID, "", "", []string{"   "}, false, false, true); ErrorCode(err) != "goal_invalid_argument" {
		t.Fatalf("blank evidence error = %v, want goal_invalid_argument", err)
	}

	completed, err := m.Update(goal.ID, StatusComplete, "已验收", nil, true, true, false)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.Status != StatusComplete {
		t.Fatalf("status = %q, want complete", completed.Status)
	}
	if _, err := m.Update(goal.ID, StatusActive, "", nil, true, false, false); ErrorCode(err) != "goal_terminal" {
		t.Fatalf("complete->active error = %v, want goal_terminal", err)
	}
	if _, err := m.Update(goal.ID, "", "", nil, false, false, false); ErrorCode(err) != "goal_invalid_argument" {
		t.Fatalf("empty update error = %v, want goal_invalid_argument", err)
	}

	second := mustCreate(t, m, "阻塞目标", "")
	blocked, err := m.Update(second.ID, StatusBlocked, "缺少外部凭据", nil, true, true, false)
	if err != nil {
		t.Fatalf("block: %v", err)
	}
	if blocked.Status != StatusBlocked {
		t.Fatalf("status = %q, want blocked", blocked.Status)
	}
	reopened, err := m.Update(second.ID, StatusActive, "", nil, true, false, false)
	if err != nil {
		t.Fatalf("reopen blocked: %v", err)
	}
	if reopened.Status != StatusActive {
		t.Fatalf("status = %q, want active", reopened.Status)
	}
	if _, err := m.Update(second.ID, "", "", nil, false, false, false); ErrorCode(err) != "goal_invalid_argument" {
		t.Fatalf("no-op update error = %v, want goal_invalid_argument", err)
	}
	if _, err := m.Update(second.ID, "done", "", nil, true, false, false); ErrorCode(err) != "goal_invalid_status" {
		t.Fatalf("invalid status error = %v, want goal_invalid_status", err)
	}
	if _, err := m.Update("goal-000001-deadbeef", StatusComplete, "x", nil, true, true, false); ErrorCode(err) != "goal_not_found" {
		t.Fatalf("unknown id error = %v, want goal_not_found", err)
	}
	if _, err := m.Update("../escape", StatusComplete, "x", nil, true, true, false); ErrorCode(err) != "goal_invalid_id" {
		t.Fatalf("path escape error = %v, want goal_invalid_id", err)
	}
}

func TestUpdatePreservesCreatedAtAndMovesUpdatedAt(t *testing.T) {
	m, _ := mustManager(t)
	goal := mustCreate(t, m, "目标", "")
	created, err := time.Parse(time.RFC3339Nano, goal.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	updated, err := m.Update(goal.ID, "", "note", nil, false, true, false)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.CreatedAt != goal.CreatedAt {
		t.Fatalf("created_at changed: %q -> %q", goal.CreatedAt, updated.CreatedAt)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updated.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !updatedAt.After(created) {
		t.Fatalf("updated_at %v not after created_at %v", updatedAt, created)
	}
}

func TestLoadAllSortsAndSkipsCorruptFiles(t *testing.T) {
	m, _ := mustManager(t)
	older := mustCreate(t, m, "older", "")
	newer := mustCreate(t, m, "newer", "")
	if _, err := m.Update(newer.ID, "", "latest", nil, false, true, false); err != nil {
		t.Fatalf("update newer: %v", err)
	}

	dir := m.Dir()
	if err := os.WriteFile(filepath.Join(dir, "goal-999999-deadbeef.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-a-goal.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "goal-999998-0badc0de.json"),
		[]byte(`{"id":"goal-999997-0badc0de","objective":"x","status":"active","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","evidence":[]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	goals, skipped := m.LoadAll()
	if len(goals) != 2 {
		t.Fatalf("LoadAll goals = %d (%v), want 2", len(goals), goals)
	}
	if goals[0].ID != newer.ID || goals[1].ID != older.ID {
		t.Fatalf("LoadAll order = [%s %s], want [%s %s]", goals[0].ID, goals[1].ID, newer.ID, older.ID)
	}
	if len(skipped) != 3 {
		t.Fatalf("skipped = %#v, want 3 diagnostics", skipped)
	}
	for _, item := range skipped {
		if item.File == "" || item.Reason == "" {
			t.Fatalf("incomplete diagnostic: %#v", item)
		}
	}
	if _, err := m.Get("goal-999999-deadbeef"); ErrorCode(err) != "goal_corrupt" {
		t.Fatalf("Get corrupt error = %v, want goal_corrupt", err)
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	m, _ := mustManager(t)
	goal := mustCreate(t, m, "目标", "")
	if _, err := m.Update(goal.ID, "", "note", []string{"e"}, false, true, true); err != nil {
		t.Fatalf("update: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(m.Dir(), ".goal-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
	raw, err := os.ReadFile(filepath.Join(m.Dir(), goal.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Goal
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("atomic result is not valid JSON: %v", err)
	}
	if decoded.Note != "note" || len(decoded.Evidence) != 1 {
		t.Fatalf("persisted goal = %+v", decoded)
	}
}

func TestDirectoryPermissionTightened(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "state", "goals"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(ws)
	mustCreate(t, m, "objective", "")
	info, err := os.Stat(m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir perm = %o, want 0700", perm)
	}
}

func TestConcurrentAccess(t *testing.T) {
	m, _ := mustManager(t)
	const workers = 16
	const updates = 5

	var wg sync.WaitGroup
	ids := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			goal, err := m.Create("concurrent objective", "")
			if err != nil {
				t.Errorf("concurrent Create: %v", err)
				return
			}
			ids <- goal.ID
			for j := 0; j < updates; j++ {
				id := goal.ID
				evidence := []string{fmt.Sprintf("evidence-%d", j)}
				if _, err := m.Update(id, "", "", evidence, false, false, true); err != nil {
					t.Errorf("concurrent Update: %v", err)
					return
				}
				if _, err := m.Get(goal.ID); err != nil {
					t.Errorf("concurrent Get: %v", err)
					return
				}
				_, _ = m.LoadAll()
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, workers)
	for id := range ids {
		seen[id] = struct{}{}
	}
	if len(seen) != workers {
		t.Fatalf("unique ids = %d, want %d", len(seen), workers)
	}
	goals, skipped := m.LoadAll()
	if len(skipped) != 0 {
		t.Fatalf("skipped = %#v, want none", skipped)
	}
	if len(goals) != workers {
		t.Fatalf("goals = %d, want %d", len(goals), workers)
	}
	for _, goal := range goals {
		if len(goal.Evidence) != updates {
			t.Fatalf("goal %s evidence = %d, want %d", goal.ID, len(goal.Evidence), updates)
		}
	}
}

func TestNilManagerMethods(t *testing.T) {
	var m *Manager
	if m.Dir() != "" || m.Workspace() != "" {
		t.Fatalf("nil manager Dir/Workspace should be empty")
	}
	if _, err := m.Create("x", ""); ErrorCode(err) != "goal_no_manager" {
		t.Fatalf("Create error = %v", err)
	}
	if _, err := m.Get("goal-000001-deadbeef"); ErrorCode(err) != "goal_no_manager" {
		t.Fatalf("Get error = %v", err)
	}
	if _, err := m.Update("goal-000001-deadbeef", "", "", nil, false, false, false); ErrorCode(err) != "goal_no_manager" {
		t.Fatalf("Update error = %v", err)
	}
	if _, skipped := m.LoadAll(); len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "未初始化") {
		t.Fatalf("LoadAll diagnostics = %#v", skipped)
	}
}

func TestSummariesProjection(t *testing.T) {
	goals := []Goal{
		{ID: "goal-000002-bbbbbbbb", Objective: "b", Status: StatusBlocked, UpdatedAt: "2026-01-02T00:00:00Z", Evidence: []string{"x", "y"}, Note: "n"},
		{ID: "goal-000001-aaaaaaaa", Objective: "a", Status: StatusActive, UpdatedAt: "2026-01-01T00:00:00Z"},
	}
	summaries := Summaries(goals)
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d", len(summaries))
	}
	if summaries[0].ID != goals[0].ID || summaries[0].EvidenceCount != 2 || summaries[0].Note != "n" {
		t.Fatalf("summary[0] = %+v", summaries[0])
	}
	if summaries[1].EvidenceCount != 0 {
		t.Fatalf("summary[1].EvidenceCount = %d, want 0", summaries[1].EvidenceCount)
	}
}

func TestWorkspaceContainment(t *testing.T) {
	m, ws := mustManager(t)
	goal := mustCreate(t, m, "objective", "")
	expected := filepath.Join(ws, "state", "goals", goal.ID+".json")
	path, err := m.goalPath(goal.ID)
	if err != nil {
		t.Fatalf("goalPath: %v", err)
	}
	if path != expected {
		t.Fatalf("goalPath = %q, want %q", path, expected)
	}
	for _, bad := range []string{"", "..", "../x", "goal-000001-../../evil", "goal-1-zzzzzzzz"} {
		if _, err := m.goalPath(bad); err == nil {
			t.Fatalf("goalPath(%q) accepted an invalid id", bad)
		}
	}
}
