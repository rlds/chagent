# 集成测试套件

本目录包含 CI 在合并 PR 之前以及构建 `main` 之前运行的集成测试套件。

这些测试用于捕获仅靠单元测试容易遗漏的回归，尤其是当不同的 PR 触及相邻代码路径、且只有各部分连接起来后才会暴露故障的情况。典型示例包括：

- 跨包边界的协议兼容性
- 真实传输上的请求与响应行为
- 子进程、CLI 或容器接线
- 通过环境变量传入的配置
- 涉及多个组件的启动、发现与拆除流程

## 两层集成测试

Chagent 目前使用两种相关机制：

1. Go 集成测试，通常位于 `*_integration_test.go` 文件中，并由 `//go:build integration` 守护
2. `integration/suites/` 下基于 Docker 的套件，它们在 CI 中启动真实依赖并运行其中一个或多个上述 Go 测试

这一区别很重要：

- 带标签的 Go 集成测试是测试实现
- 基于 Docker 的套件是让该测试可复现且对 CI 安全的方式

如果某个测试应主动保护 PR 之间的合并，它应当能通过 [`scripts/run-integration-tests.sh`](../scripts/run-integration-tests.sh) 运行到，无论是扩展现有套件还是新增套件。

有些带集成标签的测试有意设为选择性启用，不属于 Docker 套件，例如依赖外部二进制或凭据的真实 provider 冒烟测试。这些测试对人工验证有用，但不作为 PR 合并的门禁。

## CI 运行的内容

[`.github/workflows/pr.yml`](../.github/workflows/pr.yml) 和 [`.github/workflows/build.yml`](../.github/workflows/build.yml) 中的集成任务都会执行：

```bash
bash ./scripts/run-integration-tests.sh
```

运行器会自动发现 `integration/suites/` 下的每个套件，因此新增套件无需编辑 GitHub Actions 工作流。

## 运行器的工作方式

- 共享运行器定义在 [`integration/docker-compose.runner.yml`](docker-compose.runner.yml) 中。
- 每个套件位于 `integration/suites/<suite-name>/` 下。
- 运行脚本加载 `suite.env`，将共享 compose 文件与套件专属 compose 文件合并，启动依赖服务，运行套件命令，然后拆除所有内容。
- 共享运行器容器设置 `GOFLAGS=-tags=goolm,stdjson,integration`，因此测试使用与 CI 相同的构建标签运行。

实践中，每个套件为我们提供：

- 确定性的依赖图
- 稳定的执行环境
- 运行结束后的自动清理
- 一个干净的位置，用来精确表达我们想要防止的回归

## 当前参考套件

[`integration/suites/mcp-streamable/`](suites/mcp-streamable/) 是当前的参考示例。

它做三件事：

- 从 [`integration/fixtures/mcp-streamable-server/`](fixtures/mcp-streamable-server/) 构建并启动一个夹具 MCP 服务器
- 通过环境变量将连接详情注入运行器容器
- 运行 [`TestIntegration_RealConfiguredServer`](../pkg/mcp/manager_real_server_integration_test.go)，以验证 Chagent 能连接到真实服务器、发现工具、调用其中一个工具并验证响应负载

该套件与 [`TestIntegration_StreamableHTTPCompatibility`](../pkg/mcp/manager_integration_test.go) 互补，后者在进程内测试同一区域。二者共同覆盖协议行为和真实服务接线。

## 套件布局

每个套件目录必须包含：

- `suite.env`
- 至少一个 `docker-compose.yml` 或 `docker-compose.*.yml`

示例：

```text
integration/suites/my-suite/
├── docker-compose.yml
└── suite.env
```

## 必需的清单字段

`suite.env` 由运行脚本通过 `source` 加载，必须定义：

- `TEST_COMMAND`：在集成运行器容器内执行的 shell 命令

可选字段：

- `RUNNER_SERVICE`：覆盖默认运行器服务名（`integration-runner`）

示例：

```bash
TEST_COMMAND='go test ./pkg/mcp -run TestIntegration_RealConfiguredServer -v'
```

## 本地运行集成测试

### 前置条件

- 对于基于 Docker 的套件，需要带 `docker compose` 插件的 Docker
- 仅当你想直接在宿主机上而不是通过 Docker 运行带标签的集成测试时，才需要 Go 1.25+

### 运行 CI 所运行的全部内容

```bash
make integration-test
```

等价的直接命令：

```bash
bash ./scripts/run-integration-tests.sh
```

### 运行单个套件

```bash
bash ./scripts/run-integration-tests.sh mcp-streamable
```

