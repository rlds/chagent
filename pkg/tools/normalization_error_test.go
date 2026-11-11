package tools

import (
	"strings"
	"testing"
)

// TestNormalizeToolResult_PreservesLargeErrorDiagnostics locks robustness B4:
// a huge but valid error message (e.g. a 50k-character invalid path) must reach
// the model so it can self-correct; it must not be replaced by the generic
// large-base64 omission note.
func TestNormalizeToolResult_PreservesLargeErrorDiagnostics(t *testing.T) {
	msg := `error: read_file: path escapes workspace: ` + strings.Repeat("a", 50_000)
	res := normalizeToolResult(&ToolResult{ForLLM: msg, IsError: true}, "read_file", nil, "", "")
	if res.ForLLM == largeBase64OmittedMessage {
		t.Fatalf("error diagnostics were replaced by the large-payload filter")
	}
	if !strings.Contains(res.ForLLM, "path escapes workspace") {
		t.Fatalf("error diagnostics lost: %.120q", res.ForLLM)
	}
}

// TestNormalizeToolResult_StillsOmitsLargeBase64Success ensures the heuristic
// is still applied to successful tool output.
func TestNormalizeToolResult_StillsOmitsLargeBase64Success(t *testing.T) {
	res := normalizeToolResult(&ToolResult{ForLLM: strings.Repeat("QUJD", 20_000)}, "some_tool", nil, "", "")
	if res.ForLLM != largeBase64OmittedMessage {
		t.Fatalf("large base64 success output = %.120q, want omitted note", res.ForLLM)
	}
}

// TestNormalizeToolResult_TruncatesHugeErrors keeps model context bounded.
func TestNormalizeToolResult_TruncatesHugeErrors(t *testing.T) {
	res := normalizeToolResult(&ToolResult{ForLLM: strings.Repeat("e", 100_000), IsError: true}, "t", nil, "", "")
	if len(res.ForLLM) > maxErrorLLMBytes+64 {
		t.Fatalf("error result length = %d, want <= %d", len(res.ForLLM), maxErrorLLMBytes+64)
	}
	if !strings.Contains(res.ForLLM, "error truncated") {
		t.Fatalf("truncated error missing marker: %.80q", res.ForLLM)
	}
}
