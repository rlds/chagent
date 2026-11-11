---
name: summarize
description: 从 URL、播客和本地文件中总结或提取文本/转写内容（“帮我转写这个 YouTube 视频”这类请求的绝佳兜底方案）。
homepage: https://summarize.sh
metadata: {"nanobot":{"emoji":"🧾","requires":{"bins":["summarize"]},"install":[{"id":"brew","kind":"brew","formula":"steipete/tap/summarize","bins":["summarize"],"label":"安装 summarize（brew）"}]}}
---

# Summarize 技能

用于总结 URL、本地文件和 YouTube 链接的快速 CLI。

## 何时使用（触发语）

当用户提出以下任一请求时，立即使用本技能：
- “用一下 summarize.sh”
- “这个链接/视频讲了什么？”
- “总结一下这个 URL/文章”
- “转写这个 YouTube/视频”（尽力而为的转写提取；无需 `yt-dlp`）

## 快速开始

```bash
summarize "https://example.com" --model google/gemini-3-flash-preview
summarize "/path/to/file.pdf" --model google/gemini-3-flash-preview
summarize "https://youtu.be/dQw4w9WgXcQ" --youtube auto
```

## YouTube：摘要与转写记录

尽力而为的转写记录（仅支持 URL）：

```bash
summarize "https://youtu.be/dQw4w9WgXcQ" --youtube auto --extract-only
```

如果用户要求的是转写记录但内容非常庞大，先返回一份精简摘要，然后再询问要展开哪个章节/时间区间。

## 模型与密钥

为所选提供商设置 API key：
- OpenAI：`OPENAI_API_KEY`
- Anthropic：`ANTHROPIC_API_KEY`
- xAI：`XAI_API_KEY`
- Google：`GEMINI_API_KEY`（别名：`GOOGLE_GENERATIVE_AI_API_KEY`、`GOOGLE_API_KEY`）

若未做任何设置，默认模型为 `google/gemini-3-flash-preview`。

## 常用参数

- `--length short|medium|long|xl|xxl|<chars>`
- `--max-output-tokens <count>`
- `--extract-only`（仅限 URL）
- `--json`（机器可读）
- `--firecrawl auto|off|always`（回退提取）
- `--youtube auto`（若设置了 `APIFY_API_TOKEN`，则走 Apify 回退）

## 配置

可选配置文件：`~/.summarize/config.json`

```json
{ "model": "openai/gpt-5.4" }
```

可选服务：
- `FIRECRAWL_API_KEY` 用于被封锁的站点
- `APIFY_API_TOKEN` 用于 YouTube 回退