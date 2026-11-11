---
name: github
description: "使用 `gh` CLI 与 GitHub 交互。用 `gh issue`、`gh pr`、`gh run` 和 `gh api` 处理 issue、PR、CI 运行以及高级查询。"
metadata: {"nanobot":{"emoji":"🐙","requires":{"bins":["gh"]},"install":[{"id":"brew","kind":"brew","formula":"gh","bins":["gh"],"label":"安装 GitHub CLI（brew）"},{"id":"apt","kind":"apt","package":"gh","bins":["gh"],"label":"安装 GitHub CLI（apt）"}]}}
---

# GitHub 技能

使用 `gh` CLI 与 GitHub 交互。当不在 git 目录中时，始终指定 `--repo owner/repo`，或直接使用 URL。

## 拉取请求

检查某个 PR 的 CI 状态：
```bash
gh pr checks 55 --repo owner/repo
```

列出最近的工作流运行：
```bash
gh run list --repo owner/repo --limit 10
```

查看某次运行并了解哪些步骤失败：
```bash
gh run view <run-id> --repo owner/repo
```

仅查看失败步骤的日志：
```bash
gh run view <run-id> --repo owner/repo --log-failed
```

## 用于高级查询的 API

`gh api` 命令适合访问其他子命令无法获取的数据。

获取包含指定字段的 PR：
```bash
gh api repos/owner/repo/pulls/55 --jq '.title, .state, .user.login'
```

## JSON 输出

大多数命令支持用 `--json` 输出结构化数据。可以用 `--jq` 进行过滤：

```bash
gh issue list --repo owner/repo --json number,title --jq '.[] | "\(.number): \(.title)"'
```
