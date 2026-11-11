// Chagent - 超轻量级个人 AI agent
// 灵感来源于 nanobot，并基于其构建：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rlds/chagent/cmd/chagent/internal"
	"github.com/rlds/chagent/cmd/chagent/internal/gateway"
	"github.com/rlds/chagent/cmd/chagent/internal/version"
	"github.com/rlds/chagent/pkg/config"
)

var rootNoColor bool

// initTermuxSSL 检测 Termux 环境，并在尚未设置时设置 SSL_CERT_FILE。
// 这修复了在 Termux 或 termux-chroot 中运行 Chagent 时的 X509 证书错误。
// 参见：https://github.com/rlds/chagent/issues/2944
func initTermuxSSL() {
	// 仅在 Linux/Android 上适用
	if runtime.GOOS != "linux" && runtime.GOOS != "android" {
		return
	}

	// 如果已经设置则跳过
	if os.Getenv("SSL_CERT_FILE") != "" {
		return
	}

	// 在 PATH 或 HOME 中检查 Termux 前缀
	home := os.Getenv("HOME")
	path := os.Getenv("PATH")

	isTermux := strings.Contains(home, "com.termux") ||
		strings.Contains(path, "com.termux") ||
		strings.Contains(home, "/data/data/com.termux")

	if !isTermux {
		return
	}

	// 检查 Termux 中常见的 CA 证书包位置
	caPaths := []string{
		"$PREFIX/etc/tls/cert.pem",
		os.Getenv("PREFIX") + "/etc/tls/cert.pem",
		"/data/data/com.termux/files/usr/etc/tls/cert.pem",
		"/usr/etc/tls/cert.pem",
	}

	for _, caPath := range caPaths {
		expanded := os.ExpandEnv(caPath)
		if _, err := os.Stat(expanded); err == nil {
			os.Setenv("SSL_CERT_FILE", expanded)
			return
		}
	}
}

// earlyColorDisabled 在 Cobra 解析参数之前，根据环境变量和命令行参数
// 判断横幅颜色是否禁用，与横幅颜色的行为保持一致。
func earlyColorDisabled() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return true
	}
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == "--no-color" || arg == "--no-color=true" || arg == "--no-color=1" {
			return true
		}
	}
	return false
}

func NewChagentCommand() *cobra.Command {
	short := fmt.Sprintf("%s Chagent — 个人 AI 助手", internal.Logo)
	long := fmt.Sprintf(`%s Chagent is a lightweight personal AI assistant.

Version: %s`, internal.Logo, config.FormatVersion())

	cmd := &cobra.Command{
		Use:   "chagent",
		Short: short,
		Long:  long,
		Example: `chagent version
chagent --no-color status`,
		SilenceErrors: true,
		// 命令失败时，避免把纯 UsageString() 输出到 stderr/stdout。
		SilenceUsage: true,
	}

	cmd.PersistentFlags().BoolVar(&rootNoColor, "no-color", false,
		"禁用颜色（盒子布局保持不变）")

	cmd.AddCommand(
		gateway.NewGatewayCommand(),
		version.NewVersionCommand(),
	)

	return cmd
}

const (
	colorBlue   = "\033[1;38;2;62;93;185m"
	colorRed    = "\033[1;38;2;213;70;70m"
	banner      = "\r\n" + "CHAGENT" + "\033[0m\r\n"
	plainBanner = "\r\n" + "CHAGENT" + "\r\n"
)

func main() {
	// 在做其他任何事情之前，先初始化 Termux SSL 证书检测
	initTermuxSSL()

	if earlyColorDisabled() {
		fmt.Print(plainBanner)
	} else {
		fmt.Printf("%s", banner)
	}

	tzEnv := os.Getenv("TZ")
	if tzEnv != "" {
		fmt.Println("TZ environment:", tzEnv)
		zoneinfoEnv := os.Getenv("ZONEINFO")
		fmt.Println("ZONEINFO environment:", zoneinfoEnv)
		loc, err := time.LoadLocation(tzEnv)
		if err != nil {
			fmt.Println("加载时区失败：", err)
		} else {
			fmt.Println("时区加载成功：", loc)
			time.Local = loc //nolint:gosmopolitan // 我们有意根据 TZ 环境变量设置本地时区
		}
	}

	cmd := NewChagentCommand()
	_, err := cmd.ExecuteC()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
