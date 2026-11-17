# /cron 定时任务命令

`/cron` 由 Agent 本地命令系统处理，不依赖模型理解自然语言，因此可用于查看、创建、启停和删除定时任务。

## 命令

| 命令 | 说明 |
|---|---|
| `/cron list` | 列出全部定时任务（包含已停止任务） |
| `/cron get <id>` | 查看任务详情、下次执行时间、上次执行状态 |
| `/cron add ...` | 新增 Agent 定时任务 |
| `/cron stop <id>` | 停止/暂停任务（保留任务） |
| `/cron start <id>` | 开始/启用任务 |
| `/cron delete <id>` | 删除任务 |
| `/cron remove <id>` | 删除任务的同义词 |
| `/cron disable <id>` | `/cron stop` 的同义词 |
| `/cron enable <id>` | `/cron start` 的同义词 |

## 新增任务

支持三种调度方式：

```text
/cron add name="每日巡检" every_seconds=3600 message="检查主机状态"
```

```text
/cron add name="十秒后提醒" at_seconds=10 message="提醒内容"
```

```text
/cron add name="每天九点" cron_expr="0 9 * * *" message="生成日报"
```

参数：

- `name`：任务名称，可选；不填时取 `message` 前 30 个字符；
- `message`：到期后交给 Agent 执行的任务内容，必填；
- `every_seconds` / `every`：固定间隔，支持秒数或 `10m`、`1h`；
- `at_seconds` / `at`：一次性延迟；
- `cron_expr` / `cron`：cron 表达式。

Web 入口创建的定时任务只支持 Agent 任务（message），不支持直接调度 shell command。
需要调度命令时，由 Agent 在内部渠道使用 cron 工具创建。

## 生命周期

```text
/cron add ...      创建并立即启用
/cron stop <id>    暂停，任务保留，nextRunAt 清空
/cron start <id>   重新启用并重新计算 nextRunAt
/cron delete <id>  删除任务及本地 cron/jobs.json 记录
```
