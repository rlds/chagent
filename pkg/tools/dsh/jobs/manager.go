// Package jobs 为 Chagent 实现 DSH 后台 job 工具族：
// `job_run`、`job_list`、`job_output` 和 `job_kill`。
//
// 它是对 DSH `packages/jobs/tool-jobs` 面向模型的管控功能（基于
// `packages/jobs/jobs` + `packages/jobs/jobs-local` 的 job_output /
// job_list / job_kill）的 Go 重写，并扩展出一个专用的 `job_run` 生产者，
// 因为 Chagent 没有像 DSH 那样的共享 bash job 注册表：DSH 的 `bash` 工具
// 用 `ctx.jobs` 注册后台 job，而这里的生产者是 `job_run`。
//
// 设计说明 / 与 DSH 的差异（有意记录在此）：
//   - (Go) 每个 job 拥有独立的进程组（`Setpgid`），因此 `job_kill` 会终止
//     整棵进程树，而不只是 shell 包装进程；
//   - 输出会持久写入
//     `<workspace>/.chagent/jobs/<id>/output.log`（目录 0700，文件 0600），
//     并按字节偏移增量读取，而不是 DSH 的内存输出环形缓冲加可选落盘；
//   - `job_output` 还支持 `tail_lines`（DSH 只读取增量）；
//   - job 完成情况通过 `wait`/`timeout_ms` 观察，但本包没有 DSH 的完成通知 /
//     空闲属主唤醒机制：调用方（agent 循环）必须轮询或调用
//     `job_output(wait=true)`；
//   - `job_run` 接受每 job 的 `timeout_ms`；DSH 的超时则是生产者选项。
//     超期的 job 会以 `killed` 结束，detail 为 `timeout`。
//
// 该 manager 可安全地并发使用。所有文件系统写入都是原子的
// （临时文件 + fsync + rename），且限制在配置的工作区内。
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rlds/chagent/pkg/logger"
	"sync"
	"time"
)

// Job 状态。这些字符串会原样持久化到 job.json，并且是面向模型契约的一部分，
// 因此必须保持稳定。
const (
	StatusRunning  = "running"
	StatusStopping = "stopping"
	StatusComplete = "completed"
	StatusKilled   = "killed"
	StatusFailed   = "failed"
)

// Manager 各项限制的默认值。它们有意保持保守：一个下棋 agent 只需要
// 少量长时间运行的命令，而不是一座进程农场。
const (
	DefaultMaxConcurrentJobs = 8
	DefaultMaxRetainedJobs   = 50
	DefaultMaxOutputBytes    = int64(4 << 20) // 每个 job 的 output.log 为 4 MiB
	DefaultMaxReturnBytes    = 64 << 10       // 每次 job_output 读取 64 KiB
	DefaultWaitTimeout       = 30 * time.Second
	DefaultMaxWaitTimeout    = 10 * time.Minute
	DefaultKillGrace         = 3 * time.Second
	DefaultTailLines         = 200
	MaxTailLines             = 5000
)

const (
	jobsDirName   = "jobs"
	stateDirName  = ".chagent"
	outputLogName = "output.log"
	jobMetaName   = "job.json"
)

// Error 是可诊断的 job 错误。Code 是稳定的机器可读标记
// （例如 "job_not_found"）；Message 说明失败原因以及如何修正调用。
// 工具会把两者都呈现给模型。
type Error struct {
	Code    string
	Message string
}

// Error 实现 error。
func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode 返回 job Error 的稳定代码，其他错误返回 ""。
func ErrorCode(err error) string {
	var je *Error
	if errors.As(err, &je) {
		return je.Code
	}
	return ""
}

