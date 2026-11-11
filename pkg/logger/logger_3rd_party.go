// 本文件用于兼容第三方日志库，Chagent 项目中不应调用

package logger

import (
	"fmt"
	"regexp"
)

// botTokenRe 匹配 Telegram 机器人令牌的机器人 ID 前缀和密钥部分。
// 分组：1 = "bot<id>:"，2 = 密钥前 4 个字符，3 = 中间部分，4 = 最后 4 个字符。
var botTokenRe = regexp.MustCompile(`(bot\d+:)([A-Za-z0-9_-]{4})[A-Za-z0-9_-]{12,}([A-Za-z0-9_-]{4})`)

// maskSecrets 将 s 中任何内嵌的机器人令牌替换为脱敏占位符，
// 保留密钥的首尾各 4 个字符以便识别。
func maskSecrets(s string) string {
	return botTokenRe.ReplaceAllString(s, "${1}${2}****${3}")
}

// Logger 实现通用的 Logger 接口
type Logger struct {
	component string
	levels    map[int]LogLevel
}

// Debug 记录调试级别消息
func (b *Logger) Debug(v ...any) {
	logMessage(DEBUG, b.component, maskSecrets(fmt.Sprint(v...)), nil)
}

// Info 记录信息级别消息
func (b *Logger) Info(v ...any) {
	logMessage(INFO, b.component, maskSecrets(fmt.Sprint(v...)), nil)
}

// Warn 记录警告级别消息
func (b *Logger) Warn(v ...any) {
	logMessage(WARN, b.component, maskSecrets(fmt.Sprint(v...)), nil)
}

// Error 记录错误级别消息
func (b *Logger) Error(v ...any) {
	logMessage(ERROR, b.component, maskSecrets(fmt.Sprint(v...)), nil)
}

// Debugf 记录格式化的调试级别消息
func (b *Logger) Debugf(format string, v ...any) {
	logMessage(DEBUG, b.component, maskSecrets(fmt.Sprintf(format, v...)), nil)
}

// Infof 记录格式化的信息级别消息
func (b *Logger) Infof(format string, v ...any) {
	logMessage(INFO, b.component, maskSecrets(fmt.Sprintf(format, v...)), nil)
}

// Warnf 记录格式化的警告级别消息
func (b *Logger) Warnf(format string, v ...any) {
	logMessage(WARN, b.component, maskSecrets(fmt.Sprintf(format, v...)), nil)
}

// Warningf 记录格式化的警告级别消息
func (b *Logger) Warningf(format string, v ...any) {
	logMessage(WARN, b.component, maskSecrets(fmt.Sprintf(format, v...)), nil)
}

// Errorf 记录格式化的错误级别消息
func (b *Logger) Errorf(format string, v ...any) {
	logMessage(ERROR, b.component, maskSecrets(fmt.Sprintf(format, v...)), nil)
}

// Fatalf 记录格式化的致命级别消息并退出
func (b *Logger) Fatalf(format string, v ...any) {
	logMessage(FATAL, b.component, maskSecrets(fmt.Sprintf(format, v...)), nil)
}

// Log 按给定级别记录消息，并附带调用者信息
// 函数名必须如此，因为第三方日志库期望该名称
// msgL：消息级别（DEBUG、INFO、WARN、ERROR、FATAL）
// caller：未使用的参数，仅为兼容性保留
// format：格式字符串
// a：格式化参数
//
//nolint:goprintffuncname
func (b *Logger) Log(msgL, caller int, format string, a ...any) {
	level := LogLevel(msgL)
	if b.levels != nil {
		if lvl, ok := b.levels[msgL]; ok {
			level = lvl
		}
	}
	logMessage(level, b.component, maskSecrets(fmt.Sprintf(format, a...)), nil)
}

// Sync 刷新日志缓冲区（此实现为空操作）
func (b *Logger) Sync() error {
	return nil
}

// WithLevels 为该日志器设置日志级别映射
func (b *Logger) WithLevels(levels map[int]LogLevel) *Logger {
	b.levels = levels
	return b
}

// NewLogger 创建一个带可选组件名的日志器实例
func NewLogger(component string) *Logger {
	return &Logger{component: component}
}
