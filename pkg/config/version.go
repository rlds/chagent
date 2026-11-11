package config

import (
	"fmt"
	"runtime"
)

// 构建期变量，在构建过程中通过 ldflags 注入。
// 这些变量由 Makefile 或 .goreleaser.yaml 通过 -X 标志设置：
//
//	-X github.com/rlds/chagent/pkg/config.Version=<version>
//	-X github.com/rlds/chagent/pkg/config.GitCommit=<commit>
//	-X github.com/rlds/chagent/pkg/config.BuildTime=<timestamp>
//	-X github.com/rlds/chagent/pkg/config.GoVersion=<go-version>
var (
	Version   = "dev" // 未通过 ldflags 构建时的默认值
	GitCommit string  // Git 提交 SHA（短）
	BuildTime string  // RFC3339 格式的构建时间戳
	GoVersion string  // 构建所用的 Go 版本
)

// FormatVersion 返回带可选 git 提交信息的版本字符串
func FormatVersion() string {
	v := Version
	if GitCommit != "" {
		v += fmt.Sprintf(" (git: %s)", GitCommit)
	}
	return v
}

// FormatBuildInfo 返回构建时间与 Go 版本信息
func FormatBuildInfo() (string, string) {
	build := BuildTime
	goVer := GoVersion
	if goVer == "" {
		goVer = runtime.Version()
	}
	return build, goVer
}

// GetVersion 返回版本字符串
func GetVersion() string {
	return Version
}
