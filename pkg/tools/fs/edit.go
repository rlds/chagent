package fstools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

// EditFileTool 通过把 old_text 替换为 new_text 来编辑文件。
// old_text 必须与文件中的内容完全一致。
type EditFileTool struct {
	fs fileSystem
}

// NewEditFileTool 创建一个带可选目录限制的新 EditFileTool。
func NewEditFileTool(workspace string, restrict bool, allowPaths ...[]*regexp.Regexp) *EditFileTool {
	var patterns []*regexp.Regexp
	if len(allowPaths) > 0 {
		patterns = allowPaths[0]
	}
	return &EditFileTool{fs: buildFs(workspace, restrict, patterns)}
}

func (t *EditFileTool) Name() string {
	return "edit_file"
}

func (t *EditFileTool) Description() string {
	return "编辑文件，把 old_text 替换为 new_text。old_text 必须与文件中的内容完全一致。遵循标准 JSON 转义：\\n 表示换行，\\\\n 表示字面量反斜杠加 n。"
}

func (t *EditFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "要编辑的文件路径",
			},
			"old_text": map[string]any{
				"type":        "string",
				"description": "要查找并替换的精确文本。遵循标准 JSON 转义：\\n 表示换行，\\\\n 表示字面量反斜杠加 n。",
			},
			"new_text": map[string]any{
				"type":        "string",
				"description": "替换后的文本。遵循标准 JSON 转义：\\n 表示换行，\\\\n 表示字面量反斜杠加 n。",
			},
		},
		"required": []string{"path", "old_text", "new_text"},
	}
}

func (t *EditFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		return ErrorResult("path 参数必填")
	}

	oldText, ok := args["old_text"].(string)
	if !ok {
		return ErrorResult("old_text 参数必填")
	}

	newText, ok := args["new_text"].(string)
	if !ok {
		return ErrorResult("new_text 参数必填")
	}

	beforeContent, afterContent, err := editFile(t.fs, path, oldText, newText)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return DiffResult(path, beforeContent, afterContent)
}

type AppendFileTool struct {
	fs fileSystem
}

func NewAppendFileTool(workspace string, restrict bool, allowPaths ...[]*regexp.Regexp) *AppendFileTool {
	var patterns []*regexp.Regexp
	if len(allowPaths) > 0 {
		patterns = allowPaths[0]
	}
	return &AppendFileTool{fs: buildFs(workspace, restrict, patterns)}
}

func (t *AppendFileTool) Name() string {
	return "append_file"
}

func (t *AppendFileTool) Description() string {
	return "把内容追加到文件末尾。遵循标准 JSON 转义：\\n 表示换行，\\\\n 表示字面量反斜杠加 n。"
}

func (t *AppendFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "要追加内容的文件路径",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "要追加的内容。遵循标准 JSON 转义：\\n 表示换行，\\\\n 表示字面量反斜杠加 n。",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t *AppendFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		return ErrorResult("path 参数必填")
	}

	content, ok := args["content"].(string)
	if !ok {
		return ErrorResult("content 参数必填")
	}

	if err := appendFile(t.fs, path, content); err != nil {
		return ErrorResult(err.Error())
	}
	return SilentResult(fmt.Sprintf("已追加到 %s", path))
}

// editFile 通过 sysFs 读取文件，执行替换，然后写回。
// 它使用 fileSystem 接口，使同一套逻辑既可运行于受限模式也可运行于非受限模式。
func editFile(sysFs fileSystem, path, oldText, newText string) ([]byte, []byte, error) {
	content, err := sysFs.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	newContent, err := replaceEditContent(content, oldText, newText)
	if err != nil {
		return nil, nil, err
	}

	if err := sysFs.WriteFile(path, newContent); err != nil {
		return nil, nil, err
	}

	return content, newContent, nil
}

// appendFile 通过 sysFs 读取已有内容（如果存在），追加新内容，然后写回。
func appendFile(sysFs fileSystem, path, appendContent string) error {
	content, err := sysFs.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	newContent := append(content, []byte(appendContent)...)
	return sysFs.WriteFile(path, newContent)
}

// replaceEditContent 处理查找并替换 oldText 单次出现的核心逻辑。
func replaceEditContent(content []byte, oldText, newText string) ([]byte, error) {
	contentStr := string(content)

	if !strings.Contains(contentStr, oldText) {
		return nil, fmt.Errorf("文件中未找到 old_text。请确保完全匹配。")
	}

	count := strings.Count(contentStr, oldText)
	if count > 1 {
		return nil, fmt.Errorf("old_text 出现了 %d 次。请提供更多上下文使其唯一。", count)
	}

	newContent := strings.Replace(contentStr, oldText, newText, 1)
	return []byte(newContent), nil
}
