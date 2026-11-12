// Chagent - 超轻量个人 AI 智能体

package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rlds/chagent/pkg/tools"
)

// toolBatchDefaultParallelism 是单个模型步内并行安全工具调用所用滚动并发池的默认大小。
const toolBatchDefaultParallelism = 4

// toolBatchAbortedBeforeDispatch 是因批次中止而计划过但从未派发的工具调用所合成的工具结果内容。
//
// 该文本模型可见，并有意说明发生了什么：调度器绝不能让工具调用缺少工具结果，
// 否则 provider 对话与 replay/resume 逻辑会出现不平衡的工具调用/结果尾部。
const toolBatchAbortedBeforeDispatch = "工具调用在派发前被中止"

// toolBatchItem 是批次中一个已规划的工具调用。Index 是该调用在模型 tool_calls 数组中的位置；
// 无论派发/完成顺序如何，调度器都按相同顺序返回结果。
type toolBatchItem struct {
	Index int
	Tool  string
	Args  map[string]any
}

// toolBatchOutcome 是一个已规划调用的最终结果。
//
//   - 对已执行调用（含错误结果）以及因中止而跳过的调用（合成的 ErrorResult），Result 非 nil。
//   - Skipped 表示调用从未派发；SkipReason 携带人类可读原因，并同步到 Result.ForLLM。
type toolBatchOutcome struct {
	Index      int
	Result     *tools.ToolResult
	Skipped    bool
	SkipReason string

	// StartedAt/Duration 记录实际执行的时间窗，供 execution.report 使用。
	// 被跳过（未派发）的调用保持零值。
	StartedAt time.Time
	Duration  time.Duration
}

// execToolBatchFn 执行一次工具调用。返回错误会被视为仅限该调用的工具失败；
// 该错误由调度器转换为 ErrorResult。
type execToolBatchFn func(
	ctx context.Context,
	index int,
	tool string,
	args map[string]any,
) (*tools.ToolResult, error)

// runToolBatch 按 DSH 批次语义调度工具调用：
//
//   - 连续的并行安全调用（见 isSafe）通过最多 maxParallel 个 goroutine 的滚动并发池派发。
//   - 非并行安全调用独占运行并充当屏障：它只会在批次中此前所有调用结束后开始，且后续并行组只在它结束后开始。
//   - 派发顺序是并行的，但返回结果按模型顺序排列。
//   - 失败的调用（返回错误、panic 或 nil 结果）只为该调用产生错误结果；批次其余部分继续。
//   - 当 abort 触发（通道关闭）或 ctx 结束时，不再派发新调用，等待在途调用，并给每个未派发调用
//     合成错误结果。
//
// isSafe 是唯一的并行安全判定，分组与准入必须使用同一语义。调用方应传入基于工具注册表
// 的能力判定（tools.IsToolParallelSafe）：已注册工具按其 ConcurrencySafe 能力，未注册/未知
// 工具保守地串行。nil 仅用于测试/兼容旧调用点，此时退回名称级允许列表（tools.IsParallelSafe）。
//
// 返回切片始终有 len(items) 个条目，且 outcomes[i].Index == items[i].Index，
// 保持“每个工具调用都有工具结果”的不变量。
func runToolBatch(
	ctx context.Context,
	items []toolBatchItem,
	execFn execToolBatchFn,
	maxParallel int,
	abort <-chan struct{},
	isSafe func(tool string) bool,
) []toolBatchOutcome {
	outcomes := make([]toolBatchOutcome, len(items))
	for i, item := range items {
		outcomes[i] = toolBatchOutcome{Index: item.Index}
	}
	if len(items) == 0 {
		return outcomes
	}
	if maxParallel <= 0 {
		maxParallel = toolBatchDefaultParallelism
	}
	if execFn == nil {
		skipToolBatchRange(outcomes, items, 0, len(items), "工具批次没有执行器")
		return outcomes
	}
	if isSafe == nil {
		isSafe = tools.IsParallelSafe
	}

	next := 0
	for next < len(items) {
		if toolBatchAborted(ctx, abort) {
			skipToolBatchRange(outcomes, items, next, len(items), toolBatchAbortedBeforeDispatch)
			return outcomes
		}

		// 独占调用：单独运行的屏障。
		if !isSafe(items[next].Tool) {
			outcomes[next] = executeToolBatchItem(ctx, items[next], execFn)
			next++
			continue
		}

		// 连续的并行安全调用组成一个滚动池组。
		end := next + 1
		for end < len(items) && isSafe(items[end].Tool) {
			end++
		}
		runToolBatchGroup(
			ctx,
			items[next:end],
			outcomes[next:end],
			execFn,
			maxParallel,
			abort,
		)
		if toolBatchAborted(ctx, abort) {
			skipToolBatchRange(outcomes, items, end, len(items), toolBatchAbortedBeforeDispatch)
			return outcomes
		}
		next = end
	}
	return outcomes
}

