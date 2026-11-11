// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package config

import (
	"encoding/json"
	"path/filepath"

	"github.com/rlds/chagent/pkg"
)

// DefaultConfig 返回 Chagent 的默认配置。
func DefaultConfig() *Config {
	workspacePath := filepath.Join(GetHome(), pkg.WorkspaceName)

	return &Config{
		Version: CurrentVersion,
		// 隔离功能采用选择性启用（opt-in）方式，因此在用户显式启用子进程沙箱
		// 之前，现有安装会保持当前行为。
		Isolation: IsolationConfig{
			Enabled: false,
		},
		Agents: AgentsConfig{
			Defaults: AgentDefaults{
				Workspace:                 workspacePath,
				RestrictToWorkspace:       true,
				Provider:                  "",
				MaxTokens:                 32768,
				Temperature:               nil, // nil 表示使用 provider 默认值
				MaxToolIterations:         300,
				SummarizeMessageThreshold: 20,
				SummarizeTokenPercent:     75,
				SteeringMode:              "all",
				MaxParallelTurns:          20,
				ToolFeedback: ToolFeedbackConfig{
					Enabled:          false,
					MaxArgsLength:    300,
					SeparateMessages: false,
				},
				SplitOnMarker:       false,
				MaxLLMRetries:       2,
				LLMRetryBackoffSecs: 2,
				WorkspaceContext:    DefaultWorkspaceContextConfig(),
				RuntimeContext:      DefaultRuntimeContextConfig(),
				Autonomy:            DefaultAutonomyConfig(),
			},
		},
		Session: SessionConfig{
			DmScope: "per-peer",
		},
		Channels: defaultChannels(),
		Hooks: HooksConfig{
			Enabled: true,
			Defaults: HookDefaultsConfig{
				ObserverTimeoutMS:    500,
				InterceptorTimeoutMS: 5000,
				ApprovalTimeoutMS:    60000,
			},
		},
		ModelList: []*ModelConfig{
			// ============================================
			// 为你想要使用的模型添加 API key
			// ============================================

			// Zhipu AI (智谱) - https://open.bigmodel.cn/usercenter/apikeys
			{
				ModelName: "glm-4.7",
				Provider:  "zhipu",
				Model:     "glm-4.7",
				APIBase:   "https://open.bigmodel.cn/api/paas/v4",
			},

			// OpenAI - https://platform.openai.com/api-keys
			{
				ModelName: "gpt-5.4",
				Provider:  "openai",
				Model:     "gpt-5.4",
				APIBase:   "https://api.openai.com/v1",
			},

			// Anthropic Claude - https://console.anthropic.com/settings/keys
			{
				ModelName: "claude-sonnet-4.6",
				Provider:  "anthropic",
				Model:     "claude-sonnet-4-6",
				APIBase:   "https://api.anthropic.com/v1",
			},

			// DeepSeek - https://platform.deepseek.com/
			{
				ModelName: "deepseek-chat",
				Provider:  "deepseek",
				Model:     "deepseek-chat",
				APIBase:   "https://api.deepseek.com/v1",
			},

			// Venice AI - https://venice.ai
			{
				ModelName: "venice-uncensored",
				Provider:  "venice",
				Model:     "venice-uncensored",
				APIBase:   "https://api.venice.ai/api/v1",
			},

			// NEAR AI Cloud TEE 推理 - https://near.ai
			{
				ModelName: "nearai-glm",
				Provider:  "nearai",
				Model:     "zai-org/GLM-5.1-FP8",
				APIBase:   "https://cloud-api.near.ai/v1",
			},

			// Qwen (通义千问) - https://dashscope.console.aliyun.com/apiKey
			{
				ModelName: "qwen-plus",
				Provider:  "qwen",
				Model:     "qwen-plus",
				APIBase:   "https://dashscope.aliyuncs.com/compatible-mode/v1",
			},

			// Moonshot (月之暗面) - https://platform.moonshot.cn/console/api-keys
			{
				ModelName: "moonshot-v1-8k",
				Provider:  "moonshot",
				Model:     "moonshot-v1-8k",
				APIBase:   "https://api.moonshot.cn/v1",
			},

			// Groq - https://console.groq.com/keys
			{
				ModelName: "llama-3.3-70b",
				Provider:  "groq",
				Model:     "llama-3.3-70b-versatile",
				APIBase:   "https://api.groq.com/openai/v1",
			},

			// OpenRouter（100+ 模型） - https://openrouter.ai/keys
			{
				ModelName: "openrouter-auto",
				Provider:  "openrouter",
				Model:     "auto",
				APIBase:   "https://openrouter.ai/api/v1",
			},
			{
				ModelName: "openrouter-gpt-5.4",
				Provider:  "openrouter",
				Model:     "openai/gpt-5.4",
				APIBase:   "https://openrouter.ai/api/v1",
			},

			// NVIDIA - https://build.nvidia.com/
			{
				ModelName: "nemotron-4-340b",
				Provider:  "nvidia",
				Model:     "nemotron-4-340b-instruct",
				APIBase:   "https://integrate.api.nvidia.com/v1",
			},

			// Cerebras - https://inference.cerebras.ai/
			{
				ModelName: "cerebras-llama-3.3-70b",
				Provider:  "cerebras",
				Model:     "llama-3.3-70b",
				APIBase:   "https://api.cerebras.ai/v1",
			},

			// Vivgrid - https://vivgrid.com
			{
				ModelName: "vivgrid-auto",
				Provider:  "vivgrid",
				Model:     "auto",
				APIBase:   "https://api.vivgrid.com/v1",
			},

			// Volcengine (火山引擎) - https://console.volcengine.com/ark
			{
				ModelName: "ark-code-latest",
				Provider:  "volcengine",
				Model:     "ark-code-latest",
				APIBase:   "https://ark.cn-beijing.volces.com/api/v3",
			},
			{
				ModelName: "doubao-pro",
				Provider:  "volcengine",
				Model:     "doubao-pro-32k",
				APIBase:   "https://ark.cn-beijing.volces.com/api/v3",
			},

			// ShengsuanYun (神算云)
			{
				ModelName: "deepseek-v3",
				Provider:  "shengsuanyun",
				Model:     "deepseek-v3",
				APIBase:   "https://api.shengsuanyun.com/v1",
			},

			// Ollama（本地） - https://ollama.com
			{
				ModelName: "llama3",
				Provider:  "ollama",
				Model:     "llama3",
				APIBase:   "http://localhost:11434/v1",
			},

			// Mistral AI - https://console.mistral.ai/api-keys
			{
				ModelName: "mistral-small",
				Provider:  "mistral",
				Model:     "mistral-small-latest",
				APIBase:   "https://api.mistral.ai/v1",
			},

			// Avian - https://avian.io
			{
				ModelName: "deepseek-v3.2",
				Provider:  "avian",
				Model:     "deepseek/deepseek-v3.2",
				APIBase:   "https://api.avian.io/v1",
			},
			{
				ModelName: "kimi-k2.5",
				Provider:  "avian",
				Model:     "moonshotai/kimi-k2.5",
				APIBase:   "https://api.avian.io/v1",
			},

			// Minimax - https://api.minimaxi.com/
			{
				ModelName: "MiniMax-M2.5",
				Provider:  "minimax",
				Model:     "MiniMax-M2.5",
				APIBase:   "https://api.minimaxi.com/v1",
				ExtraBody: map[string]any{"reasoning_split": true},
			},

			// LongCat - https://longcat.chat/platform
			{
				ModelName: "LongCat-Flash-Thinking",
				Provider:  "longcat",
				Model:     "LongCat-Flash-Thinking",
				APIBase:   "https://api.longcat.chat/openai",
			},

			// ModelScope (魔搭社区) - https://modelscope.cn/my/tokens
			{
				ModelName: "modelscope-qwen",
				Provider:  "modelscope",
				Model:     "Qwen/Qwen3-235B-A22B-Instruct-2507",
				APIBase:   "https://api-inference.modelscope.cn/v1",
			},

			// VLLM（本地） - http://localhost:8000
			{
				ModelName: "local-model",
				Provider:  "vllm",
				Model:     "custom-model",
				APIBase:   "http://localhost:8000/v1",
			},

			// LM Studio（本地） - http://localhost:1234
			{
				ModelName: "lmstudio-local",
				Provider:  "lmstudio",
				Model:     "openai/gpt-oss-20b",
				APIBase:   "http://localhost:1234/v1",
			},
		},
		Gateway: GatewayConfig{
			Host:      "127.0.0.1",
			Port:      0,
			HotReload: false,
			LogLevel:  "fatal",
		},
		Events: EventsConfig{
			Logging: defaultEventLoggingConfig(),
		},
		Tools: ToolsConfig{
			FilterSensitiveData: true,
			FilterMinLength:     8,
			MediaCleanup: MediaCleanupConfig{
				ToolConfig: ToolConfig{
					Enabled: true,
				},
				MaxAge:   30,
				Interval: 5,
			},
			Web: WebToolsConfig{
				ToolConfig: ToolConfig{
					Enabled: true,
				},
				Provider:        "auto",
				PreferNative:    true,
				Proxy:           "",
				FetchLimitBytes: 10 * 1024 * 1024, // 默认 10MB
				Format:          "plaintext",
				Brave: BraveConfig{
					Enabled:    false,
					MaxResults: 5,
				},
				Tavily: TavilyConfig{
					Enabled:    false,
					MaxResults: 5,
				},
				Kagi: KagiConfig{
					Enabled:    false,
					BaseURL:    "https://kagi.com/api/v1/search",
					MaxResults: 5,
				},
				Sogou: SogouConfig{
					Enabled:    true,
					MaxResults: 5,
				},
				DuckDuckGo: DuckDuckGoConfig{
					Enabled:    false,
					MaxResults: 5,
				},
				Gemini: GeminiSearchConfig{
					Enabled:    false,
					Model:      "gemini-2.5-flash",
					MaxResults: 5,
				},
				Perplexity: PerplexityConfig{
					Enabled:    false,
					MaxResults: 5,
				},
				SearXNG: SearXNGConfig{
					Enabled:    false,
					BaseURL:    "",
					MaxResults: 5,
				},
				GLMSearch: GLMSearchConfig{
					Enabled:      false,
					BaseURL:      "https://open.bigmodel.cn/api/paas/v4/web_search",
					SearchEngine: "search_std",
					MaxResults:   5,
				},
				BaiduSearch: BaiduSearchConfig{
					Enabled:    false,
					BaseURL:    "https://qianfan.baidubce.com/v2/ai_search/web_search",
					MaxResults: 10,
				},
			},
			Cron: CronToolsConfig{
				ToolConfig: ToolConfig{
					Enabled: true,
				},
				ExecTimeoutMinutes: 5,
				AllowCommand:       true,
			},
			Exec: ExecConfig{
				ToolConfig: ToolConfig{
					Enabled: true,
				},
				EnableDenyPatterns: true,
				AllowRemote:        true,
				TimeoutSeconds:     60,
			},
			Skills: SkillsToolsConfig{
				ToolConfig: ToolConfig{
					Enabled: true,
				},
				CatalogMode: SkillsCatalogModeContext,
				Registries: SkillsRegistriesConfig{
					&SkillRegistryConfig{
						Name:    "clawhub",
						Enabled: true,
						BaseURL: "https://clawhub.ai",
						Param:   map[string]any{},
					},
					&SkillRegistryConfig{
						Name:    "github",
						Enabled: true,
						BaseURL: "https://github.com",
						Param:   map[string]any{},
					},
				},
				MaxConcurrentSearches: 2,
				SearchCache: SearchCacheConfig{
					MaxSize:    50,
					TTLSeconds: 300,
				},
			},
			SendFile: ToolConfig{
				Enabled: true,
			},
			MCP: MCPConfig{
				ToolConfig: ToolConfig{
					Enabled: false,
				},
				Discovery: ToolDiscoveryConfig{
					Enabled:          false,
					TTL:              5,
					MaxSearchResults: 5,
					UseBM25:          true,
					UseRegex:         false,
				},
				MaxInlineTextChars: DefaultMCPMaxInlineTextChars,
				Servers:            map[string]MCPServerConfig{},
			},
			AppendFile: ToolConfig{
				Enabled: true,
			},
			EditFile: ToolConfig{
				Enabled: true,
			},
			FindSkills: ToolConfig{
				Enabled: true,
			}, InstallSkill: ToolConfig{
				Enabled: true,
			},
			ListDir: ToolConfig{
				Enabled: true,
			},
			LoadImage: ToolConfig{
				Enabled: true,
			},
			Message: MessageToolsConfig{
				ToolConfig: ToolConfig{
					Enabled: true,
				},
				MediaEnabled: false,
			},
			ReadFile: ReadFileToolConfig{
				Enabled:         true,
				Mode:            ReadFileModeBytes,
				MaxReadFileSize: 64 * 1024, // 64KB
			}, Spawn: ToolConfig{
				Enabled: true,
			},
			SpawnStatus: ToolConfig{
				Enabled: false,
			}, Subagent: ToolConfig{
				Enabled: true,
			},
			// 自主性/工作区上下文工具：chess 默认启用这些工具。
			Task: ToolConfig{
				Enabled: true,
			},
			Todo: ToolConfig{
				Enabled: true,
			},
			SkillLoad: ToolConfig{
				Enabled: true,
			},
			WebFetch: ToolConfig{
				Enabled: true,
			},
			WriteFile: ToolConfig{
				Enabled: true,
			},
		},
		Heartbeat: HeartbeatConfig{
			Enabled:  true,
			Interval: 30,
		},
		BuildInfo: BuildInfo{
			Version:   Version,
			GitCommit: GitCommit,
			BuildTime: BuildTime,
			GoVersion: GoVersion,
		},
	}
}

func defaultChannels() ChannelsConfig {
	defs := map[string]any{
		"pico": map[string]any{
			"settings": map[string]any{
				"ping_interval":   30,
				"read_timeout":    60,
				"write_timeout":   10,
				"max_connections": 100,
				"streaming":       map[string]any{"enabled": true},
			},
		},
	}

	channels := make(ChannelsConfig, len(defs))
	for name, def := range defs {
		data, err := json.Marshal(def)
		if err != nil {
			continue
		}
		bc := &Channel{}
		if err := json.Unmarshal(data, bc); err != nil {
			continue
		}
		bc.SetName(name)
		if bc.Type == "" {
			bc.Type = name
		}
		channels[name] = bc
	}
	return channels
}
