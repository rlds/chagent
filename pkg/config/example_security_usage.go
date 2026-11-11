// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

// 本文件演示如何使用安全配置（security）功能。
// 它不用于编译，只作为文档说明。

/*
Package config

# 示例：使用安全配置

## 概述

安全配置功能允许你将敏感数据（API key、token、secret、password）与主配置分离。
系统会自动从 `.security.yml` 加载这些值，并把它们填入配置中对应的字段。

**要点：**
- `.security.yml` 中的值会自动映射到配置字段
- 不需要 `ref:` 语法——只需在 config.json 中省略敏感字段
- 若同一字段在两个文件中都存在，则以 `.security.yml` 的值为准
- 可以混用 config.json 中的直接值与安全配置文件中的值

## 1. 创建 .security.yml

文件：~/.chagent/.security.yml

```yaml
# 模型 API Key
# 所有模型都必须使用 'api_keys'（复数）数组格式
# 即使只有一个 key，也必须以只含一个元素的数组形式提供
model_list:

	gpt-5.4:
	  api_keys:
	    - "sk-proj-your-actual-openai-key-1"
	    - "sk-proj-your-actual-openai-key-2"  # 可选：多个 key 用于故障转移
	claude-sonnet-4.6:
	  api_keys:
	    - "sk-ant-your-actual-anthropic-key"  # 单 key 也使用数组格式

# 渠道 Token
channels:

	telegram:
	  token: "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz"
	discord:
	  token: "your-discord-bot-token"

# Web 工具 Key
# Brave、Tavily、Perplexity、Kagi：使用 'api_keys' 数组
# GLMSearch、BaiduSearch：使用 'api_key' 单个字符串
web:

	brave:
	  api_keys:
	    - "BSAyour-brave-api-key-1"
	    - "BSAyour-brave-api-key-2"  # 可选：多个 key 用于故障转移
	tavily:
	  api_keys:
	    - "tvly-your-tavily-api-key"  # 单 key 也使用数组格式
	perplexity:
	  api_keys:
	    - "pplx-your-perplexity-api-key"  # 单 key 也使用数组格式
	kagi:
	  api_keys:
	    - "your-kagi-api-key"  # 单 key 也使用数组格式
	glm_search:
	  api_key: "your-glm-search-api-key"  # 单 key（非数组）
	baidu_search:
	  api_key: "your-baidu-search-api-key"  # 单 key（非数组）

```

## 2. 精简 config.json

文件：~/.chagent/config.json

注意：敏感字段已省略，因为它们会从 .security.yml 加载

```json

	{
	  "version": 1,
	  "agents": {
	    "defaults": {
	      "workspace": "~/chagent-workspace",
	      "model_name": "gpt-5.4"
	    }
	  },
	  "model_list": [
	    {
	      "model_name": "gpt-5.4",
	      "model": "openai/gpt-5.4",
	      "api_base": "https://api.openai.com/v1"
	      // api_key 自动从 .security.yml 加载
	    },
	    {
	      "model_name": "claude-sonnet-4.6",
	      "model": "anthropic/claude-sonnet-4.6",
	      "api_base": "https://api.anthropic.com/v1"
	      // api_key 自动从 .security.yml 加载
	    }
	  ],
	  "channels": {
	    "telegram": {
	      "enabled": true
	      // token 自动从 .security.yml 加载
	    },
	    "discord": {
	      "enabled": true
	      // token 自动从 .security.yml 加载
	    }
	  },
	  "tools": {
	    "web": {
	      "brave": {
	        "enabled": true
	        // api_key 自动从 .security.yml 加载
	      },
	      "tavily": {
	        "enabled": true
	        // api_key 自动从 .security.yml 加载
	      },
	      "glm_search": {
	        "enabled": true
	        // api_key 自动从 .security.yml 加载
	      },
	      "baidu_search": {
	        "enabled": true
	        // api_key 自动从 .security.yml 加载
	      }
	    }
	  }
	}

```

## 3. 设置合适的权限

```bash
chmod 600 ~/.chagent/.security.yml
```

## 4. 加入 .gitignore

```gitignore
# 安全配置文件
.security.yml
```

## 5. 验证是否生效

```bash
chagent --version
```

# .security.yml 支持的字段

## 模型 API Key

在 .security.yml 中，所有模型都必须使用 `api_keys`（复数）数组格式。

```yaml
model_list:

	<model_name>:
	  api_keys:
	    - "key-1"
	    - "key-2"  # 可选：多个 key 用于故障转移

```

示例：
```yaml
model_list:

	gpt-5.4:
	  api_keys:
	    - "sk-proj-key-1"
	    - "sk-proj-key-2"
	claude-sonnet-4.6:
	  api_keys:
	    - "sk-ant-key"

```

**重要：**
- 模型始终使用 `api_keys`（复数）
- 即使只有一个 key，也必须写成数组格式
- .security.yml 中的 model_name 必须与 config.json 中的 model_name 一致

## 渠道 Token/Secret

```yaml
channels:

	telegram:
	  token: "value"
	feishu:
	  app_secret: "value"
	  encrypt_key: "value"
	  verification_token: "value"
	discord:
	  token: "value"
	weixin:
	  token: "value"
	qq:
	  app_secret: "value"
	dingtalk:
	  client_secret: "value"
	slack:
	  bot_token: "value"
	  app_token: "value"
	matrix:
	  access_token: "value"
	line:
	  channel_secret: "value"
	  channel_access_token: "value"
	onebot:
	  access_token: "value"
	wecom:
	  token: "value"
	  encoding_aes_key: "value"
	wecom_app:
	  corp_secret: "value"
	  token: "value"
	  encoding_aes_key: "value"
	wecom_aibot:
	  secret: "value"
	  token: "value"
	  encoding_aes_key: "value"
	pico:
	  token: "value"
	irc:
	  password: "value"
	  nickserv_password: "value"
	  sasl_password: "value"

## Web 工具 API Key

**Brave、Tavily、Perplexity、Kagi：**
```yaml
web:

	brave:
	  api_keys:
	    - "BSA-key-1"
	    - "BSA-key-2"
	tavily:
	  api_keys:
	    - "tvly-key"
	perplexity:
	  api_keys:
	    - "pplx-key"
	kagi:
	  api_keys:
	    - "kagi-key"

```
使用 `api_keys`（复数）数组格式。

**GLMSearch、BaiduSearch：**
```yaml
web:

	glm_search:
	  api_key: "your-glm-key"
	baidu_search:
	  api_key: "your-baidu-key"

```
使用 `api_key`（单数）单个字符串格式。

## 技能注册表 Token

```yaml
skills:

	github:
	  token: "value"
	clawhub:
	  auth_token: "value"

```

# 向后兼容

如有需要，你仍可以在 config.json 中直接写值：

```json

	{
	  "model_list": [
	    {
	      "model_name": "local-model",
	      "model": "ollama/llama3",
	      "api_base": "http://localhost:11434/v1",
	      "api_key": "ollama"  // 直接写入的值（可以正常工作）
	    }
	  ]
	}

```

你也可以混用安全配置值与直接写入的值：

```json

	{
	  "model_list": [
	    {
	      "model_name": "cloud-model",
	      // api_key 从 .security.yml 加载
	    },
	    {
	      "model_name": "local-model",
	      "model": "ollama/llama3",
	      "api_base": "http://localhost:11434/v1",
	      "api_key": "ollama"  // 直接写入的值
	    }
	  ]
	}

```

**优先级顺序：**
1. 环境变量（最高优先级）
2. .security.yml 中的值
3. config.json 中直接写入的值（最低优先级）

# 从旧配置迁移

## 步骤 1：备份配置
```bash
cp ~/.chagent/config.json ~/.chagent/config.json.backup
```

## 步骤 2：创建 .security.yml
```bash
cp security.example.yml ~/.chagent/.security.yml
```

## 步骤 3：填入 API key
编辑 ~/.chagent/.security.yml，把占位符替换为真实 key。

## 步骤 4：精简 config.json（推荐）
从 ~/.chagent/config.json 中删除敏感字段：
- model_list 条目中的 `api_key` 字段
- channels 中的 `token` 字段
- tools.web 中的 `api_key` 字段
- tools.skills 中的 `token`/`auth_token` 字段

## 步骤 5：设置权限
```bash
chmod 600 ~/.chagent/.security.yml
```

## 步骤 6：测试
```bash
chagent --version
```

如果一切正常，可以删除备份：
```bash
rm ~/.chagent/config.json.backup
```

# 高级功能

## 多个 API Key（负载均衡与故障转移）

你可以为模型和 Web 工具配置多个 API key，从而启用：
- **负载均衡**：请求会分散到多个 key 上
- **故障转移**：某个 key 失败时，系统会自动切换到另一个 key
- **限流管理**：把用量分摊到多个 key 上
- **高可用**：减少 API 提供方故障期间的不可用时间

### 示例：带多个 key 的模型

**.security.yml：**
```yaml
model_list:

	gpt-5.4:
	  api_keys:
	    - "sk-proj-key-1"
	    - "sk-proj-key-2"
	    - "sk-proj-key-3"

```

**config.json：**
```json

	{
	  "model_list": [
	    {
	      "model_name": "gpt-5.4",
	      "model": "openai/gpt-5.4",
	      "api_base": "https://api.openai.com/v1"
	    }
	  ]
	}

```

### 示例：带多个 key 的 Web 工具

**.security.yml：**
```yaml
web:

	brave:
	  api_keys:
	    - "BSA-key-1"
	    - "BSA-key-2"
	tavily:
	  api_keys:
	    - "tvly-your-key"  # 单 key 也使用数组格式
	glm_search:
	  api_key: "your-glm-key"  # GLMSearch 使用单 key 格式

```

**config.json：**
```json

	{
	  "tools": {
	    "web": {
	      "brave": {
	        "enabled": true
	      },
	      "tavily": {
	        "enabled": true
	      },
	      "glm_search": {
	        "enabled": true
	      }
	    }
	  }
	}

```

## 单个 Key 格式

**模型、Brave、Tavily、Perplexity、Kagi：**
```yaml
model_list:

	gpt-5.4:
	  api_keys:
	    - "sk-proj-your-key"  # 单 key 也使用数组格式

```

**GLMSearch、BaiduSearch：**
```yaml
web:

	glm_search:
	  api_key: "your-glm-key"  # 单 key（非数组）

```

## 模型名匹配

系统支持在 .security.yml 中使用智能的模型名匹配：

### 示例 1：精确匹配

**config.json：**
```json

	{
	  "model_name": "gpt-5.4:0"
	}

```

**.security.yml（带索引的精确匹配）：**
```yaml
model_list:

	gpt-5.4:0:
	  api_keys: ["key-1"]

```

### 示例 2：基础名匹配

**config.json：**
```json

	{
	  "model_name": "gpt-5.4:0"
	}

```

**.security.yml（不带索引的基础名）：**
```yaml
model_list:

	gpt-5.4:
	  api_keys: ["key-1", "key-2"]

```

两种方式都有效。基础名匹配让你可以在 .security.yml 中使用更简单的键，
即使 config.json 为了负载均衡使用了带索引的模型名。

## 安全配置文件权限

安全配置文件应具有受限权限：

```bash
chmod 600 ~/.chagent/.security.yml
```

这样只有属主才能读写该文件。

# 安全最佳实践

1. 绝不要把 .security.yml 提交到版本控制
2. 把 .security.yml 加入 .gitignore
3. 设置文件权限：chmod 600 ~/.chagent/.security.yml
4. 为不同环境（dev、staging、production）使用不同的 key
5. 定期轮换 key 并更新 .security.yml
6. 加密包含 .security.yml 的备份
7. 定期检查访问情况

# 环境变量

你可以用环境变量覆盖任意安全配置值：

```bash
# 渠道
export CHAGENT_CHANNELS_TELEGRAM_TOKEN="token-from-env"
export CHAGENT_CHANNELS_DISCORD_TOKEN="discord-token-from-env"

# Web 工具
export CHAGENT_TOOLS_WEB_BRAVE_API_KEY="brave-key-from-env"
export CHAGENT_TOOLS_WEB_BAIDU_API_KEY="baidu-key-from-env"

# 技能
export CHAGENT_TOOLS_SKILLS_GITHUB_TOKEN="github-token-from-env"
```

环境变量具有最高优先级，会覆盖 config.json 与 .security.yml 中的值。

# 故障排查

## 错误："failed to load security config"
- 确认 .security.yml 与 config.json 位于同一目录
- 检查 YAML 语法是否合法（可用 YAML 校验器）
- 确认文件权限允许读取

## 错误："model security entry not found"
- 检查 config.json 中的模型名是否与 .security.yml 中完全一致
- 确认 .security.yml 中存在 model_list 段
- 对于带索引的名称（例如 "gpt-5.4:0"），同时检查精确匹配与基础名匹配
- 确认 YAML 结构正确（缩进正确）

## 多个 API Key 不生效
- 确认模型与 Web 工具（GLMSearch/BaiduSearch 除外）在 .security.yml 中使用的是 `api_keys`（复数）
- 检查 YAML 中数组格式是否正确（用短横线正确缩进）
- 记住：模型、Brave、Tavily、Perplexity、Kagi 必须使用 `api_keys`（数组格式）
- GLMSearch 与 BaiduSearch 必须使用 `api_key`（单个字符串格式）

## Key 没有被应用
- 检查 .security.yml 是否与 config.json 位于同一目录
- 确认文件权限允许读取（chmod 600 ~/.chagent/.security.yml）
- 确认 YAML 结构与预期格式一致
- 检查字段名是否有拼写错误（区分大小写）
- 确认模型/渠道名完全一致（区分大小写）

## 负载均衡/故障转移问题
- 确认 api_keys 数组中的所有 API key 都有效
- 检查所有 key 是否具有相同的限流与权限
- 查看日志，确认正在使用和失败的 key
- 确认 api_keys 数组在 YAML 中格式正确
*/
package config

// 本文件仅作为文档