// runToolBatchGroup 通过滚动并发池派发一组最大的并行安全调用。
// outcomes 是父结果数组中该组的切片，因此在下标 i 的写入会落到正确的模型下标。
func runToolBatchGroup(
	ctx context.Context,
	items []toolBatchItem,
	outcomes []toolBatchOutcome,
	execFn execToolBatchFn,
	maxParallel int,
	abort <-chan struct{},
) {
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	stopped := false
	for i := range items {
		if stopped || toolBatchAborted(ctx, abort) {
			stopped = true
			skipToolBatchRange(outcomes, items, i, len(items), toolBatchAbortedBeforeDispatch)
			break
		}
		sem <- struct{}{}
		// 我们在等待槽位时批次可能已中止；此时不得再派发该调用。
		if toolBatchAborted(ctx, abort) {
			<-sem
			skipToolBatchRange(outcomes, items, i, len(items), toolBatchAbortedBeforeDispatch)
			stopped = true
			break
		}
		wg.Add(1)
		go func(slot int, item toolBatchItem) {
			defer wg.Done()
			defer func() { <-sem }()
			outcomes[slot] = executeToolBatchItem(ctx, item, execFn)
		}(i, items[i])
	}
	wg.Wait()
}

// executeToolBatchItem 执行一次调用，并把所有失败模式归一化为 ErrorResult，
// 使故障工具绝不会拖垮调度器或回合。
func executeToolBatchItem(
	ctx context.Context,
	item toolBatchItem,
	execFn execToolBatchFn,
) (outcome toolBatchOutcome) {
	outcome = toolBatchOutcome{Index: item.Index}
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome.Result = tools.ErrorResult(
				fmt.Sprintf("工具执行发生 panic：%v", recovered),
			)
			outcome.Skipped = false
			outcome.SkipReason = ""
			return
		}
		if outcome.Result == nil {
			outcome.Result = tools.ErrorResult("工具返回了 nil 结果")
		}
	}()

	outcome.StartedAt = time.Now()
	result, err := execFn(ctx, item.Index, item.Tool, item.Args)
	outcome.Duration = time.Since(outcome.StartedAt)
	if err != nil {
		outcome.Result = tools.ErrorResult(fmt.Sprintf("工具执行失败：%v", err))
		return outcome
	}
	outcome.Result = result
	return outcome
}

// skipToolBatchRange 用合成的中止结果填充 outcomes[from:to]。
func skipToolBatchRange(
	outcomes []toolBatchOutcome,
	items []toolBatchItem,
	from, to int,
	reason string,
) {
	if from < 0 {
		from = 0
	}
	if to > len(outcomes) {
		to = len(outcomes)
	}
	if from >= to {
		return
	}
	if reason == "" {
		reason = toolBatchAbortedBeforeDispatch
	}
	for i := from; i < to; i++ {
		outcomes[i] = toolBatchOutcome{
			Index:      items[i].Index,
			Result:     tools.ErrorResult(reason),
			Skipped:    true,
			SkipReason: reason,
		}
	}
}

// toolBatchAborted 报告批次是否必须停止派发新调用。
func toolBatchAborted(ctx context.Context, abort <-chan struct{}) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if abort == nil {
		return false
	}
	select {
	case <-abort:
		return true
	default:
		return false
	}
}
