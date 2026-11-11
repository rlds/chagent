package commands

import (
	"context"
	"fmt"
)

type Outcome int

const (
	// OutcomePassthrough 表示该输入应继续走正常的 agent 流程。
	OutcomePassthrough Outcome = iota
	// OutcomeHandled 表示已执行某个命令处理器（无论处理器是否出错）。
	OutcomeHandled
)

type ExecuteResult struct {
	Outcome Outcome
	Command string
	Err     error
}

type Executor struct {
	reg *Registry
	rt  *Runtime
}

func NewExecutor(reg *Registry, rt *Runtime) *Executor {
	return &Executor{reg: reg, rt: rt}
}

// Execute 实现两态命令决策：
// 1) handled（已处理）：立即执行命令；
// 2) passthrough（透传）：不是命令，或有意交由 agent 逻辑处理。
func (e *Executor) Execute(ctx context.Context, req Request) ExecuteResult {
	cmdName, ok := parseCommandName(req.Text)
	if !ok {
		return ExecuteResult{Outcome: OutcomePassthrough}
	}

	if e == nil || e.reg == nil {
		return ExecuteResult{Outcome: OutcomePassthrough, Command: cmdName}
	}

	def, found := e.reg.Lookup(cmdName)
	if !found {
		return ExecuteResult{Outcome: OutcomePassthrough, Command: cmdName}
	}

	return e.executeDefinition(ctx, req, def)
}

func (e *Executor) executeDefinition(ctx context.Context, req Request, def Definition) ExecuteResult {
	// 确保 Reply 始终非 nil，这样处理器就无需再检查。
	if req.Reply == nil {
		req.Reply = func(string) error { return nil }
	}

	// 简单命令——没有子命令
	if len(def.SubCommands) == 0 {
		if def.Handler == nil {
			return ExecuteResult{Outcome: OutcomePassthrough, Command: def.Name}
		}
		err := def.Handler(ctx, req, e.rt)
		return ExecuteResult{Outcome: OutcomeHandled, Command: def.Name, Err: err}
	}

	// 子命令路由
	subName := nthToken(req.Text, 1)
	if subName == "" {
		err := req.Reply("用法：" + def.EffectiveUsage())
		return ExecuteResult{Outcome: OutcomeHandled, Command: def.Name, Err: err}
	}

	normalized := normalizeCommandName(subName)
	for _, sc := range def.SubCommands {
		if normalizeCommandName(sc.Name) == normalized {
			if sc.Handler == nil {
				return ExecuteResult{Outcome: OutcomePassthrough, Command: def.Name}
			}
			err := sc.Handler(ctx, req, e.rt)
			return ExecuteResult{Outcome: OutcomeHandled, Command: def.Name, Err: err}
		}
	}

	// 未知子命令
	err := req.Reply(fmt.Sprintf("未知选项：%s。用法：%s", subName, def.EffectiveUsage()))
	return ExecuteResult{Outcome: OutcomeHandled, Command: def.Name, Err: err}
}