// Options 配置 Manager。零值有效，并且会选用文档中说明的默认值。
type Options struct {
	// MaxConcurrentJobs 限制同时运行的 job 数量（默认 8）。
	MaxConcurrentJobs int
	// MaxRetainedJobs 限制仍然被列出的已完成 job 数量；更早完成的
	// job 会从磁盘上清理（默认 50）。
	MaxRetainedJobs int
	// MaxOutputBytes 限制每个 job 捕获的 output.log 大小（默认 4 MiB）。
	// 超出上限的输出会被丢弃，并将该 job 标记为已截断。
	MaxOutputBytes int64
	// MaxReturnBytes 限制单次 job_output 响应的大小（默认 64 KiB）。
	MaxReturnBytes int
	// KillGrace 是 SIGTERM -> SIGKILL 之间的宽限期（默认 3s）。
	KillGrace time.Duration
	// WaitTimeout 在 job_output 设置 wait 但未给出 timeout_ms 时使用
	//（默认 30s）。
	WaitTimeout time.Duration
	// MaxWaitTimeout 限制任何一次 job_output 等待（默认 10min）。
	MaxWaitTimeout time.Duration
	// DefaultRunTimeout 在 job_run 省略 timeout_ms 时使用。零值（即
	// 默认值）表示没有截止时间。
	DefaultRunTimeout time.Duration
	// DenyPatterns 扩展内置的危险命令模式。
	DenyPatterns []*regexp.Regexp
	// AllowPatterns 非空时，要求必须匹配才能绕过拒绝检查并被接受
	//（语义与 exec 工具的允许列表相同）。
	AllowPatterns []*regexp.Regexp
	// Unrestricted 禁用工作区路径防护。对 agent 请保持 false。
	Unrestricted bool
}