这是精确复现 CI 集成任务针对单个套件所做操作的最快方式。

### 直接运行带标签的集成测试

编写测试时为了更快迭代，可以不用 Docker 直接运行 Go 测试：

```bash
go test -tags=goolm,stdjson,integration ./pkg/mcp -run TestIntegration_StreamableHTTPCompatibility -v
```

如果提供与 Docker 套件会注入的相同环境变量，你也可以直接运行真实服务器冒烟测试：

```bash
CHAGENT_MCP_REAL_SERVER_JSON='{"enabled":true,"type":"http","url":"http://127.0.0.1:8080/mcp"}' \
CHAGENT_MCP_REAL_TOOL_NAME=echo \
CHAGENT_MCP_REAL_TOOL_ARGS_JSON='{"message":"hello"}' \
CHAGENT_MCP_REAL_EXPECT_SUBSTRING=hello \
go test -tags=goolm,stdjson,integration ./pkg/mcp -run TestIntegration_RealConfiguredServer -v
```

注意事项：

- 避免 `-short`：当前集成测试在 short 模式下会跳过
- 使用直接 `go test` 获得紧凑的反馈循环，然后在提交前用 Docker 套件验证

## 何时添加集成测试

当风险存在于交互中而非仅存在于函数体内时，就应使用集成测试。合适的候选包括：

- 跨越进程或容器边界的代码
- 特定于传输的行为，例如 HTTP、SSE、stdio 或 streamable MCP 流程
- 依赖真实子进程执行的 CLI 解析与接线
- 通过文件、环境变量、头部或服务发现进行的配置传播
- 通常只有在合并多个各自合理的 PR 后才出现的回归

当行为是纯的、局部的且完全可在进程内控制时，优先使用单元测试。

## 如何添加新的集成测试

### 1. 从想要防止的回归开始

描述合并后可能中断的真实工作流。场景越清晰，测试就越经得起时间考验。

示例：

- “在传输规范化变更后，Chagent 仍能连接到 streamable MCP 服务器。”
- “在响应处理重构后，provider 包装器仍能解析真实的 CLI 输出格式。”

### 2. 实现 Go 测试

在负责该行为的包中添加或扩展 `*_integration_test.go` 文件，并用以下构建标签守护：

```go
//go:build integration
```

准则：

- 让断言聚焦于可观察行为
- 使用有界超时
- 优先使用确定性夹具，而不是互联网访问或共享外部状态
- 仅在依赖有意可选时跳过，例如本地安装的第三方 CLI

### 3. 决定它是否必须作为 CI 合并门禁

使用以下经验法则：

- 如果测试只是手动冒烟检查，带标签的 Go 测试可能就足够
- 如果测试应防止回归通过 PR 合并落地，请将其接入基于 Docker 的套件

### 4. 复用或新增套件

如果现有套件已经测试同一子系统，请扩展它。否则创建：

```text
integration/suites/<name>/
├── docker-compose.yml
└── suite.env
```

使用 `integration/fixtures/` 放置可复用的辅助服务或假服务器。

### 5. 定义套件命令

在 `suite.env` 中，将 `TEST_COMMAND` 指向你希望 CI 运行的 Go 测试。

示例：

```bash
TEST_COMMAND='go test ./pkg/mcp -run TestIntegration_RealConfiguredServer -v'
```

```bash
TEST_COMMAND='go test ./pkg/somepkg -run TestIntegration_MyScenario -v'
```

如果多个测试共享同一环境，你也可以运行它们，但请保持套件内聚且失败时易于诊断。

### 6. 在 Docker Compose 中建模依赖

套件 compose 文件可以：

- 定义测试所需的依赖服务
- 扩展或覆盖共享的 `integration-runner` 服务
- 向运行器注入环境变量供测试使用

实用建议：

- 优先使用 Docker 服务名而不是硬编码主机端口
- 当运行器依赖服务就绪状态时，添加健康检查
- 保持套件自包含且确定性

### 7. 提交前在本地验证

至少：

```bash
go test -tags=goolm,stdjson,integration ./path/to/package -run TestIntegration_Name -v
bash ./scripts/run-integration-tests.sh <suite-name>
```

第一条命令便于编写阶段使用。第二条证明 CI 路径端到端可用。

## 新套件审查清单

在打开 PR 之前，检查新套件：

- 复现真实的跨组件故障模式
- 具有确定性且相互隔离
- 不需要在 CI 中手动设置
- 具有清晰的失败输出
- 在合理时间内完成
- 通过常规运行器拆除自行清理

提交后，该套件将被 CI 集成任务自动发现。
