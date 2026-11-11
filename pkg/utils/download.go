package utils

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/rlds/chagent/pkg/logger"
)

// DownloadToFile 将 HTTP 响应体以小数据块（约 32KB）流式写入临时文件，
// 无论文件多大，峰值内存占用都保持恒定。
//
// 参数：
//   - ctx:      用于取消/超时的 context
//   - client:   要使用的 HTTP 客户端（超时、传输等由调用方控制）
//   - req:      完全准备好的 *http.Request（方法、URL、请求头等）
//   - maxBytes: 最大下载字节数；0 表示不限制
//
// 返回临时文件的路径。调用方负责在使用完毕后删除该文件
// （defer os.Remove(path)）。
//
// 发生任何错误时，临时文件都会被自动清理。
func DownloadToFile(ctx context.Context, client *http.Client, req *http.Request, maxBytes int64) (string, error) {
	// 附加 context。
	req = req.WithContext(ctx)

	logger.DebugCF("download", "Starting download", map[string]any{
		"url":       req.URL.String(),
		"max_bytes": maxBytes,
	})

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 读取少量内容用于错误信息。
		errBody := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, errBody)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(errBody[:n]))
	}

	// 创建临时文件。
	tmpFile, err := os.CreateTemp("", "chagent-dl-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	logger.DebugCF("download", "Streaming to temp file", map[string]any{
		"path": tmpPath,
	})

	// 清理辅助函数 —— 发生任何错误时删除临时文件。
	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	// 可选：限制下载大小。
	var src io.Reader = resp.Body
	if maxBytes > 0 {
		src = io.LimitReader(resp.Body, maxBytes+1) // +1 用于检测超限
	}

	written, err := io.Copy(tmpFile, src)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("download write failed: %w", err)
	}

	if maxBytes > 0 && written > maxBytes {
		cleanup()
		return "", fmt.Errorf("download too large: %d bytes (max %d)", written, maxBytes)
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}

	logger.DebugCF("download", "Download complete", map[string]any{
		"path":          tmpPath,
		"bytes_written": written,
	})

	return tmpPath, nil
}