func (o Options) withDefaults() Options {
	if o.MaxConcurrentJobs <= 0 {
		o.MaxConcurrentJobs = DefaultMaxConcurrentJobs
	}
	if o.MaxRetainedJobs <= 0 {
		o.MaxRetainedJobs = DefaultMaxRetainedJobs
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if o.MaxReturnBytes <= 0 {
		o.MaxReturnBytes = DefaultMaxReturnBytes
	}
	if o.KillGrace <= 0 {
		o.KillGrace = DefaultKillGrace
	}
	if o.WaitTimeout <= 0 {
		o.WaitTimeout = DefaultWaitTimeout
	}
	if o.MaxWaitTimeout <= 0 {
		o.MaxWaitTimeout = DefaultMaxWaitTimeout
	}
	if o.DefaultRunTimeout < 0 {
		o.DefaultRunTimeout = 0
	}
	return o
}

// JobView 是单个 job 稳定的、面向模型的投影。
type JobView struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	PID         int    `json:"pid"`
	Command     string `json:"command"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	OutputBytes int64  `json:"output_bytes"`
	Truncated   bool   `json:"truncated"`
	LogPath     string `json:"log_path"`
	Detail      string `json:"detail,omitempty"`
	TimedOut    bool   `json:"timed_out,omitempty"`
}

// OutputResult 是对某个 job 输出的一次增量读取或尾部读取。
type OutputResult struct {
	Job         JobView `json:"job"`
	Output      string  `json:"output"`
	NextOffset  int64   `json:"next_offset"`
	TotalBytes  int64   `json:"total_bytes"`
	Lossy       bool    `json:"lossy"`
	WaitedOut   bool    `json:"wait_timed_out,omitempty"`
	TailLines   int     `json:"tail_lines,omitempty"`
	OutputTrunc bool    `json:"output_truncated"`
}

// OutputOptions 控制 Manager.Output。
type OutputOptions struct {
	// Offset 是继续读取的字节偏移（增量读取）。
	Offset int64
	// TailLines 请求最后 N 行，而不是按偏移读取。
	TailLines int
	// Wait 阻塞直到 job 结束或 Timeout 到期。
	Wait    bool
	Timeout time.Duration
	// MaxBytes 限制本次读取（默认 Options.MaxReturnBytes）。
	MaxBytes int
}

// job 是单个 job 的内存记录。
type job struct {
	id          string
	command     string
	dir         string
	logPath     string
	cmd         *exec.Cmd
	pid         int
	startedAt   time.Time
	finishedAt  time.Time
	status      string
	exitCode    *int
	detail      string
	timedOut    bool
	outputBytes int64
	truncated   bool
	killAsked   bool
	done        chan struct{}
	doneOnce    sync.Once
	writer      *cappedWriter
	logFile     *os.File
}

// Manager 持有以工作区为作用域的 job 注册表和进程生命周期。
type Manager struct {
	workspace string
	root      string
	opts      Options

	mu       sync.Mutex
	jobs     map[string]*job
	order    []string
	seq      uint64
	closed   bool
	closedCh chan struct{}
	initErr  error

	userDeny  []*regexp.Regexp
	userAllow []*regexp.Regexp
}

// NewManager 返回一个根目录为 <workspace>/.chagent/jobs 的 manager。空的
// workspace 会产生一个 manager，其工具会报告 manager_unavailable，而不会去
// 触碰文件系统。
func NewManager(workspace string, opts Options) *Manager {
	m := &Manager{
		workspace: strings.TrimSpace(workspace),
		opts:      opts.withDefaults(),
		jobs:      make(map[string]*job),
		closedCh:  make(chan struct{}),
		userDeny:  append([]*regexp.Regexp(nil), opts.DenyPatterns...),
		userAllow: append([]*regexp.Regexp(nil), opts.AllowPatterns...),
	}
	if m.workspace == "" {
		m.initErr = newError("manager_unavailable", "jobs manager 需要工作区 workspace")
		return m
	}
	abs, err := filepath.Abs(m.workspace)
	if err != nil {
		m.initErr = newError("manager_unavailable", "无法解析工作区 %q：%v", m.workspace, err)
		return m
	}
	m.workspace = abs
	m.root = filepath.Join(abs, stateDirName, jobsDirName)
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		m.initErr = newError("manager_unavailable", "无法创建 jobs 目录 %q：%v", m.root, err)
		return m
	}
	m.cleanupStaleJobDirs()
	return m
}

// cleanupStaleJobDirs 删除上一个进程遗留的 job 目录：这些目录是 job 仍被标记
// 为 running 时该进程死亡后留下的。若记录的 pid 仍然存活，则不改动该 job
// （它可能属于另一个存活的 manager），因此重新加载绝不会杀掉同级 manager 的
// 工作。
func (m *Manager) cleanupStaleJobDirs() {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(m.root, entry.Name())
		data, readErr := os.ReadFile(filepath.Join(dir, jobMetaName))
		if readErr != nil {
			continue
		}
		var meta persistedJob
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		if meta.Status != StatusRunning && meta.Status != StatusStopping {
			continue
		}
		if meta.PID > 0 && processAlive(meta.PID) {
			continue
		}
		_ = os.RemoveAll(dir)
		logger.WarnCF("jobs", "Removed stale background job directory from previous process",
			map[string]any{"job_id": meta.ID, "pid": meta.PID, "status": meta.Status})
	}
}

// Workspace 返回 manager 根目录所在的绝对工作区（不可用时为 ""）。
func (m *Manager) Workspace() string {
	if m == nil {
		return ""
	}
	return m.workspace
}

// Root 返回 job 目录的绝对路径（不可用时为 ""）。
func (m *Manager) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}

func (m *Manager) available() error {
	if m == nil {
		return newError("manager_unavailable", "jobs manager 未初始化")
	}
	if m.initErr != nil {
		return m.initErr
	}
	return nil
}

// Run 为 command 启动一个后台 job。进程一旦启动便返回；调用方通过 Output
// 读取输出，通过 Kill 控制它。
func (m *Manager) Run(command string) (*JobView, error) {
	return m.run(command, 0)
}

// RunWithTimeout 以显式的截止时间启动一个后台 job（0 = 使用 manager 的
// DefaultRunTimeout，而它本身默认为无截止时间）。
func (m *Manager) RunWithTimeout(command string, timeout time.Duration) (*JobView, error) {
	return m.run(command, timeout)
}

func (m *Manager) run(command string, timeout time.Duration) (*JobView, error) {
	if err := m.available(); err != nil {
		return nil, err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, newError("invalid_command", "command 不能为空")
	}
	if err := m.validateCommand(command); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = m.opts.DefaultRunTimeout
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, newError("manager_closed", "jobs manager 已关闭，不能再启动 job")
	}
	if m.runningLocked() >= m.opts.MaxConcurrentJobs {
		m.mu.Unlock()
		return nil, newError(
			"too_many_jobs",
			"并发后台 job 已达上限（%d）；请先 job_kill 或等待现有 job 结束",
			m.opts.MaxConcurrentJobs,
		)
	}
	m.seq++
	id := newJobID(m.seq)
	dir := filepath.Join(m.root, id)
	m.mu.Unlock()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, newError("job_start_failed", "创建 job 目录失败：%v", err)
	}
	logPath := filepath.Join(dir, outputLogName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, newError("job_start_failed", "创建输出文件失败：%v", err)
	}
	writer := newCappedWriter(logFile, m.opts.MaxOutputBytes)

	cmd := shellCommand(command)
	cmd.Dir = m.workspace
	cmd.Stdout = writer
	cmd.Stderr = writer
	cmd.Stdin = nil
	configureProcessGroup(cmd)

	j := &job{
		id:        id,
		command:   command,
		dir:       dir,
		logPath:   logPath,
		cmd:       cmd,
		startedAt: time.Now().UTC(),
		status:    StatusRunning,
		done:      make(chan struct{}),
		writer:    writer,
		logFile:   logFile,
	}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = os.RemoveAll(dir)
		return nil, newError("job_start_failed", "启动命令失败：%v", err)
	}
	j.pid = cmd.Process.Pid

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		terminateProcessGroup(j.pid, true)
		_ = cmd.Wait()
		_ = logFile.Close()
		_ = os.RemoveAll(dir)
		return nil, newError("manager_closed", "jobs manager 已关闭，不能再启动 job")
	}
	m.jobs[id] = j
	m.order = append(m.order, id)
	m.mu.Unlock()

	m.persist(j)
	go m.wait(j)

	if timeout > 0 {
		go func() {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-j.done:
				return
			case <-timer.C:
				m.markTimedOut(j)
				m.terminateAsync(j)
			case <-m.closedCh:
				return
			}
		}()
	}
	m.mu.Lock()
	view := m.view(j)
	m.mu.Unlock()
	return view, nil
}

func (m *Manager) runningLocked() int {
	count := 0
	for _, j := range m.jobs {
		if j.status == StatusRunning || j.status == StatusStopping {
			count++
		}
	}
	return count
}

// wait 阻塞等待进程退出，结算记录、持久化，并清理旧的已完成 job。
// 每个 job 都运行在自己的 goroutine 中。
func (m *Manager) wait(j *job) {
	err := j.cmd.Wait()
	_ = j.logFile.Close()
	finished := time.Now().UTC()

	m.mu.Lock()
	j.finishedAt = finished
	j.outputBytes = j.writer.written()
	j.truncated = j.writer.truncated()
	switch {
	case j.killAsked:
		j.status = StatusKilled
	case err != nil:
		j.status = StatusFailed
	default:
		j.status = StatusComplete
	}
	if err == nil {
		code := 0
		j.exitCode = &code
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			j.exitCode = &code
			if code < 0 {
				j.detail = exitErr.Error()
			}
		} else {
			j.detail = err.Error()
		}
	}
	if j.timedOut {
		j.status = StatusKilled
		j.detail = joinDetail(j.detail, "timeout")
	}
	m.mu.Unlock()

	m.persist(j)
	m.pruneLockedFinished()
	j.doneOnce.Do(func() { close(j.done) })
}

func joinDetail(existing, extra string) string {
	if strings.TrimSpace(existing) == "" {
		return extra
	}
	return existing + "; " + extra
}

func (m *Manager) markTimedOut(j *job) {
	m.mu.Lock()
	j.timedOut = true
	j.killAsked = true
	j.status = StatusStopping
	m.mu.Unlock()
	m.persist(j)
}

// terminateAsync 请求 job 停止，且不阻塞调用方。
func (m *Manager) terminateAsync(j *job) {
	go func() {
		m.mu.Lock()
		pid := j.pid
		m.mu.Unlock()
		if pid <= 0 {
			return
		}
		terminateProcessGroup(pid, false)
		select {
		case <-j.done:
			return
		case <-time.After(m.opts.KillGrace):
			terminateProcessGroup(pid, true)
		}
	}()
}

// Kill 请求运行中的 job 停止：先向其进程组发送 SIGTERM，宽限期过后再发送
// SIGKILL。它会等待 job 结束（有上限）并返回最终视图。终止一个已经结束的
// job 是空操作。
func (m *Manager) Kill(id string) (*JobView, error) {
	if err := m.available(); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, newError("invalid_job_id", "job_id 不能为空")
	}

	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return nil, newError("job_not_found", "job %q 不存在", id)
	}
	if j.status != StatusRunning && j.status != StatusStopping {
		view := m.view(j)
		m.mu.Unlock()
		return view, nil
	}
	j.killAsked = true
	if j.status == StatusRunning {
		j.status = StatusStopping
	}
	pid := j.pid
	done := j.done
	m.mu.Unlock()

	_ = m.persistByID(id)
	terminateProcessGroup(pid, false)
	deadline := time.NewTimer(m.opts.KillGrace + 2*time.Second)
	defer deadline.Stop()
	select {
	case <-done:
	case <-deadline.C:
		terminateProcessGroup(pid, true)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}

	m.mu.Lock()
	view := m.view(j)
	m.mu.Unlock()
	return view, nil
}

// List 返回已知 job 的快照，最新的在前。status 非空时用于过滤。
func (m *Manager) List(status string, limit int) ([]JobView, error) {
	if err := m.available(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	status = strings.TrimSpace(status)
	if status != "" && !validStatus(status) {
		return nil, newError("invalid_status", "status 只允许 %s", strings.Join(statusNames(), "/"))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed && len(m.jobs) == 0 {
		return []JobView{}, nil
	}
	ids := append([]string(nil), m.order...)
	sort.Slice(ids, func(i, k int) bool {
		left, right := m.jobs[ids[i]], m.jobs[ids[k]]
		if left == nil || right == nil {
			return false
		}
		if !left.startedAt.Equal(right.startedAt) {
			return left.startedAt.After(right.startedAt)
		}
		return ids[i] < ids[k]
	})
	out := make([]JobView, 0, limit)
	for _, id := range ids {
		j := m.jobs[id]
		if j == nil {
			continue
		}
		if status != "" && j.status != status {
			continue
		}
		out = append(out, *m.view(j))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Output 以增量方式（Offset）或尾部方式（TailLines）读取捕获的输出，可选地
// 先等待 job 结束。
func (m *Manager) Output(id string, opts OutputOptions) (*OutputResult, error) {
	if err := m.available(); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, newError("invalid_job_id", "job_id 不能为空")
	}
	if opts.Offset < 0 {
		return nil, newError("invalid_output_request", "offset 不能为负")
	}
	if opts.TailLines < 0 {
		return nil, newError("invalid_output_request", "tail_lines 不能为负")
	}
	if opts.TailLines > MaxTailLines {
		return nil, newError("invalid_output_request", "tail_lines 最大 %d", MaxTailLines)
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 || maxBytes > m.opts.MaxReturnBytes {
		maxBytes = m.opts.MaxReturnBytes
	}

	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return nil, newError("job_not_found", "job %q 不存在", id)
	}
	done := j.done
	status := j.status
	waitTimeout := opts.Timeout
	if waitTimeout <= 0 {
		waitTimeout = m.opts.WaitTimeout
	}
	if waitTimeout > m.opts.MaxWaitTimeout {
		waitTimeout = m.opts.MaxWaitTimeout
	}
	closedCh := m.closedCh
	m.mu.Unlock()

	waitedOut := false
	if opts.Wait && status != StatusComplete && status != StatusKilled && status != StatusFailed {
		timer := time.NewTimer(waitTimeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			waitedOut = true
		case <-closedCh:
			waitedOut = true
		}
	}

	m.mu.Lock()
	status = j.status
	j.outputBytes = j.writer.written()
	j.truncated = j.writer.truncated()
	view := m.view(j)
	m.mu.Unlock()

	result := &OutputResult{
		Job:       *view,
		WaitedOut: waitedOut,
	}
	data, err := os.ReadFile(j.logPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, newError("output_read_failed", "读取 job 输出失败：%v", err)
	}
	total := int64(len(data))
	result.TotalBytes = total

	if opts.TailLines > 0 {
		tail := lastLines(data, opts.TailLines)
		if int64(len(tail)) > int64(maxBytes) {
			tail = tail[int64(len(tail))-int64(maxBytes):]
			result.Lossy = true
		}
		result.Output = string(tail)
		result.NextOffset = total
		result.TailLines = opts.TailLines
		return result, nil
	}

	offset := opts.Offset
	if offset > total {
		offset = total
	}
	chunk := data[offset:]
	if len(chunk) > maxBytes {
		chunk = chunk[:maxBytes]
	}
	if offset > 0 && offset < int64(len(data)) && opts.Offset == 0 {
		// 不可达：上面已做防护，保留以便理解。
		result.Lossy = true
	}
	result.Output = string(chunk)
	result.NextOffset = offset + int64(len(chunk))
	if result.NextOffset < total {
		result.OutputTrunc = true
	}
	return result, nil
}

func lastLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return nil
	}
	starts := make([]int, 1, n+1)
	starts[0] = 0
	for i, b := range data {
		if b == '\n' && i+1 < len(data) {
			starts = append(starts, i+1)
		}
	}
	if len(starts) <= n {
		return data
	}
	return data[starts[len(starts)-n]:]

}

// Close 终止所有运行中的 job（SIGTERM，宽限期过后再 SIGKILL），并拒绝后续的
// Run 调用。它是幂等的，可以在 agent 关闭时安全调用，包括从信号处理
// goroutine 中调用。
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.closedCh)
	type runningJob struct {
		j    *job
		pid  int
		done chan struct{}
	}
	running := make([]runningJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		if j.status == StatusRunning || j.status == StatusStopping {
			j.killAsked = true
			running = append(running, runningJob{j: j, pid: j.pid, done: j.done})
		}
	}
	m.mu.Unlock()

	for _, r := range running {
		terminateProcessGroup(r.pid, false)
	}
	graceDeadline := time.Now().Add(m.opts.KillGrace)
	for _, r := range running {
		remain := time.Until(graceDeadline)
		if remain <= 0 {
			break
		}
		timer := time.NewTimer(remain)
		select {
		case <-r.done:
		case <-timer.C:
		}
		timer.Stop()
	}
	// 升级处理仍存活的进程，然后有上限地等待 wait() 结算各记录。
	for _, r := range running {
		select {
		case <-r.done:
			continue
		default:
			terminateProcessGroup(r.pid, true)
		}
	}
	hardStop := time.Now().Add(3 * time.Second)
	for _, r := range running {
		remain := time.Until(hardStop)
		if remain <= 0 {
			break
		}
		select {
		case <-r.done:
		case <-time.After(remain):
		}
	}
	return nil
}

// Closed 报告是否已调用 Close。
func (m *Manager) Closed() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// view 渲染一个 job。调用方必须持有 m.mu（或独占拥有该 job）。
func (m *Manager) view(j *job) *JobView {
	v := &JobView{
		ID:          j.id,
		Status:      j.status,
		PID:         j.pid,
		Command:     j.command,
		StartedAt:   j.startedAt.Format(time.RFC3339),
		OutputBytes: j.outputBytes,
		Truncated:   j.truncated,
		LogPath:     j.logPath,
		Detail:      j.detail,
		TimedOut:    j.timedOut,
	}
	if !j.finishedAt.IsZero() {
		v.FinishedAt = j.finishedAt.Format(time.RFC3339)
	}
	if j.exitCode != nil {
		code := *j.exitCode
		v.ExitCode = &code
	}
	return v
}

func validStatus(status string) bool {
	switch status {
	case StatusRunning, StatusStopping, StatusComplete, StatusKilled, StatusFailed:
		return true
	}
	return false
}

func statusNames() []string {
	return []string{StatusRunning, StatusStopping, StatusComplete, StatusKilled, StatusFailed}
}

func newJobID(seq uint64) string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("job-%d-%s", seq, hex.EncodeToString(buf[:]))
}

// persist 原子地写入 job.json。它绝不会导致 job 失败；持久化属于诊断状态，
// 写入错误在工具层以不含 Detail 的日志反映之后会被有意忽略。
func (m *Manager) persist(j *job) {
	if j == nil || j.dir == "" {
		return
	}
	m.mu.Lock()
	meta := persistedJob{
		ID:          j.id,
		Command:     j.command,
		Status:      j.status,
		PID:         j.pid,
		StartedAt:   j.startedAt.Format(time.RFC3339),
		OutputBytes: j.outputBytes,
		Truncated:   j.truncated,
		TimedOut:    j.timedOut,
		Detail:      j.detail,
	}
	if !j.finishedAt.IsZero() {
		meta.FinishedAt = j.finishedAt.Format(time.RFC3339)
	}
	if j.exitCode != nil {
		code := *j.exitCode
		meta.ExitCode = &code
	}
	m.mu.Unlock()
	writePersistedJob(j.dir, meta)
}

func (m *Manager) persistByID(id string) error {
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j == nil {
		return nil
	}
	m.persist(j)
	return nil
}

// persistedJob 是磁盘上 job.json 的结构。
type persistedJob struct {
	ID          string `json:"id"`
	Command     string `json:"command"`
	Status      string `json:"status"`
	PID         int    `json:"pid"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	OutputBytes int64  `json:"output_bytes"`
	Truncated   bool   `json:"truncated"`
	TimedOut    bool   `json:"timed_out,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

func writePersistedJob(dir string, meta persistedJob) {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return
	}
	data = append(data, '\n')
	path := filepath.Join(dir, jobMetaName)
	tmp, err := os.CreateTemp(dir, ".job-*.json")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return
	}
	if _, err := tmp.Write(data); err != nil {
		return
	}
	if err := tmp.Sync(); err != nil {
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return
	}
	cleanup = false
}

// pruneLockedFinished 在保留数量超过 MaxRetainedJobs 后删除最旧的已完成 job
// 目录。运行中的 job 永远不会被清理。
func (m *Manager) pruneLockedFinished() {
	m.mu.Lock()
	finished := make([]*job, 0, len(m.jobs))
	for _, j := range m.jobs {
		if j.status == StatusRunning || j.status == StatusStopping {
			continue
		}
		finished = append(finished, j)
	}
	sort.Slice(finished, func(i, k int) bool {
		return finished[i].finishedAt.Before(finished[k].finishedAt)
	})
	remove := len(finished) - m.opts.MaxRetainedJobs
	if remove <= 0 {
		m.mu.Unlock()
		return
	}
	dirs := make([]string, 0, remove)
	ids := make([]string, 0, remove)
	for i := 0; i < remove; i++ {
		dirs = append(dirs, finished[i].dir)
		ids = append(ids, finished[i].id)
	}
	for _, id := range ids {
		delete(m.jobs, id)
	}
	m.mu.Unlock()

	for _, dir := range dirs {
		_ = os.RemoveAll(dir)
	}
}
