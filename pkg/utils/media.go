package utils

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/media"
)

var audioExtensions = []string{".mp3", ".wav", ".ogg", ".m4a", ".flac", ".aac", ".wma"}

func AudioFormat(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	for _, supportedExt := range audioExtensions {
		if ext == supportedExt {
			return strings.TrimPrefix(ext, "."), nil
		}
	}

	return "", fmt.Errorf("unsupported audio format for %q", path)
}

// IsAudioFile 根据文件名扩展名和内容类型判断文件是否为音频文件。
func IsAudioFile(filename, contentType string) bool {
	audioTypes := []string{"audio/", "application/ogg", "application/x-ogg"}

	for _, ext := range audioExtensions {
		if strings.HasSuffix(strings.ToLower(filename), ext) {
			return true
		}
	}

	for _, audioType := range audioTypes {
		if strings.HasPrefix(strings.ToLower(contentType), audioType) {
			return true
		}
	}

	return false
}

// SanitizeFilename 移除文件名中可能危险的字符，
// 并返回适合本地文件系统存储的安全版本。
func SanitizeFilename(filename string) string {
	// 获取不含路径的基文件名
	base := filepath.Base(filename)

	// 移除任何目录穿越尝试
	base = strings.ReplaceAll(base, "..", "")
	base = strings.ReplaceAll(base, "/", "_")
	base = strings.ReplaceAll(base, "\\", "_")

	return base
}

// DownloadOptions 保存下载文件的可选参数
type DownloadOptions struct {
	Timeout             time.Duration
	ExtraHeaders        map[string]string
	LoggerPrefix        string
	ProxyURL            string
	BlockPrivateTargets bool
}

// DownloadFile 从 URL 下载文件到本地临时目录。
// 返回本地文件路径；出错时返回空字符串。
func DownloadFile(urlStr, filename string, opts DownloadOptions) string {
	// 设置默认值
	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.LoggerPrefix == "" {
		opts.LoggerPrefix = "utils"
	}

	mediaDir := media.TempDir()
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		logger.ErrorCF(opts.LoggerPrefix, "Failed to create media directory", map[string]any{
			"error": err.Error(),
		})
		return ""
	}

	// 使用 UUID 前缀生成唯一文件名，避免冲突
	safeName := SanitizeFilename(filename)
	localPath := filepath.Join(mediaDir, uuid.New().String()[:8]+"_"+safeName)

	var client *http.Client
	var err error
	if opts.BlockPrivateTargets {
		validateErr := ValidateSafeHTTPURL(urlStr, nil, nil)
		if validateErr != nil {
			logger.ErrorCF(opts.LoggerPrefix, "Blocked unsafe download URL", map[string]any{
				"error": validateErr.Error(),
				"url":   urlStr,
			})
			return ""
		}
		client, err = CreateSafeHTTPClient(SafeHTTPClientOptions{
			ProxyURL:     opts.ProxyURL,
			Timeout:      opts.Timeout,
			MaxRedirects: 10,
		})
		if err != nil {
			logger.ErrorCF(opts.LoggerPrefix, "Failed to create safe download client", map[string]any{
				"error": err.Error(),
			})
			return ""
		}
	} else {
		client = &http.Client{Timeout: opts.Timeout}
		if opts.ProxyURL != "" {
			proxyURL, parseErr := url.Parse(opts.ProxyURL)
			if parseErr != nil {
				logger.ErrorCF(opts.LoggerPrefix, "Invalid proxy URL for download", map[string]any{
					"error": parseErr.Error(),
					"proxy": opts.ProxyURL,
				})
				return ""
			}
			client.Transport = &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			}
		}
	}

	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
	if err != nil {
		logger.ErrorCF(opts.LoggerPrefix, "Failed to create download request", map[string]any{
			"error": err.Error(),
		})
		return ""
	}

	// 添加额外请求头（例如 Slack 所需的 Authorization）
	for key, value := range opts.ExtraHeaders {
		req.Header.Set(key, value)
	}
	if opts.BlockPrivateTargets {
		AllowConfiguredProxyFirstHop(req, client.Transport)
	}

	resp, err := client.Do(req)
	if err != nil {
		logger.ErrorCF(opts.LoggerPrefix, "Failed to download file", map[string]any{
			"error": err.Error(),
			"url":   urlStr,
		})
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.ErrorCF(opts.LoggerPrefix, "File download returned non-200 status", map[string]any{
			"status": resp.StatusCode,
			"url":    urlStr,
		})
		return ""
	}

	out, err := os.Create(localPath)
	if err != nil {
		logger.ErrorCF(opts.LoggerPrefix, "Failed to create local file", map[string]any{
			"error": err.Error(),
		})
		return ""
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		os.Remove(localPath)
		logger.ErrorCF(opts.LoggerPrefix, "Failed to write file", map[string]any{
			"error": err.Error(),
		})
		return ""
	}
	if err := out.Close(); err != nil {
		logger.ErrorCF(opts.LoggerPrefix, "Failed to close downloaded file", map[string]any{
			"error": err.Error(),
		})
		os.Remove(localPath)
		return ""
	}

	logger.DebugCF(opts.LoggerPrefix, "File downloaded successfully", map[string]any{
		"path": localPath,
	})

	return localPath
}

// DownloadFileSimple 是 DownloadFile 的简化版本，不接受选项参数
func DownloadFileSimple(url, filename string) string {
	return DownloadFile(url, filename, DownloadOptions{
		LoggerPrefix: "media",
	})
}
