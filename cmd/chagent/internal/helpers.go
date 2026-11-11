package internal

import (
	"os"
	"path/filepath"

	"github.com/rlds/chagent/pkg"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
)

const Logo = pkg.Logo

// GetChagentHome 返回 chagent 主目录。
// 优先级：$CHAGENT_HOME > ~/.chagent
func GetChagentHome() string {
	return config.GetHome()
}

func GetConfigPath() string {
	if configPath := os.Getenv(config.EnvConfig); configPath != "" {
		return configPath
	}
	return filepath.Join(GetChagentHome(), "config.json")
}

func LoadConfig() (*config.Config, error) {
	cfg, err := config.LoadConfig(GetConfigPath())
	if err != nil {
		return nil, err
	}
	logger.SetLevelFromString(cfg.Gateway.LogLevel)
	return cfg, nil
}

// FormatVersion 返回版本字符串，可选附带 git 提交信息
// Deprecated: 请改用 pkg/config.FormatVersion
func FormatVersion() string {
	return config.FormatVersion()
}

// FormatBuildInfo 返回构建时间与 Go 版本信息
// Deprecated: 请改用 pkg/config.FormatBuildInfo
func FormatBuildInfo() (string, string) {
	return config.FormatBuildInfo()
}

// GetVersion 返回版本字符串
// Deprecated: 请改用 pkg/config.GetVersion
func GetVersion() string {
	return config.GetVersion()
}
