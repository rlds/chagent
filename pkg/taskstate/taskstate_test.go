package taskstate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testNow() time.Time {
	return time.Date(2026, 10, 28, 16, 10, 0, 0, time.FixedZone("CST", 8*3600))
}

func newTestState(t *testing.T) State {
	t.Helper()
	state, err := New("重构 provider 层并保持所有测试通过", DefaultMaxRounds, testNow())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return state
}

func TestTaskStateJSONRoundTrip(t *testing.T) {
	state := newTestState(t)
	state.Todos = []Todo{
		{Content: "盘点现有 provider 接口", Status: TodoCompleted},
		{Content: "引入公共协议类型", Status: TodoInProgress},
	}
	state.Round = 2
	state.Evidence = []string{"已新增 `pkg/providers/protocoltypes/types.go`"}
	state.NoProgressRounds = 1
	state.BlockerRounds = 2
	state.BlockerFingerprint = "abc123"
	state.BlockerLastRound = 2

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded State
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("Validate after round trip: %v", err)
	}
	if decoded.Objective != state.Objective ||
		decoded.Phase != PhaseActive ||
		decoded.Round != 2 ||
		decoded.MaxRounds != DefaultMaxRounds ||
		len(decoded.Todos) != 2 ||
		decoded.Todos[1].Status != TodoInProgress ||
		len(decoded.Evidence) != 1 ||
		decoded.BlockerRounds != 2 ||
		decoded.BlockerFingerprint != "abc123" {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
	if !decoded.LastProgressAt.Equal(state.LastProgressAt) {
		t.Fatalf("LastProgressAt = %v, want %v", decoded.LastProgressAt, state.LastProgressAt)
	}
	// JSON field names are part of the stable contract.
	for _, field := range []string{
		`"version"`, `"objective"`, `"phase"`, `"round"`, `"max_rounds"`,
		`"last_progress_at"`, `"no_progress_rounds"`, `"blocker_fingerprint"`,
		`"blocker_rounds"`, `"evidence"`, `"updated_at"`,
	} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("serialized state missing %s: %s", field, data)
		}
	}
}

func TestTaskStateValidation(t *testing.T) {
	state := newTestState(t)

	cases := []struct {
		name   string
		mutate func(*State)
	}{
		{"empty objective", func(s *State) { s.Objective = "  " }},
		{"invalid phase", func(s *State) { s.Phase = "running" }},
		{"max rounds zero", func(s *State) { s.MaxRounds = 0 }},
		{"max rounds too large", func(s *State) { s.MaxRounds = MaxRoundsLimit + 1 }},
		{"negative round", func(s *State) { s.Round = -1 }},
		{"todo empty content", func(s *State) { s.Todos = []Todo{{Content: " ", Status: TodoPending}} }},
		{"todo invalid status", func(s *State) { s.Todos = []Todo{{Content: "a", Status: "done"}} }},
		{"todo duplicate content", func(s *State) {
			s.Todos = []Todo{
				{Content: "重复", Status: TodoPending},
				{Content: " 重复 ", Status: TodoInProgress},
			}
		}},
		{"blocked without reason", func(s *State) { s.Phase = PhaseBlocked }},
		{"reason outside blocked", func(s *State) {
			s.BlockedReason = &BlockedReason{Code: "model-reported", Message: "x"}
		}},
		{"empty evidence", func(s *State) { s.Evidence = []string{"ok", " "} }},
		{"version too new", func(s *State) { s.Version = StateVersion + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := state.Clone()
			tc.mutate(&mutated)
			if err := mutated.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
		})
	}

	// A valid blocked state passes.
	blocked := newTestState(t)
	blocked.Phase = PhaseBlocked
	blocked.BlockedReason = &BlockedReason{Code: "no-progress", Message: "连续 3 轮没有进展"}
	if err := blocked.Validate(); err != nil {
		t.Fatalf("valid blocked state rejected: %v", err)
	}

	// Parallel in-progress is allowed at the storage layer but rejected by the
	// stricter product validation used by todo_write.
	parallel := newTestState(t)
	parallel.Todos = []Todo{
		{Content: "a", Status: TodoInProgress},
		{Content: "b", Status: TodoInProgress},
	}
	if err := parallel.Validate(); err != nil {
		t.Fatalf("storage validation should allow parallel in_progress: %v", err)
	}
	if err := parallel.ValidateWith(ValidationOptions{AllowParallelInProgress: false}); err == nil {
		t.Fatalf("strict validation should reject two in_progress todos")
	}
}

