---
name: agent-browser
description: "通过 agent-browser CLI 实现浏览器自动化。适用于用户需要浏览网站、填写表单、点击按钮、截图、提取数据或测试 Web 应用的场景。"
metadata: {"nanobot":{"emoji":"🌐","requires":{"bins":["agent-browser"]},"install":[{"id":"npm","kind":"npm","package":"agent-browser","global":true,"bins":["agent-browser"],"label":"安装 agent-browser（npm）"}]}}
---

# Agent Browser 技能

通过 Chrome/Chromium CDP 实现的 CLI 浏览器自动化。安装：`npm i -g agent-browser && agent-browser install`。

**使用本技能前**，请先运行 `which agent-browser` 确认该工具可用。若找不到该命令，请告知用户：浏览器自动化需要 `agent-browser` CLI 与 Chromium，它们仅存在于 heavy 容器镜像中。不要在运行时尝试安装。

## 核心工作流

1. `agent-browser open <url>` —— 打开并导航
2. `agent-browser snapshot -i` —— 获取带引用标记（`@e1`、`@e2`……）的可交互元素
3. 使用引用标记进行交互 —— `click @e1`、`fill @e2 "text"`
4. 任何导航或 DOM 变更后都要重新执行 snapshot —— 引用标记会失效

```bash
agent-browser open https://example.com/form
agent-browser snapshot -i
# @e1 [input] "Email", @e2 [input] "Password", @e3 [button] "Submit"
agent-browser fill @e1 "user@example.com"
agent-browser fill @e2 "secret"
agent-browser click @e3
agent-browser wait --load networkidle
agent-browser snapshot -i
```

当不需要中间输出时，用 `&&` 串联命令：
```bash
agent-browser open https://example.com && agent-browser wait --load networkidle && agent-browser snapshot -i
```

## 命令

```bash
# 导航
agent-browser open <url>
agent-browser close

# 快照
agent-browser snapshot -i                # 带引用标记的可交互元素
agent-browser snapshot -s "#selector"    # 限定到 CSS 选择器

# 交互（使用快照中的 @ 引用）
agent-browser click @e1
agent-browser fill @e2 "text"            # 先清空再输入
agent-browser type @e2 "text"            # 直接输入（不清空）
agent-browser select @e1 "option"
agent-browser check @e1
agent-browser press Enter
agent-browser scroll down 500

# 获取信息
agent-browser get text @e1
agent-browser get url
agent-browser get title

# 等待
agent-browser wait @e1                   # 等待元素出现
agent-browser wait --load networkidle    # 等待网络空闲
agent-browser wait --url "**/dashboard"  # 等待 URL 匹配指定模式
agent-browser wait --text "Welcome"      # 等待指定文本出现
agent-browser wait 2000                  # 等待指定毫秒数

# 截图与导出
agent-browser screenshot                 # 截图到临时目录
agent-browser screenshot --full          # 整页截图
agent-browser screenshot --annotate      # 附带编号元素标签（[N] -> @eN）
agent-browser pdf output.pdf

# 语义定位器（引用不可用时使用）
agent-browser find text "Sign In" click
agent-browser find label "Email" fill "user@test.com"
agent-browser find role button click --name "Submit"
```

## 身份认证

```bash
# 选项 1：从用户正在运行的 Chrome 导入
agent-browser --auto-connect state save ./auth.json
agent-browser --state ./auth.json open https://app.example.com

# 选项 2：持久化 profile
agent-browser --profile ~/.myapp open https://app.example.com/login
# ……只需登录一次，之后每次运行都已认证

# 选项 3：会话名（自动保存/恢复）
agent-browser --session-name myapp open https://app.example.com/login
# ……登录并关闭，下次运行时状态会自动恢复

# 选项 4：状态文件
agent-browser state save auth.json
agent-browser state load auth.json
```

## 内嵌框架（iframe）

快照中会内联 iframe 的内容。可直接与 iframe 的引用标记交互 —— 无需切换 frame。

## 并行会话

```bash
agent-browser --session s1 open https://site-a.com
agent-browser --session s2 open https://site-b.com
agent-browser session list
```

## JavaScript 求值

```bash
agent-browser eval 'document.title'

# 复杂 JS —— 使用 --stdin 避免 shell 引号问题
agent-browser eval --stdin <<'EVALEOF'
JSON.stringify(Array.from(document.querySelectorAll("a")).map(a => a.href))
EVALEOF
```

## 清理

完成后请始终关闭会话：
```bash
agent-browser close
agent-browser --session s1 close
```
