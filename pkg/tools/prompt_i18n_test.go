package tools

import (
	"strings"
	"testing"
	"unicode"
)

func toolTextContainsHan(t *testing.T, label, text string) {
	t.Helper()
	if !strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
		t.Fatalf("%s is not Chinese: %q", label, text)
	}
}

// collectParameterDescriptions walks a JSON-schema-ish map and returns every
// model-visible "description" string.
func collectParameterDescriptions(value any) []string {
	var descriptions []string
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "description" {
				if text, ok := child.(string); ok {
					descriptions = append(descriptions, text)
				}
				continue
			}
			descriptions = append(descriptions, collectParameterDescriptions(child)...)
		}
	case []any:
		for _, child := range typed {
			descriptions = append(descriptions, collectParameterDescriptions(child)...)
		}
	}
	return descriptions
}

// TestDefaultChessToolPromptsAreChinese verifies that the model-visible
// Description() and Parameters() descriptions of the chess default tools are
// translated to Chinese (T4.6). Identifiers, tool names and JSON fields stay
// unchanged.
func TestDefaultChessToolPromptsAreChinese(t *testing.T) {
	workspace := t.TempDir()
	execTool, err := NewExecTool(workspace, false)
	if err != nil {
		t.Fatalf("NewExecTool() error = %v", err)
	}

	tools := []struct {
		name string
		tool Tool
	}{
		{"read_file", NewReadFileTool(workspace, false, 0)},
		{"read_file_lines", NewReadFileLinesTool(workspace, false, 0)},
		{"write_file", NewWriteFileTool(workspace, false, nil)},
		{"edit_file", NewEditFileTool(workspace, false)},
		{"append_file", NewAppendFileTool(workspace, false)},
		{"list_dir", NewListDirTool(workspace, false)},
		{"exec", execTool},
		{"find_skills", NewFindSkillsTool(nil, nil)},
		{"install_skill", NewInstallSkillTool(nil, workspace)},
		{"spawn", NewSpawnTool(nil)},
		{"spawn_status", NewSpawnStatusTool(nil)},
		{"subagent", NewSubagentTool(nil)},
		{"message", NewMessageTool()},
		{"tool_search_tool_regex", NewRegexSearchTool(nil, 0, 0)},
		{"tool_search_tool_bm25", NewBM25SearchTool(nil, 0, 0)},
		{"delegate", NewDelegateTool()},
	}

	for _, tt := range tools {
		t.Run(tt.name, func(t *testing.T) {
			description := tt.tool.Description()
			if strings.TrimSpace(description) == "" {
				t.Fatalf("%s Description() is empty", tt.name)
			}
			toolTextContainsHan(t, tt.name+" Description()", description)

			for _, paramDescription := range collectParameterDescriptions(tt.tool.Parameters()) {
				toolTextContainsHan(t, tt.name+" parameter description", paramDescription)
			}
		})
	}
}

// TestLegacyEnglishToolDescriptionsGone locks the specific legacy sentences
// that were translated so they do not silently regress.
func TestLegacyEnglishToolDescriptionsGone(t *testing.T) {
	workspace := t.TempDir()
	execTool, err := NewExecTool(workspace, false)
	if err != nil {
		t.Fatalf("NewExecTool() error = %v", err)
	}

	legacy := []struct {
		name    string
		tool    Tool
		snippet string
	}{
		{"read_file", NewReadFileTool(workspace, false, 0), "Read the contents of a file"},
		{"write_file", NewWriteFileTool(workspace, false, nil), "Write content to a file"},
		{"edit_file", NewEditFileTool(workspace, false), "Edit a file by replacing"},
		{"append_file", NewAppendFileTool(workspace, false), "Append content to the end"},
		{"list_dir", NewListDirTool(workspace, false), "List files and directories"},
		{"exec", execTool, "Execute shell commands"},
		{"find_skills", NewFindSkillsTool(nil, nil), "Search for installable skills"},
		{"install_skill", NewInstallSkillTool(nil, workspace), "Install a skill from a registry"},
		{"spawn", NewSpawnTool(nil), "Spawn a subagent to handle a task"},
		{"spawn_status", NewSpawnStatusTool(nil), "Get the status of spawned subagents"},
		{"subagent", NewSubagentTool(nil), "Execute a subagent task synchronously"},
		{"message", NewMessageTool(), "Send a text message to the user"},
		{"tool_search_tool_regex", NewRegexSearchTool(nil, 0, 0), "Search available hidden tools on-demand"},
		{"tool_search_tool_bm25", NewBM25SearchTool(nil, 0, 0), "Search available hidden tools on-demand"},
		{"delegate", NewDelegateTool(), "Delegate a task to another agent"},
	}

	for _, tt := range legacy {
		if strings.Contains(tt.tool.Description(), tt.snippet) {
			t.Fatalf("%s Description() still contains legacy English %q: %q", tt.name, tt.snippet, tt.tool.Description())
		}
	}
}