func TestTaskStatePhaseTransitions(t *testing.T) {
	now := testNow()

	if CanTransition("", PhaseActive) != true {
		t.Fatal("start should be allowed")
	}
	for _, tc := range []struct {
		from, to Phase
		want     bool
	}{
		{PhaseActive, PhasePaused, true},
		{PhaseActive, PhaseBlocked, true},
		{PhaseActive, PhaseComplete, true},
		{PhaseActive, PhaseActive, false},
		{PhasePaused, PhaseActive, true},
		{PhasePaused, PhaseComplete, false},
		{PhaseBlocked, PhaseActive, true},
		{PhaseBlocked, PhaseComplete, false},
		{PhaseComplete, PhaseActive, false},
		{PhaseComplete, PhasePaused, false},
	} {
		if got := CanTransition(tc.from, tc.to); got != tc.want {
			t.Fatalf("CanTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}

	state := newTestState(t)
	if err := state.Pause(now); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if state.Phase != PhasePaused {
		t.Fatalf("Phase = %s, want paused", state.Phase)
	}
	if err := state.Pause(now); err == nil {
		t.Fatal("pausing twice should fail")
	}
	if err := state.Resume(now); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if state.Phase != PhaseActive {
		t.Fatalf("Phase = %s, want active", state.Phase)
	}
	if err := state.Complete([]string{"go test ./pkg/... 通过"}, now); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if state.Phase != PhaseComplete {
		t.Fatalf("Phase = %s, want complete", state.Phase)
	}
	if err := state.Resume(now); err == nil {
		t.Fatal("resuming a complete task should fail")
	}
	if err := state.Pause(now); err == nil {
		t.Fatal("pausing a complete task should fail")
	}
	if err := state.Complete([]string{"again"}, now); err == nil {
		t.Fatal("completing twice should fail")
	}
}

func TestTaskStateCompleteRequiresEvidence(t *testing.T) {
	state := newTestState(t)
	if err := state.Complete(nil, testNow()); err == nil {
		t.Fatal("Complete(nil) should fail")
	}
	if err := state.Complete([]string{"  ", ""}, testNow()); err == nil {
		t.Fatal("Complete with blank evidence should fail")
	}
	state.Phase = PhasePaused
	if err := state.Complete([]string{"evidence"}, testNow()); err == nil {
		t.Fatal("Complete from paused should fail")
	}
}

func TestTaskStateBlockedThreshold(t *testing.T) {
	now := testNow()
	state := newTestState(t)
	reason := BlockedReason{Code: "model-reported", Message: "缺少 chess 服务端 token"}

	// A different condition does not satisfy the threshold.
	if state.CanBlock(reason, DefaultBlockedThreshold) {
		t.Fatal("CanBlock should be false before any attempt")
	}
	if err := state.Block(reason, DefaultBlockedThreshold, now); err == nil {
		t.Fatal("Block before threshold should fail")
	}

	// Round 1: first observation.
	state.Round = 1
	allowed, rounds, err := state.RecordBlockerAttempt(reason, DefaultBlockedThreshold, now)
	if err != nil {
		t.Fatalf("RecordBlockerAttempt: %v", err)
	}
	if allowed || rounds != 1 {
		t.Fatalf("round 1: allowed=%v rounds=%d, want false/1", allowed, rounds)
	}

	// Repeating the same attempt in the same round must not advance the count.
	allowed, rounds, _ = state.RecordBlockerAttempt(reason, DefaultBlockedThreshold, now)
	if allowed || rounds != 1 {
		t.Fatalf("same round repeat: allowed=%v rounds=%d, want false/1", allowed, rounds)
	}

	// Round 2: same condition.
	state.Round = 2
	allowed, rounds, _ = state.RecordBlockerAttempt(reason, DefaultBlockedThreshold, now)
	if allowed || rounds != 2 {
		t.Fatalf("round 2: allowed=%v rounds=%d, want false/2", allowed, rounds)
	}

	// Round 3: threshold met.
	state.Round = 3
	allowed, rounds, _ = state.RecordBlockerAttempt(reason, DefaultBlockedThreshold, now)
	if !allowed || rounds != 3 {
		t.Fatalf("round 3: allowed=%v rounds=%d, want true/3", allowed, rounds)
	}
	if !state.CanBlock(reason, DefaultBlockedThreshold) {
		t.Fatal("CanBlock should be true after 3 rounds")
	}
	if err := state.Block(reason, DefaultBlockedThreshold, now); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if state.Phase != PhaseBlocked || state.BlockedReason == nil {
		t.Fatalf("state not blocked: %+v", state)
	}
	if state.BlockedReason.Code != "model-reported" || state.BlockedReason.Message != reason.Message {
		t.Fatalf("blocked reason = %+v", state.BlockedReason)
	}

	// A different condition resets the counter.
	fresh := newTestState(t)
	fresh.Round = 1
	if _, _, err := fresh.RecordBlockerAttempt(reason, DefaultBlockedThreshold, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	other := BlockedReason{Code: "model-reported", Message: "另一个条件"}
	if allowed, rounds, _ := fresh.RecordBlockerAttempt(other, DefaultBlockedThreshold, now); allowed || rounds != 1 {
		t.Fatalf("different condition: allowed=%v rounds=%d, want false/1", allowed, rounds)
	}

	// Progress clears the pending blocker.
	fresh.MarkProgress(now)
	if fresh.BlockerRounds != 0 || fresh.BlockerFingerprint != "" {
		t.Fatalf("MarkProgress should reset blocker tracking: %+v", fresh)
	}
}

func TestTaskStateFingerprintStable(t *testing.T) {
	a := BlockedReason{Code: "model-reported", Message: "缺少   chess   服务端 token "}
	b := BlockedReason{Code: "model-reported", Message: "缺少 chess 服务端 token"}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("fingerprint should normalize whitespace")
	}
	c := BlockedReason{Code: "model-reported", Message: "缺少 chess 服务端密钥"}
	if a.Fingerprint() == c.Fingerprint() {
		t.Fatal("different messages must have different fingerprints")
	}
	if !IsValidCode(a.Code) {
		t.Fatal("model-reported should be a valid code")
	}
}

func TestTaskStateProgressAndRounds(t *testing.T) {
	now := testNow()
	state := newTestState(t)
	state.Round = 0

	for i := 1; i <= DefaultNoProgressRounds; i++ {
		if got := state.RecordNoProgressRound(now); got != i {
			t.Fatalf("RecordNoProgressRound = %d, want %d", got, i)
		}
	}
	state.MarkProgress(now)
	if state.NoProgressRounds != 0 {
		t.Fatalf("NoProgressRounds = %d, want 0", state.NoProgressRounds)
	}
	if !state.LastProgressAt.Equal(now) {
		t.Fatalf("LastProgressAt = %v, want %v", state.LastProgressAt, now)
	}

	if state.RoundLimitReached() {
		t.Fatal("RoundLimitReached should be false at round 0")
	}
	for state.Round < state.MaxRounds {
		if err := state.AdvanceRound(now); err != nil {
			t.Fatalf("AdvanceRound: %v", err)
		}
	}
	if !state.RoundLimitReached() {
		t.Fatal("RoundLimitReached should be true at max")
	}
	if state.ShouldContinue() {
		t.Fatal("ShouldContinue should be false at max")
	}
	if err := state.AdvanceRound(now); err == nil {
		t.Fatal("AdvanceRound beyond max should fail")
	}
	if err := state.SetRoundLimitBlocked(now); err != nil {
		t.Fatalf("SetRoundLimitBlocked: %v", err)
	}
	if state.Phase != PhaseBlocked || state.BlockedReason.Code != "round-limit" {
		t.Fatalf("round limit block = %+v", state.BlockedReason)
	}
}

func TestTaskStateNoProgressAndForceBlock(t *testing.T) {
	now := testNow()
	state := newTestState(t)
	state.RecordNoProgressRound(now)
	state.RecordNoProgressRound(now)
	state.RecordNoProgressRound(now)
	if err := state.SetNoProgressBlocked(now); err != nil {
		t.Fatalf("SetNoProgressBlocked: %v", err)
	}
	if state.BlockedReason == nil || state.BlockedReason.Code != "no-progress" {
		t.Fatalf("blocked reason = %+v", state.BlockedReason)
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("blocked state should validate: %v", err)
	}
}

func TestTaskStateNewValidation(t *testing.T) {
	if _, err := New("", 0, testNow()); err == nil {
		t.Fatal("empty objective should fail")
	}
	if _, err := New("目标", MaxRoundsLimit+1, testNow()); err == nil {
		t.Fatal("max rounds above limit should fail")
	}
	state, err := New("目标", 0, testNow())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if state.MaxRounds != DefaultMaxRounds {
		t.Fatalf("MaxRounds = %d, want %d", state.MaxRounds, DefaultMaxRounds)
	}
	if state.Version != StateVersion {
		t.Fatalf("Version = %d, want %d", state.Version, StateVersion)
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("fresh state should validate: %v", err)
	}
}

// fakeStore is a tiny in-memory Store used to test Load/Save helpers.
type fakeStore struct {
	states map[string]State
	err    error
}

func (f *fakeStore) GetTaskState(sessionKey string) (State, bool) {
	state, ok := f.states[sessionKey]
	return state, ok
}

func (f *fakeStore) SetTaskState(sessionKey string, state State) error {
	if f.err != nil {
		return f.err
	}
	f.states[sessionKey] = state
	return nil
}

func TestTaskStateStoreHelpers(t *testing.T) {
	store := &fakeStore{states: map[string]State{}}
	if _, ok := Load(store, "s1"); ok {
		t.Fatal("Load on empty store should return false")
	}
	if _, ok := Load(nil, "s1"); ok {
		t.Fatal("Load(nil) should return false")
	}
	state := newTestState(t)
	state.Todos = []Todo{{Content: "a", Status: TodoPending}}
	if err := Save(store, "s1", state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, ok := Load(store, "s1")
	if !ok {
		t.Fatal("Load after Save should return true")
	}
	// Load must return a deep copy.
	loaded.Todos[0].Content = "mutated"
	if store.states["s1"].Todos[0].Content != "a" {
		t.Fatal("Load must deep copy the state")
	}
	if err := Save(store, "", state); err == nil {
		t.Fatal("Save without session key should fail")
	}
	if err := Save(nil, "s1", state); err == nil {
		t.Fatal("Save(nil) should fail")
	}
	invalid := state.Clone()
	invalid.Objective = ""
	if err := Save(store, "s2", invalid); err == nil {
		t.Fatal("Save should validate the state")
	}
}
