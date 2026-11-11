package config

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
)

// AgentNameEnv 将 gateway 的 --name 取值传入配置加载流程。该值由 gateway
// 命令设置；它属于进程内部约定，而不是用户可调的配置项。
const AgentNameEnv = "CHAGENT_AGENT_NAME"

// ResolveAgentName 规范化 --name。允许使用空名称，空名称会映射为
// "default"；派生出的 token 在 pico 协议两端都是确定性的。
func ResolveAgentName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "default"
	}
	return name
}

// DerivePicoToken 是对外 pico 握手约定：
//
//	token = md5(name)
//
// 调用方（chess/client 或任意 pico 客户端）必须对传给 gateway 的精确 --name
// 计算出相同的 md5。空的 --name 使用 "default"。
func DerivePicoToken(name string) string {
	sum := md5.Sum([]byte(ResolveAgentName(name)))
	return hex.EncodeToString(sum[:])
}

// ApplyNameDerivedPicoToken 让 pico 成为内置通道：它会自动启用
// （channels 在 config.json 中是可选的），且其 token 始终为 md5(--name)。
// 不会从 config.json 读取 token，也不会向其中写入 token。
func ApplyNameDerivedPicoToken(cfg *Config, name string) error {
	if cfg == nil {
		return nil
	}
	if cfg.Channels == nil {
		cfg.Channels = make(ChannelsConfig)
	}
	pico := cfg.Channels.Get("pico")
	if pico == nil {
		pico = &Channel{Enabled: true, Type: "pico"}
		cfg.Channels["pico"] = pico
	}
	pico.Enabled = true
	if pico.Type == "" {
		pico.Type = "pico"
	}
	decoded, err := pico.GetDecoded()
	if err != nil {
		return err
	}
	settings, ok := decoded.(*PicoSettings)
	if !ok || settings == nil {
		return nil
	}
	settings.SetToken(DerivePicoToken(name))
	return nil
}
