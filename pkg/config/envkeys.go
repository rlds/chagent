// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package config

import (
	"os"
	"path/filepath"

	"github.com/rlds/chagent/pkg"
)

// chagent 进程的运行时环境变量键。
// 它们控制运行时文件与二进制文件的位置，并通过
// os.Getenv / os.LookupEnv 直接读取。所有 chagent 专用键都使用
// CHAGENT_ 前缀。请引用这些常量而不是内联字符串字面量，
// 这样所有受支持的配置项都能集中可见，并可避免拼写错误。
const (
	// EnvHome 覆盖所有 chagent 数据的基准目录
	// （配置、工作区、技能、认证存储等）。
	// 默认值：~/.chagent
	EnvHome = "CHAGENT_HOME"

	// EnvConfig 覆盖 JSON 配置文件的完整路径。
	// 默认值：$CHAGENT_HOME/config.json
	EnvConfig = "CHAGENT_CONFIG"

	// EnvBuiltinSkills 覆盖内置技能加载的来源目录。
	// 默认值：<cwd>/skills
	EnvBuiltinSkills = "CHAGENT_BUILTIN_SKILLS"

	// EnvBinary 覆盖 chagent 可执行文件的路径。
	// Web 启动器在派发 gateway 子进程时会使用它。
	// 默认值：从当前可执行文件所在目录解析。
	EnvBinary = "CHAGENT_BINARY"

	// EnvGatewayHost 覆盖 gateway 服务器的主机地址。
	// 默认值："localhost"
	EnvGatewayHost = "CHAGENT_GATEWAY_HOST"

	EnvWorkspace = "CHAGENT_WORKSPACE"
)

func GetHome() string {
	homePath, _ := os.UserHomeDir()
	if chagentHome := os.Getenv(EnvHome); chagentHome != "" {
		homePath = chagentHome
	} else if homePath != "" {
		homePath = filepath.Join(homePath, pkg.DefaultChagentHome)
		// 遗留部署迁移：如果 ~/.chagent 尚不存在，但旧的 ~/.chagent
		// 主目录存在，则继续使用旧的数据目录。
		if _, statErr := os.Stat(homePath); os.IsNotExist(statErr) {
			legacy := filepath.Join(os.Getenv("HOME"), ".chagent")
			if info, legacyErr := os.Stat(legacy); legacyErr == nil && info.IsDir() {
				homePath = legacy
			}
		}
	}
	if homePath == "" {
		homePath = "."
	}
	return homePath
}

func GetWorkspace() string {
	return os.Getenv(EnvWorkspace)
}
