// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// workspace context / runtime context / autonomy 子系统的默认值。
// 这些值描述了“chess profile”：上下文注入开启，仅扫描 agent 工作区，
// 并在启用自主运行的同时配备保守的进度保护。
const (
	DefaultWorkspaceContextMaxBytes       = 32768
	DefaultWorkspaceContextMaxSourceBytes = 1048576

	RuntimeContextModeSystem   = "system"
	RuntimeContextModeSnapshot = "snapshot"
)

// DefaultWorkspaceContextMarkers / files / local files 是扫描项目指令时
// 使用的候选名称。
var (
	DefaultWorkspaceContextMarkers      = []string{".git"}
	DefaultWorkspaceContextFiles        = []string{"AGENTS.md", "CLAUDE.md", "CHAGENT.md"}
	DefaultWorkspaceContextLocalFiles   = []string{"AGENTS.local.md", "CLAUDE.local.md"}
	defaultWorkspaceContextCandidateSet = [][]string{
		DefaultWorkspaceContextMarkers,
		DefaultWorkspaceContextFiles,
		DefaultWorkspaceContextLocalFiles,
	}
)

// WorkspaceContextConfig 控制工作区指令文件（AGENTS.md / CLAUDE.md 等）
// 的发现与注入。
type WorkspaceContextConfig struct {
	Enabled               bool     `json:"enabled"`
	MaxBytes              int      `json:"max_bytes,omitempty"`
	MaxSourceBytes        int      `json:"max_source_bytes,omitempty"`
	ProjectRootMarkers    []string `json:"project_root_markers,omitempty"`
	InstructionFiles      []string `json:"instruction_files,omitempty"`
	LocalInstructionFiles []string `json:"local_instruction_files,omitempty"`
	UserGlobal            bool     `json:"user_global,omitempty"`
	FollowSymlinks        bool     `json:"follow_symlinks,omitempty"`
}

// DefaultWorkspaceContextConfig 返回 chess profile 的默认值。
func DefaultWorkspaceContextConfig() WorkspaceContextConfig {
	return WorkspaceContextConfig{
		Enabled:               true,
		MaxBytes:              DefaultWorkspaceContextMaxBytes,
		MaxSourceBytes:        DefaultWorkspaceContextMaxSourceBytes,
		ProjectRootMarkers:    append([]string(nil), DefaultWorkspaceContextMarkers...),
		InstructionFiles:      append([]string(nil), DefaultWorkspaceContextFiles...),
		LocalInstructionFiles: append([]string(nil), DefaultWorkspaceContextLocalFiles...),
		UserGlobal:            false,
		FollowSymlinks:        false,
	}
}

// ApplyDefaults 填充零值，使省略该配置段（或将其显式清空）的旧配置
// 仍然表现得与 chess profile 一致。
func (c *WorkspaceContextConfig) ApplyDefaults() {
	if c == nil {
		return
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultWorkspaceContextMaxBytes
	}
	if c.MaxSourceBytes <= 0 {
		c.MaxSourceBytes = DefaultWorkspaceContextMaxSourceBytes
	}
	if len(c.ProjectRootMarkers) == 0 {
		c.ProjectRootMarkers = append([]string(nil), DefaultWorkspaceContextMarkers...)
	}
	if len(c.InstructionFiles) == 0 {
		c.InstructionFiles = append([]string(nil), DefaultWorkspaceContextFiles...)
	}
	if len(c.LocalInstructionFiles) == 0 {
		c.LocalInstructionFiles = append([]string(nil), DefaultWorkspaceContextLocalFiles...)
	}
}

