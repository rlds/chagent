package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/providers"
)

// jsonSession 为迁移用途镜像 pkg/session.Session。
type jsonSession struct {
	Key      string              `json:"key"`
	Messages []providers.Message `json:"messages"`
	Summary  string              `json:"summary,omitempty"`
	Created  time.Time           `json:"created"`
	Updated  time.Time           `json:"updated"`
}

// MigrateFromJSON 从 sessionsDir 读取遗留的 sessions/*.json 文件，
// 将其写入 Store，并把每个已迁移的文件重命名为
// .json.migrated 作为备份。返回已迁移的会话数量。
//
// 解析失败的文件会被记录日志并跳过。已迁移的文件
// (.json.migrated) 会被忽略，因此该函数是幂等的。
func MigrateFromJSON(
	ctx context.Context, sessionsDir string, store Store,
) (int, error) {
	entries, err := os.ReadDir(sessionsDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("memory: read sessions dir: %w", err)
	}

	migrated := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		// 跳过 JSONL 元数据文件。它们属于新的存储格式，
		// 而不是遗留的会话快照，重新导入它们会用空消息列表
		// 覆盖与之配对的 .jsonl 历史记录。
		if strings.HasSuffix(name, ".meta.json") {
			continue
		}
		// 跳过已迁移的文件。
		if strings.HasSuffix(name, ".migrated") {
			continue
		}

		srcPath := filepath.Join(sessionsDir, name)

		data, readErr := os.ReadFile(srcPath)
		if readErr != nil {
			log.Printf("memory: migrate: skip %s: %v", name, readErr)
			continue
		}

		var sess jsonSession
		if parseErr := json.Unmarshal(data, &sess); parseErr != nil {
			log.Printf("memory: migrate: skip %s: %v", name, parseErr)
			continue
		}

		// 使用 JSON 内容中的 key，而不是文件名。
		// 文件名会被净化（":" → "_"），但 key 不会。
		key := sess.Key
		if key == "" {
			key = strings.TrimSuffix(name, ".json")
		}

		// 使用 SetHistory（原子替换）而不是逐条消息调用
		// AddFullMessage。这使迁移具备幂等性：如果进程
		// 在写入消息之后、执行下面的重命名之前崩溃，
		// 重试会干净地替换掉部分数据，
		// 而不会重复写入消息。
		if setErr := store.SetHistory(ctx, key, sess.Messages); setErr != nil {
			return migrated, fmt.Errorf(
				"memory: migrate %s: set history: %w",
				name, setErr,
			)
		}

		if sess.Summary != "" {
			if sumErr := store.SetSummary(ctx, key, sess.Summary); sumErr != nil {
				return migrated, fmt.Errorf(
					"memory: migrate %s: set summary: %w",
					name, sumErr,
				)
			}
		}

		// 重命名为 .migrated 作为备份（而不是删除）。
		renameErr := os.Rename(srcPath, srcPath+".migrated")
		if renameErr != nil {
			log.Printf("memory: migrate: rename %s: %v", name, renameErr)
		}

		migrated++
	}

	return migrated, nil
}