// Validate 拒绝危险/有歧义的值，而不是静默降级。
func (c *WorkspaceContextConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.MaxBytes <= 0 {
		return fmt.Errorf("agents.defaults.workspace_context.max_bytes must be > 0")
	}
	if c.MaxSourceBytes <= 0 {
		return fmt.Errorf("agents.defaults.workspace_context.max_source_bytes must be > 0")
	}
	fIELDS := []struct {
		name   string
		values []string
	}{
		{"project_root_markers", c.ProjectRootMarkers},
		{"instruction_files", c.InstructionFiles},
		{"local_instruction_files", c.LocalInstructionFiles},
	}
	for _, field := range fIELDS {
		for _, value := range field.values {
			if err := validateWorkspaceContextCandidate(field.name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateWorkspaceContextCandidate(field, value string) error {
	name := strings.TrimSpace(value)
	if name == "" {
		return fmt.Errorf("agents.defaults.workspace_context.%s contains an empty candidate name", field)
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf(
			"agents.defaults.workspace_context.%s contains invalid candidate %q: must not contain '/', '\\' or '..'",
			field,
			value,
		)
	}
	return nil
}

// RuntimeContextConfig 控制易变的运行时状态（时间、工作区、会话、发送者）
// 如何传递给模型。
type RuntimeContextConfig struct {
	// Mode 为 "snapshot"（值变化时追加一条持久上下文消息）
	// 或 "system"（旧行为：渲染进系统提示词）。
	Mode                   string `json:"mode,omitempty"`
	RefreshIntervalSeconds int    `json:"refresh_interval_seconds,omitempty"`
	IncludeSender          bool   `json:"include_sender,omitempty"`
}

// DefaultRuntimeContextConfig 返回 chess profile 的默认值。
func DefaultRuntimeContextConfig() RuntimeContextConfig {
	return RuntimeContextConfig{
		Mode:                   RuntimeContextModeSnapshot,
		RefreshIntervalSeconds: 600,
		IncludeSender:          true,
	}
}

// ApplyDefaults 为旧配置填充零值。
func (c *RuntimeContextConfig) ApplyDefaults() {
	if c == nil {
		return
	}
	if strings.TrimSpace(c.Mode) == "" {
		c.Mode = RuntimeContextModeSnapshot
	}
	if c.RefreshIntervalSeconds <= 0 {
		c.RefreshIntervalSeconds = 600
	}
}

// Validate 检查运行时上下文模式与刷新间隔。
func (c *RuntimeContextConfig) Validate() error {
	if c == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	if mode != RuntimeContextModeSystem && mode != RuntimeContextModeSnapshot {
		return fmt.Errorf(
			"agents.defaults.runtime_context.mode = %q is invalid: allowed values are %q or %q",
			c.Mode,
			RuntimeContextModeSystem,
			RuntimeContextModeSnapshot,
		)
	}
	if c.RefreshIntervalSeconds < 0 {
		return fmt.Errorf("agents.defaults.runtime_context.refresh_interval_seconds must be >= 0")
	}
	return nil
}

// AutonomyConfig 控制一轮结束后自主续跑的轮次。
type AutonomyConfig struct {
	Enabled                 bool `json:"enabled"`
	MaxRounds               int  `json:"max_rounds,omitempty"`
	RoundDelayMS            int  `json:"round_delay_ms,omitempty"`
	NoProgressRounds        int  `json:"no_progress_rounds,omitempty"`
	ResumeOnStart           bool `json:"resume_on_start,omitempty"`
	SendIntermediate        bool `json:"send_intermediate,omitempty"`
	AllowParallelInProgress bool `json:"allow_parallel_in_progress,omitempty"`
}

// DefaultAutonomyConfig 返回 chess profile 的默认值：自主续跑开启，
// 但进程启动时绝不恢复。
func DefaultAutonomyConfig() AutonomyConfig {
	return AutonomyConfig{
		Enabled:                 true,
		MaxRounds:               20,
		RoundDelayMS:            1000,
		NoProgressRounds:        3,
		ResumeOnStart:           false,
		SendIntermediate:        false,
		AllowParallelInProgress: false,
	}
}

// ApplyDefaults 为旧配置填充零值。
func (c *AutonomyConfig) ApplyDefaults() {
	if c == nil {
		return
	}
	if c.MaxRounds <= 0 {
		c.MaxRounds = 20
	}
	if c.RoundDelayMS <= 0 {
		c.RoundDelayMS = 1000
	}
	if c.NoProgressRounds <= 0 {
		c.NoProgressRounds = 3
	}
}

// Validate 对自主运行参数进行边界检查。
func (c *AutonomyConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.MaxRounds < 1 || c.MaxRounds > 200 {
		return fmt.Errorf("agents.defaults.autonomy.max_rounds must be between 1 and 200, got %d", c.MaxRounds)
	}
	if c.NoProgressRounds < 1 || c.NoProgressRounds > 10 {
		return fmt.Errorf(
			"agents.defaults.autonomy.no_progress_rounds must be between 1 and 10, got %d",
			c.NoProgressRounds,
		)
	}
	if c.RoundDelayMS < 0 {
		return fmt.Errorf("agents.defaults.autonomy.round_delay_ms must be >= 0")
	}
	return nil
}

// explicitContextNumericFields 对应 workspace-context/autonomy 的数值参数，
// ApplyContextDefaults 会把其中的非正值静默替换为默认值。指针字段使缺失的键
// 能与显式的 0 区分开（null 会解码为 nil，同样被视为缺失）。
type explicitContextNumericFields struct {
	Agents struct {
		Defaults struct {
			Autonomy struct {
				MaxRounds        *int `json:"max_rounds"`
				NoProgressRounds *int `json:"no_progress_rounds"`
			} `json:"autonomy"`
			WorkspaceContext struct {
				MaxBytes       *int `json:"max_bytes"`
				MaxSourceBytes *int `json:"max_source_bytes"`
			} `json:"workspace_context"`
		} `json:"defaults"`
	} `json:"agents"`
}

// ValidateExplicitContextZeros 拒绝将某个 workspace-context/autonomy 数值参数
// 显式设置为非正值的配置。
//
// ApplyContextDefaults 会把所有 <= 0 的值替换为 chess profile 默认值，
// 因此等到校验运行时，显式的 `"max_rounds": 0`（一种常见的意外清零字段的方式）
// 与省略该字段已无法区分。在应用默认值之前检查原始 JSON，可以让这类笔误
// 保持可诊断（字段路径 + 允许范围），而真正省略的键仍会获得默认值。
// round_delay_ms 有意不检查，因为 0 在那里是合法值。
//
// 格式错误的 JSON 交给常规解码器处理：此处返回 nil 可保留现有的
// 语法/类型诊断信息（包括精确的字段路径）。
func ValidateExplicitContextZeros(raw []byte) error {
	var fields explicitContextNumericFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	defaults := fields.Agents.Defaults

	if v := defaults.WorkspaceContext.MaxBytes; v != nil && *v <= 0 {
		return fmt.Errorf(
			"agents.defaults.workspace_context.max_bytes must be > 0, got %d: explicitly configured values must be positive, omit the field to use the default",
			*v,
		)
	}
	if v := defaults.WorkspaceContext.MaxSourceBytes; v != nil && *v <= 0 {
		return fmt.Errorf(
			"agents.defaults.workspace_context.max_source_bytes must be > 0, got %d: explicitly configured values must be positive, omit the field to use the default",
			*v,
		)
	}
	if v := defaults.Autonomy.MaxRounds; v != nil && *v <= 0 {
		return fmt.Errorf(
			"agents.defaults.autonomy.max_rounds must be between 1 and 200, got %d: explicitly configured values must be within range, omit the field to use the default",
			*v,
		)
	}
	if v := defaults.Autonomy.NoProgressRounds; v != nil && *v <= 0 {
		return fmt.Errorf(
			"agents.defaults.autonomy.no_progress_rounds must be between 1 and 10, got %d: explicitly configured values must be within range, omit the field to use the default",
			*v,
		)
	}
	return nil
}

// ApplyContextDefaults 用 chess profile 默认值填充每个 context/autonomy 配置段。
// 它在加载配置后调用，使省略这些配置段的旧文件仍能正常工作。
func (c *Config) ApplyContextDefaults() {
	if c == nil {
		return
	}
	c.Agents.Defaults.WorkspaceContext.ApplyDefaults()
	c.Agents.Defaults.RuntimeContext.ApplyDefaults()
	c.Agents.Defaults.Autonomy.ApplyDefaults()
}

// ValidateContextConfigs 校验 workspace/runtime/autonomy 配置段。
func (c *Config) ValidateContextConfigs() error {
	if c == nil {
		return nil
	}
	if err := c.Agents.Defaults.WorkspaceContext.Validate(); err != nil {
		return err
	}
	if err := c.Agents.Defaults.RuntimeContext.Validate(); err != nil {
		return err
	}
	return c.Agents.Defaults.Autonomy.Validate()
}

// WorkspaceContextEnabled 报告工作区指令扫描是否开启。
func (c *Config) WorkspaceContextEnabled() bool {
	return c != nil && c.Agents.Defaults.WorkspaceContext.Enabled
}

// RuntimeContextMode 返回规范化后的运行时上下文模式。
func (c *Config) RuntimeContextMode() string {
	if c == nil {
		return RuntimeContextModeSnapshot
	}
	mode := strings.ToLower(strings.TrimSpace(c.Agents.Defaults.RuntimeContext.Mode))
	if mode == "" {
		return RuntimeContextModeSnapshot
	}
	return mode
}

// AutonomyEnabled 报告自主续跑轮次是否开启。
func (c *Config) AutonomyEnabled() bool {
	return c != nil && c.Agents.Defaults.Autonomy.Enabled
}
