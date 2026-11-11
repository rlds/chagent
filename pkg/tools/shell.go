package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/constants"
	"github.com/rlds/chagent/pkg/isolation"
	"github.com/rlds/chagent/pkg/logger"
)

var (
	globalSessionManager = NewSessionManager()
	sessionManagerMu     sync.RWMutex
)

func getSessionManager() *SessionManager {
	sessionManagerMu.RLock()
	defer sessionManagerMu.RUnlock()
	return globalSessionManager
}

type ExecTool struct {
	workingDir          string
	timeout             time.Duration
	denyPatterns        []*regexp.Regexp
	allowPatterns       []*regexp.Regexp
	customAllowPatterns []*regexp.Regexp
	allowedPathPatterns []*regexp.Regexp
	restrictToWorkspace bool
	allowRemote         bool
	sessionManager      *SessionManager
}

var (
	defaultDenyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\brm\s+-[rf]{1,2}\b`),
		regexp.MustCompile(`\bdel\s+/[fq]\b`),
		regexp.MustCompile(`\brmdir\s+/s\b`),
		// 匹配磁盘擦除命令（其后必须跟空格/参数）
		regexp.MustCompile(
			`(^|[^-\w])\b(format|mkfs|diskpart)\b\s`,
		),
		regexp.MustCompile(`\bdd\s+if=`),
		// 阻止写入块设备（涵盖所有常见命名方式）。
		regexp.MustCompile(
			`>\s*/dev/(sd[a-z]|hd[a-z]|vd[a-z]|xvd[a-z]|nvme\d|mmcblk\d|loop\d|dm-\d|md\d|sr\d|nbd\d)`,
		),
		regexp.MustCompile(`\b(shutdown|reboot|poweroff)\b`),
		regexp.MustCompile(`:\(\)\s*\{.*\};\s*:`),
		regexp.MustCompile(`\$\([^)]+\)`),
		regexp.MustCompile(`\$\{[^}]+\}`),
		regexp.MustCompile("`[^`]+`"),
		regexp.MustCompile(`\|\s*sh\b`),
		regexp.MustCompile(`\|\s*bash\b`),
		regexp.MustCompile(`;\s*rm\s+-[rf]`),
		regexp.MustCompile(`&&\s*rm\s+-[rf]`),
		regexp.MustCompile(`\|\|\s*rm\s+-[rf]`),
		regexp.MustCompile(`<<\s*EOF`),
		regexp.MustCompile(`\$\(\s*cat\s+`),
		regexp.MustCompile(`\$\(\s*curl\s+`),
		regexp.MustCompile(`\$\(\s*wget\s+`),
		regexp.MustCompile(`\$\(\s*which\s+`),
		regexp.MustCompile(`\bsudo\b`),
		regexp.MustCompile(`\bchmod\s+[0-7]{3,4}\b`),
		regexp.MustCompile(`\bchown\b`),
		regexp.MustCompile(`\bpkill\b`),
		regexp.MustCompile(`\bkillall\b`),
		regexp.MustCompile(`\bkill\b`),
		regexp.MustCompile(`\bcurl\b.*\|\s*(sh|bash)`),
		regexp.MustCompile(`\bwget\b.*\|\s*(sh|bash)`),
		regexp.MustCompile(`\bnpm\s+install\s+-g\b`),
		regexp.MustCompile(`\bpip\s+install\s+--user\b`),
		regexp.MustCompile(`\bapt\s+(install|remove|purge)\b`),
		regexp.MustCompile(`\byum\s+(install|remove)\b`),
		regexp.MustCompile(`\bdnf\s+(install|remove)\b`),
		regexp.MustCompile(`\bdocker\s+run\b`),
		regexp.MustCompile(`\bdocker\s+exec\b`),
		regexp.MustCompile(`\bgit\s+push\b`),
		regexp.MustCompile(`\bgit\s+force\b`),
		regexp.MustCompile(`\bssh\b.*@`),
		regexp.MustCompile(`\beval\b`),
		regexp.MustCompile(`\bsource\s+.*\.sh\b`),
	}

	// windowsDenyPatterns 包含仅适用于 Windows 的 PowerShell 专用拒绝规则，
	// 在 Windows 上命令通过 powershell -Command 执行。
	windowsDenyPatterns = []*regexp.Regexp{
		// [Text.Encoding] 用于在运行时构造命令字符串。
		// 匹配 [Text.Encoding] 与 [System.Text.Encoding] 两种写法。
		regexp.MustCompile(`\[(?:\w+\.)?text\.encoding\]`),
		// PowerShell 的 -EncodedCommand 参数（base64 编码的命令）及其所有短写形式。
		// 匹配：-e、-ec、-enc、-en、-EncodedCommand（均带空格前缀）
		regexp.MustCompile(` -e(?:$|\s)| -ec(?:$|\s)| -enc(?:$|\s)| -en(?:$|\s)| -encodedcommand\b`),
		// 对字节数组调用 .GetString 以解码命令。
		regexp.MustCompile(`\.getstring\s*\(\s*\[byte\[\]`),
		// 命令构造链中使用的 FromBase64String。
		regexp.MustCompile(`frombase64string\(`),
		// 保存字节数组供 GetString 使用的 PowerShell 变量。
		regexp.MustCompile(`\$[a-zA-Z_]\w*\s*=\s*\[byte\[\]`),
		// 可能被用于构造命令的 Unicode 转义序列。
		// 匹配用于表示字符（如 i = "i"）的 \uXXXX 格式
		regexp.MustCompile(`\\u[0-9a-fA-F]{4}`),
	}

	// absolutePathPattern 匹配命令中的绝对文件路径（Unix 与 Windows）。
	// 在 shell 元字符（; | & ( ) < > 与反引号）处结束，避免把
	// "/dev/null;"、"/tmp/x&&" 这类带 shell 语法的 token 误判为路径。
	absolutePathPattern = regexp.MustCompile("[A-Za-z]:\\\\[^\\\\\"'\\s;|&()<>`]+|/[^\\s\"';|&()<>`]+")

	// safePaths 是内核伪设备，无论工作区限制如何，
	// 在命令中引用它们始终是安全的。它们不含用户数据，
	// 也不会导致破坏性写入。
	safePaths = map[string]bool{
		"/dev/null":    true,
		"/dev/zero":    true,
		"/dev/random":  true,
		"/dev/urandom": true,
		"/dev/stdin":   true,
		"/dev/stdout":  true,
		"/dev/stderr":  true,
	}
)

func NewExecTool(workingDir string, restrict bool, allowPaths ...[]*regexp.Regexp) (*ExecTool, error) {
	return NewExecToolWithConfig(workingDir, restrict, nil, allowPaths...)
}

func NewExecToolWithConfig(
	workingDir string,
	restrict bool,
	cfg *config.Config,
	allowPaths ...[]*regexp.Regexp,
) (*ExecTool, error) {
	denyPatterns := make([]*regexp.Regexp, 0)
	customAllowPatterns := make([]*regexp.Regexp, 0)
	var allowedPathPatterns []*regexp.Regexp
	allowRemote := true
	if len(allowPaths) > 0 {
		allowedPathPatterns = allowPaths[0]
	}

	if cfg != nil {
		execConfig := cfg.Tools.Exec
		enableDenyPatterns := execConfig.EnableDenyPatterns
		allowRemote = execConfig.AllowRemote
		if enableDenyPatterns {
			denyPatterns = append(denyPatterns, defaultDenyPatterns...)
			if runtime.GOOS == "windows" {
				denyPatterns = append(denyPatterns, windowsDenyPatterns...)
			}
			if len(execConfig.CustomDenyPatterns) > 0 {
				logger.InfoCF("tools", "using custom deny patterns", map[string]any{
					"patterns": execConfig.CustomDenyPatterns,
				})
				for _, pattern := range execConfig.CustomDenyPatterns {
					re, err := regexp.Compile(pattern)
					if err != nil {
						return nil, fmt.Errorf("自定义 deny 规则无效 %q：%w", pattern, err)
					}
					denyPatterns = append(denyPatterns, re)
				}
			}
		} else {
			// 如果禁用拒绝规则，则不添加任何规则，从而允许所有命令。
			logger.WarnCF("tools", "deny patterns are disabled, all commands will be allowed", nil)
		}
		for _, pattern := range execConfig.CustomAllowPatterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("自定义 allow 规则无效 %q：%w", pattern, err)
			}
			customAllowPatterns = append(customAllowPatterns, re)
		}
	} else {
		denyPatterns = append(denyPatterns, defaultDenyPatterns...)
		if runtime.GOOS == "windows" {
			denyPatterns = append(denyPatterns, windowsDenyPatterns...)
		}
	}

	var timeout time.Duration
	if cfg != nil && cfg.Tools.Exec.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.Tools.Exec.TimeoutSeconds) * time.Second
	}

	return &ExecTool{
		workingDir:          workingDir,
		timeout:             timeout,
		denyPatterns:        denyPatterns,
		allowPatterns:       nil,
		customAllowPatterns: customAllowPatterns,
		allowedPathPatterns: allowedPathPatterns,
		restrictToWorkspace: restrict,
		allowRemote:         allowRemote,
		sessionManager:      getSessionManager(),
	}, nil
}

func (t *ExecTool) Name() string {
	return "exec"
}

func (t *ExecTool) Description() string {
	return `执行 shell 命令。长时间运行的命令用 background=true（返回 sessionId）。交互式命令用 pty=true（可与 background=true 组合）。用 sessionId 配合 poll/read/write/send-keys/kill 管理后台会话。进程退出 30 分钟后会话自动清理；可提前用 kill 终止。输出缓冲上限 1MB。`
}

//nolint:dupl // 工具参数 schema 有意使用相似的 JSON-schema map 字面量。
func (t *ExecTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"run", "list", "poll", "read", "write", "kill", "send-keys"},
				"description": "操作：run（执行命令）、list（列出会话）、poll（检查状态）、read（获取输出）、write（发送输入）、kill（终止）、send-keys（向 PTY 发送按键）",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "要执行的 shell 命令（run 时必填）",
			},
			"sessionId": map[string]any{
				"type":        "string",
				"description": "会话 ID（poll/read/write/kill/send-keys 时必填）",
			},
			"keys": map[string]any{
				"type":        "string",
				"description": "send-keys 的按键名：up、down、left、right、enter、tab、escape、backspace、ctrl-c、ctrl-d、home、end、pageup、pagedown、f1-f12",
			},
			"data": map[string]any{
				"type":        "string",
				"description": "写入 stdin 的数据（write 时必填）",
			},
			"background": map[string]any{
				"type":        "string",
				"description": "立即在后台运行",
			},
			"pty": map[string]any{
				"type":        "string",
				"description": "可用时在伪终端（PTY）中运行",
			},
			"cwd": map[string]any{
				"type":        "string",
				"description": "命令的工作目录",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "超时秒数（0 表示不超时）",
			},
		},
		"required": []string{"action"},
	}
}

func (t *ExecTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	action, _ := args["action"].(string)
	if action == "" {
		return ErrorResult("action 参数必填")
	}

	switch action {
	case "run":
		return t.executeRun(ctx, args)
	case "list":
		return t.executeList()
	case "poll":
		return t.executePoll(args)
	case "read":
		return t.executeRead(args)
	case "write":
		return t.executeWrite(args)
	case "kill":
		return t.executeKill(args)
	case "send-keys":
		return t.executeSendKeys(args)
	default:
		return ErrorResult(fmt.Sprintf("未知 action：%s", action))
	}
}

func (t *ExecTool) executeRun(ctx context.Context, args map[string]any) *ToolResult {
	command, ok := args["command"].(string)
	if !ok {
		return ErrorResult("command 参数必填")
	}

	// GHSA-pv8c-p6jf-3fpp：阻止来自远程 channel（例如 Telegram webhook）的 exec 调用，
	// 除非通过配置显式开启。失败即关闭：空 channel 视为被阻止。
	if !t.allowRemote {
		channel := ToolChannel(ctx)
		if channel == "" {
			channel, _ = args["__channel"].(string)
		}
		channel = strings.TrimSpace(channel)
		if channel == "" || !constants.IsInternalChannel(channel) {
			return ErrorResult("exec 仅限内部 channel 使用")
		}
	}

	getBoolArg := func(key string) bool {
		switch v := args[key].(type) {
		case bool:
			return v
		case string:
			return v == "true"
		}
		return false
	}
	isPty := getBoolArg("pty")
	isBackground := getBoolArg("background")

	if isPty {
		if runtime.GOOS == "windows" {
			return ErrorResult("Windows 不支持 PTY。请使用 background=true 且不要设置 pty。")
		}
	}

	cwd := t.workingDir
	if wd, ok := args["cwd"].(string); ok && wd != "" {
		if t.restrictToWorkspace && t.workingDir != "" {
			resolvedWD, err := validatePathWithAllowPaths(wd, t.workingDir, true, t.allowedPathPatterns)
			if err != nil {
				return ErrorResult("命令被安全护栏拦截（" + err.Error() + "）")
			}
			cwd = resolvedWD
		} else {
			cwd = wd
		}
	}

	if cwd == "" {
		wd, err := os.Getwd()
		if err == nil {
			cwd = wd
		}
	}

	if guardError := t.guardCommand(command, cwd); guardError != "" {
		return ErrorResult(guardError)
	}

	// 执行前立即重新解析符号链接，以缩短校验与赋值
	// cmd.Dir 之间的 TOCTOU 窗口。
	if t.restrictToWorkspace && t.workingDir != "" && cwd != t.workingDir {
		resolved, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return ErrorResult(fmt.Sprintf("命令被安全护栏拦截（路径解析失败：%v）", err))
		}
		if isAllowedPath(resolved, t.allowedPathPatterns) {
			cwd = resolved
		} else {
			absWorkspace, _ := filepath.Abs(t.workingDir)
			wsResolved, _ := filepath.EvalSymlinks(absWorkspace)
			if wsResolved == "" {
				wsResolved = absWorkspace
			}
			rel, err := filepath.Rel(wsResolved, resolved)
			if err != nil || !filepath.IsLocal(rel) {
				return ErrorResult("命令被安全护栏拦截（工作目录逃逸工作区）")
			}
			cwd = resolved
		}
	}

	if isBackground {
		return t.runBackground(ctx, command, cwd, isPty)
	}

	return t.runSync(ctx, command, cwd)
}

func (t *ExecTool) runSync(ctx context.Context, command, cwd string) *ToolResult {
	// timeout == 0 表示不超时
	var cmdCtx context.Context
	var cancel context.CancelFunc
	if t.timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, t.timeout)
	} else {
		cmdCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(cmdCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.CommandContext(cmdCtx, "sh", "-c", command)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	prepareCommandForTermination(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// 通过共享的隔离入口执行 shell，使 exec 工具的子进程
	// 获得与其他集成相同的隔离策略。
	if err := isolation.Start(cmd); err != nil {
		return ErrorResult(fmt.Sprintf("启动命令失败：%v", err))
	}
	// 父进程死亡看门狗：若网关被 SIGKILL，命令的进程组会被
	// 终止，而不会变成孤儿进程（见 orphan_guard_unix.go）。
	if cmd.Process != nil {
		if stopGuard, guardErr := startOrphanGuard(cmd.Process.Pid); guardErr == nil {
			defer stopGuard()
		} else {
			logger.WarnCF("shell", "Failed to start orphan guard", map[string]any{"error": guardErr.Error()})
		}
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorCF("shell", "cmd.Wait goroutine panic recovered",
					map[string]any{
						"panic": fmt.Sprintf("%v", r),
						"stack": string(debug.Stack()),
					})
				done <- fmt.Errorf("panic in cmd.Wait: %v", r)
			}
		}()
		done <- cmd.Wait()
	}()

	var err error
	select {
	case err = <-done:
	case <-cmdCtx.Done():
		_ = terminateProcessTree(cmd)
		select {
		case err = <-done:
		case <-time.After(2 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			err = <-done
		}
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR:\n" + stderr.String()
	}

	if err != nil {
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			msg := fmt.Sprintf("命令超时（%v）", t.timeout)
			if output != "" {
				msg += "\n\n超时前的部分输出：\n" + output
			}
			return &ToolResult{
				ForLLM:  msg,
				ForUser: msg,
				IsError: true,
				Err:     fmt.Errorf("command timeout: %w", err),
			}
		}

		// 提取详细的退出信息
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode := exitErr.ExitCode()
			output += fmt.Sprintf("\n\n[命令退出码 %d]", exitCode)

			// 若被信号杀死（Unix），补充信号信息
			if exitCode == -1 {
				output += "（被信号终止）"
			}
		} else {
			output += fmt.Sprintf("\n\n[命令失败：%v]", err)
		}
	}

	if output == "" {
		output = "(no output)"
	}

	maxLen := 10000
	if len(output) > maxLen {
		output = output[:maxLen] + fmt.Sprintf("\n...（已截断，省略 %d 字符）", len(output)-maxLen)
	}

	if err != nil {
		return &ToolResult{
			ForLLM:  output,
			ForUser: output,
			IsError: true,
		}
	}

	return &ToolResult{
		ForLLM:  output,
		ForUser: output,
		IsError: false,
	}
}

func (t *ExecTool) runBackground(ctx context.Context, command, cwd string, ptyEnabled bool) *ToolResult {
	sessionID := generateSessionID()
	session := &ProcessSession{
		ID:         sessionID,
		Command:    command,
		PTY:        ptyEnabled,
		Background: true,
		StartTime:  time.Now().Unix(),
		Status:     "running",
		ptyKeyMode: PtyKeyModeCSI,
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	prepareCommandForTermination(cmd)

	var stdoutReader io.ReadCloser
	var stderrReader io.ReadCloser
	var stdinWriter io.WriteCloser

	if ptyEnabled {
		ptmx, tty, err := pty.Open()
		if err != nil {
			return ErrorResult(fmt.Sprintf("创建 PTY 失败：%v", err))
		}

		cmd.Stdin = tty
		cmd.Stdout = tty
		cmd.Stderr = tty

		// PTY 模式下需要使用 Setsid 创建新会话。
		// 注意：Setsid 与 Setpgid 冲突，因此必须整体替换 SysProcAttr。
		setSysProcAttrForPty(cmd)

		session.ptyMaster = ptmx
	} else {
		var err error
		stdoutReader, err = cmd.StdoutPipe()
		if err != nil {
			return ErrorResult(fmt.Sprintf("创建 stdout 管道失败：%v", err))
		}
		stderrReader, err = cmd.StderrPipe()
		if err != nil {
			return ErrorResult(fmt.Sprintf("创建 stderr 管道失败：%v", err))
		}
		stdinWriter, err = cmd.StdinPipe()
		if err != nil {
			return ErrorResult(fmt.Sprintf("创建 stdin 管道失败：%v", err))
		}
		session.stdoutPipe = io.MultiReader(stdoutReader, stderrReader)
		session.stdinWriter = stdinWriter
	}

	// 后台会话使用相同的启动路径，使隔离策略与同步 exec 运行保持一致。
	if err := isolation.Start(cmd); err != nil {
		if session.ptyMaster != nil {
			_ = session.ptyMaster.Close()
		}
		return ErrorResult(fmt.Sprintf("启动命令失败：%v", err))
	}

	session.PID = cmd.Process.Pid
	if stopGuard, guardErr := startOrphanGuard(session.PID); guardErr == nil {
		session.orphanGuardStop = stopGuard
	} else {
		logger.WarnCF("shell", "Failed to start orphan guard for background session",
			map[string]any{"error": guardErr.Error(), "session_id": sessionID})
	}
	t.sessionManager.Add(session)

	session.outputBuffer = &bytes.Buffer{}

	// PTY 模式：从 ptyMaster 读取并等待进程结束
	// 注意：在 Linux 上关闭 ptyMaster 不会中断阻塞中的 Read() 调用，
	// 因此需要在单独的 goroutine 中调用 cmd.Wait() 来检测进程退出。
	if session.PTY && session.ptyMaster != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.ErrorCF("shell", "PTY cmd.Wait goroutine panic recovered",
						map[string]any{
							"panic": fmt.Sprintf("%v", r),
							"stack": string(debug.Stack()),
						})
					session.mu.Lock()
					session.Status = "error"
					session.mu.Unlock()
				}
			}()
			cmd.Wait() // 等待进程退出
			session.mu.Lock()
			if cmd.ProcessState != nil {
				session.ExitCode = cmd.ProcessState.ExitCode()
			}
			session.Status = "done"
			session.mu.Unlock()
			session.stopOrphanGuard()
		}()

		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.ErrorCF("shell", "PTY read goroutine panic recovered",
						map[string]any{
							"panic": fmt.Sprintf("%v", r),
							"stack": string(debug.Stack()),
						})
				}
			}()
			buf := make([]byte, 4096)
			for {
				n, err := session.ptyMaster.Read(buf)
				if n > 0 {
					raw := string(buf[:n])
					if mode := detectPtyKeyMode(raw); mode != PtyKeyModeNotFound && mode != session.GetPtyKeyMode() {
						session.SetPtyKeyMode(mode)
					}

					session.mu.Lock()
					if session.outputBuffer.Len() >= maxOutputBufferSize {
						if !session.outputTruncated {
							session.outputBuffer.WriteString(outputTruncateMarker)
							session.outputTruncated = true
						}
					} else {
						session.outputBuffer.Write(buf[:n])
					}
					session.mu.Unlock()
				}
				if err != nil {
					break
				}
			}
		}()
	} else {
		// 非 PTY 模式：由单个 goroutine 读取管道。
		// 当 Read() 返回 EOF（管道已关闭）时跳出循环。
		// 进程退出时 OS 关闭管道写端 → Read() 返回 EOF → 本循环退出。
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.ErrorCF("shell", "pipe read goroutine panic recovered",
						map[string]any{
							"panic": fmt.Sprintf("%v", r),
							"stack": string(debug.Stack()),
						})
				}
			}()
			buf := make([]byte, 4096)

			// 读取 stdout
			for {
				n, err := stdoutReader.Read(buf)
				if n > 0 {
					session.mu.Lock()
					if session.outputBuffer.Len() >= maxOutputBufferSize {
						if !session.outputTruncated {
							session.outputBuffer.WriteString(outputTruncateMarker)
							session.outputTruncated = true
						}
					} else {
						session.outputBuffer.Write(buf[:n])
					}
					session.mu.Unlock()
				}
				if err != nil {
					break
				}
			}

			// 读取 stderr
			for {
				n, err := stderrReader.Read(buf)
				if n > 0 {
					session.mu.Lock()
					if session.outputBuffer.Len() >= maxOutputBufferSize {
						if !session.outputTruncated {
							session.outputBuffer.WriteString(outputTruncateMarker)
							session.outputTruncated = true
						}
					} else {
						session.outputBuffer.Write(buf[:n])
					}
					session.mu.Unlock()
				}
				if err != nil {
					break
				}
			}

			// 所有管道已关闭，获取退出状态
			if stdinWriter != nil {
				_ = stdinWriter.Close()
			}
			cmd.Wait()

			session.mu.Lock()
			if cmd.ProcessState != nil {
				session.ExitCode = cmd.ProcessState.ExitCode()
			}
			session.Status = "done"
			session.mu.Unlock()
			session.stopOrphanGuard()
		}()
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    "running",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("会话 %s 已启动", sessionID),
		IsError: false,
	}
}

func (t *ExecTool) executeList() *ToolResult {
	sessions := t.sessionManager.List()
	resp := ExecResponse{
		Sessions: sessions,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("%d 个活动会话", len(sessions)),
		IsError: false,
	}
}

func (t *ExecTool) executePoll(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId 参数必填")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("会话不存在：%s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    session.GetStatus(),
		ExitCode:  session.GetExitCode(),
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		IsError: false,
	}
}

func (t *ExecTool) executeRead(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId 参数必填")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("会话不存在：%s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	output := session.Read()

	resp := ExecResponse{
		SessionID: sessionID,
		Output:    output,
		Status:    session.GetStatus(),
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		IsError: false,
	}
}

func (t *ExecTool) executeWrite(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId 参数必填")
	}

	data, ok := args["data"].(string)
	if !ok {
		return ErrorResult("data 参数必填")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("会话不存在：%s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	if session.IsDone() {
		return ErrorResult(fmt.Sprintf("进程已退出，退出码 %d", session.GetExitCode()))
	}

	if err = session.Write(data); err != nil {
		if errors.Is(err, ErrSessionDone) {
			return ErrorResult(fmt.Sprintf("进程已退出，退出码 %d", session.GetExitCode()))
		}
		return ErrorResult(fmt.Sprintf("写入会话失败：%v", err))
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    session.GetStatus(),
	}
	respData, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(respData),
		IsError: false,
	}
}

func (t *ExecTool) executeKill(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId 参数必填")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("会话不存在：%s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	if session.IsDone() {
		return ErrorResult(fmt.Sprintf("进程已退出，退出码 %d", session.GetExitCode()))
	}

	if err = session.Kill(); err != nil {
		return ErrorResult(fmt.Sprintf("终止会话失败：%v", err))
	}
	session.stopOrphanGuard()

	t.sessionManager.Remove(sessionID)

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    "done",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("Session %s killed", sessionID),
		IsError: false,
	}
}

// keyMap 将按键名映射到对应的转义序列。
var keyMap = map[string]string{
	"enter":     "\r",
	"return":    "\r",
	"tab":       "\t",
	"escape":    "\x1b",
	"esc":       "\x1b",
	"space":     " ",
	"backspace": "\x7f",
	"bspace":    "\x7f",
	"up":        "\x1b[A",
	"down":      "\x1b[B",
	"right":     "\x1b[C",
	"left":      "\x1b[D",
	"home":      "\x1b[1~",
	"end":       "\x1b[4~",
	"pageup":    "\x1b[5~",
	"pagedown":  "\x1b[6~",
	"pgup":      "\x1b[5~",
	"pgdn":      "\x1b[6~",
	"insert":    "\x1b[2~",
	"ic":        "\x1b[2~",
	"delete":    "\x1b[3~",
	"del":       "\x1b[3~",
	"dc":        "\x1b[3~",
	"btab":      "\x1b[Z",
	"f1":        "\x1bOP",
	"f2":        "\x1bOQ",
	"f3":        "\x1bOR",
	"f4":        "\x1bOS",
	"f5":        "\x1b[15~",
	"f6":        "\x1b[17~",
	"f7":        "\x1b[18~",
	"f8":        "\x1b[19~",
	"f9":        "\x1b[20~",
	"f10":       "\x1b[21~",
	"f11":       "\x1b[23~",
	"f12":       "\x1b[24~",
}

// ss3KeysMap 将按键名映射到 SS3 转义序列
var ss3KeysMap = map[string]string{
	"up":    "\x1bOA",
	"down":  "\x1bOB",
	"right": "\x1bOC",
	"left":  "\x1bOD",
	"home":  "\x1bOH",
	"end":   "\x1bOF",
}

func detectPtyKeyMode(raw string) PtyKeyMode {
	const SMKX = "\x1b[?1h"
	const RMKX = "\x1b[?1l"

	lastSmkx := strings.LastIndex(raw, SMKX)
	lastRmkx := strings.LastIndex(raw, RMKX)

	if lastSmkx == -1 && lastRmkx == -1 {
		return PtyKeyModeNotFound
	}

	if lastSmkx > lastRmkx {
		return PtyKeyModeSS3
	}
	return PtyKeyModeCSI
}

// encodeKeyToken 将单个按键 token 编码为对应的转义序列。
// 支持：
//   - 具名按键："enter"、"tab"、"up"、"ctrl-c"、"alt-x" 等。
//   - Ctrl 修饰符："ctrl-c" 或 "c-c"（发送 Ctrl+字符）
//   - Alt 修饰符："alt-x" 或 "m-x"（发送 ESC+字符）
func encodeKeyToken(token string, ptyKeyMode PtyKeyMode) (string, error) {
	token = strings.ToLower(strings.TrimSpace(token))
	if token == "" {
		return "", nil
	}

	// 处理 ctrl-X 形式（c-x）
	if strings.HasPrefix(token, "c-") {
		char := token[2]
		if char >= 'a' && char <= 'z' {
			return string(rune(char) & 0x1f), nil // ctrl-a 到 ctrl-z
		}
		return "", fmt.Errorf("无效的 ctrl 组合键：%s", token)
	}

	// 处理 ctrl-X 形式（ctrl-x）
	if strings.HasPrefix(token, "ctrl-") {
		char := token[5]
		if char >= 'a' && char <= 'z' {
			return string(rune(char) & 0x1f), nil
		}
		return "", fmt.Errorf("无效的 ctrl 组合键：%s", token)
	}

	// 处理 alt-X 形式（m-x 或 alt-x）
	if strings.HasPrefix(token, "m-") || strings.HasPrefix(token, "alt-") {
		var char string
		if strings.HasPrefix(token, "m-") {
			char = token[2:]
		} else {
			char = token[4:]
		}
		if len(char) == 1 {
			return "\x1b" + char, nil
		}
		return "", fmt.Errorf("无效的 alt 组合键：%s", token)
	}

	// 处理特殊按键的 shift 修饰符（shift-up、shift-down 等）
	if strings.HasPrefix(token, "s-") || strings.HasPrefix(token, "shift-") {
		var key string
		if strings.HasPrefix(token, "s-") {
			key = token[2:]
		} else {
			key = token[6:]
		}
		// 应用 shift 修饰符：单字符按键返回大写形式
		if seq, ok := keyMap[key]; ok {
			// 对于转义序列，难以直接叠加 shift
			// 对于单字符按键（字母），返回大写形式
			if len(seq) == 1 {
				return strings.ToUpper(seq), nil
			}
			return seq, nil
		}
		return "", fmt.Errorf("未知的 shift 组合键：%s", key)
	}

	if ptyKeyMode == PtyKeyModeSS3 {
		if seq, ok := ss3KeysMap[token]; ok {
			return seq, nil
		}
	}

	if seq, ok := keyMap[token]; ok {
		return seq, nil
	}

	return "", fmt.Errorf("未知按键：%s（文本输入请使用 write action）", token)
}

// encodeKeySequence 将一组按键 token 编码为一个字符串。
func encodeKeySequence(tokens []string, ptyKeyMode PtyKeyMode) (string, error) {
	var result string
	for _, token := range tokens {
		seq, err := encodeKeyToken(token, ptyKeyMode)
		if err != nil {
			return "", err
		}
		result += seq
	}
	return result, nil
}

func (t *ExecTool) executeSendKeys(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId 参数必填")
	}

	keysStr, ok := args["keys"].(string)
	if !ok {
		return ErrorResult("keys 必须是字符串")
	}

	if keysStr == "" {
		return ErrorResult("keys 不能为空")
	}

	// 解析逗号分隔的按键名
	keyNames := strings.Split(keysStr, ",")
	var keys []string
	for _, k := range keyNames {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}

	if len(keys) == 0 {
		return ErrorResult("keys 不能为空")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("会话不存在：%s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	ptyKeyMode := session.GetPtyKeyMode()

	data, err := encodeKeySequence(keys, ptyKeyMode)
	if err != nil {
		return ErrorResult(fmt.Sprintf("无效按键：%v", err))
	}

	if session.IsDone() {
		return ErrorResult(fmt.Sprintf("进程已退出，退出码 %d", session.GetExitCode()))
	}

	if err = session.Write(data); err != nil {
		if errors.Is(err, ErrSessionDone) {
			return ErrorResult(fmt.Sprintf("进程已退出，退出码 %d", session.GetExitCode()))
		}
		return ErrorResult(fmt.Sprintf("发送按键失败：%v", err))
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    "running",
		Output:    fmt.Sprintf("已发送按键：%v", keys),
	}
	respData, err := json.Marshal(resp)
	if err != nil {
		return ErrorResult(err.Error())
	}
	return &ToolResult{
		ForLLM:  string(respData),
		IsError: false,
	}
}

// expandPowerShellEnvVars 将 PowerShell（$env:VAR）与 CMD（%VAR%）
// 两种环境变量语法展开为实际值。
func expandPowerShellEnvVars(cmd string) string {
	// 处理 PowerShell 风格：$env:VAR 与 ${env:VAR}
	rePs := regexp.MustCompile(`\$\{?env:(\w+)\}?`)
	cmd = rePs.ReplaceAllStringFunc(cmd, func(match string) string {
		varName := rePs.FindStringSubmatch(match)[1]
		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match
	})

	// 处理 CMD 风格：%VAR%
	reCmd := regexp.MustCompile(`%([^%]+)%`)
	return reCmd.ReplaceAllStringFunc(cmd, func(match string) string {
		varName := reCmd.FindStringSubmatch(match)[1]
		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match
	})
}

func (t *ExecTool) guardCommand(command, cwd string) string {
	cmd := strings.TrimSpace(command)
	lower := strings.ToLower(cmd)

	// 自定义 allow 规则可让命令免于拒绝检查。
	explicitlyAllowed := false
	for _, pattern := range t.customAllowPatterns {
		if pattern.MatchString(lower) {
			explicitlyAllowed = true
			break
		}
	}

	if !explicitlyAllowed {
		for _, pattern := range t.denyPatterns {
			if pattern.MatchString(lower) {
				return "命令被安全护栏拦截（检测到危险模式）"
			}
		}
	}

	if len(t.allowPatterns) > 0 {
		allowed := false
		for _, pattern := range t.allowPatterns {
			if pattern.MatchString(lower) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "命令被安全护栏拦截（不在允许列表中）"
		}
	}

	if t.restrictToWorkspace {
		// 阻止路径穿越模式，包括 .../.../ 这类变体
		if regexp.MustCompile(`\.\.(?:[\\/]\.\.)*[\\/]`).MatchString(cmd) {
			return "命令被安全护栏拦截（检测到路径穿越）"
		}

		cwdPath, err := filepath.Abs(cwd)
		if err != nil {
			return ""
		}

		// 这些 Web URL scheme 的路径部分（以 // 开头）应豁免工作区沙箱检查。
		// 有意排除 file:，使 file:// URI 仍会针对
		// 工作区边界进行校验。
		webSchemes := []string{"http:", "https:", "ftp:", "ftps:", "sftp:", "ssh:", "git:"}

		// 在 Windows 上，路径检查前先展开 ~ 与 PowerShell 环境变量（$env:VAR）
		if runtime.GOOS == "windows" {
			// 展开 PowerShell 环境变量（$env:VAR 与 ${env:VAR}）
			cmd = expandPowerShellEnvVars(cmd)
			// 为完整起见也展开 ~
			if home, err := os.UserHomeDir(); err == nil {
				cmd = strings.ReplaceAll(cmd, "~", filepath.FromSlash(home))
			}
		}

		matchIndices := absolutePathPattern.FindAllStringIndex(cmd, -1)

		for _, loc := range matchIndices {
			raw := cmd[loc[0]:loc[1]]

			// 跳过看起来来自 Web URL 的路径部分。
			// 解析 "https://github.com" 这类 URL 时，正则会把
			// "//github.com" 当作匹配项（即 "https:" 之后的路径部分）。
			// 这里使用精确的匹配位置（loc[0]），使同一命令中重复出现的
			// //path 子串各自按其位置分别判断。
			if strings.HasPrefix(raw, "//") && loc[0] > 0 {
				before := cmd[:loc[0]]
				isWebURL := false

				for _, scheme := range webSchemes {
					if strings.HasSuffix(before, scheme) {
						isWebURL = true
						break
					}
				}

				if isWebURL {
					continue
				}
			}

			// 跳过形如 "wttr.in/Beijing" 的无 scheme URL 路径。
			// 当 /path 前面紧邻的 token 看起来像域名，且该 token
			// 在本地文件系统中并不存在时，就把该路径视为 URL 的一部分，
			// 跳过工作区沙箱校验。
			//
			// 本地路径存在性检查可防止符号链接绕过：若
			// "foo.bar" 作为本地符号链接或目录存在，该路径
			// 仍会走完整的工作区校验（见 #2965）。
			if loc[0] > 0 && raw[0] == '/' {
				// 找到紧邻 "/" 之前的 token。
				j := loc[0] - 1
				for j >= 0 && !isShellTokenBoundary(cmd[j]) {
					j--
				}
				token := cmd[j+1 : loc[0]]
				if looksLikeDomain(token) && !localPathExists(cwd, token) {
					continue
				}
			}

			p, err := commandPathAbs(commandPathTextFromMatch(cmd, loc[0], loc[1]), cwdPath)
			if err != nil {
				continue
			}

			// Windows 专用：规范化路径，以阻止 ADS 与超长路径
			if runtime.GOOS == "windows" {
				// 去掉 \\?\ 前缀（超长路径）
				p = strings.TrimPrefix(p, `\\?\`)
				// 去掉 NTFS 备用数据流（仅当冒号不在位置 1，即不是盘符时）
				if idx := strings.Index(p, ":"); idx > 1 {
					p = p[:idx]
				}
			}

			// 检查符号链接与 junction
			resolved, err := filepath.EvalSymlinks(p)
			if err == nil {
				p = resolved
			}

			if safePaths[p] {
				continue
			}
			if isAllowedPath(p, t.allowedPathPatterns) {
				continue
			}

			// 同时规范化工作目录：在 macOS 上 /var 是指向
			// /private/var 的符号链接，否则解析后的目标路径
			// 会被误判为逃逸出工作区。
			cwdReal := cwdPath
			if resolvedCwd, resolveErr := filepath.EvalSymlinks(cwdPath); resolveErr == nil {
				cwdReal = resolvedCwd
			}
			rel, err := filepath.Rel(cwdReal, p)
			if err != nil {
				continue
			}
			if strings.HasPrefix(rel, "..") {
				// 拒绝前先回退到词法比较。
				if lexicalRel, lexicalErr := filepath.Rel(cwdPath, p); lexicalErr == nil && !strings.HasPrefix(lexicalRel, "..") {
					continue
				}
				return "命令被安全护栏拦截（路径位于工作目录之外）"
			}
		}
	}

	return ""
}

func commandPathAbs(pathText, cwdPath string) (string, error) {
	if filepath.IsAbs(pathText) {
		return filepath.Abs(pathText)
	}
	return filepath.Abs(filepath.Join(cwdPath, pathText))
}

func commandPathTextFromMatch(cmd string, start, end int) string {
	raw := cmd[start:end]
	if !strings.HasPrefix(raw, "/") || isUnixAbsolutePathMatchStart(cmd, start) {
		return raw
	}

	tokenStart, tokenEnd := shellTokenBounds(cmd, start)
	prefix := cmd[tokenStart:start]
	// 对于 --flag=rel/path，校验其取值。对于 -isystem/path 这类
	// 选项与路径粘连的歧义形式，保守地保留以斜杠开头的路径。
	if eq := strings.IndexByte(prefix, '='); eq >= 0 {
		return cmd[tokenStart+eq+1 : tokenEnd]
	}
	if strings.HasPrefix(prefix, "-") {
		return raw
	}
	return cmd[tokenStart:tokenEnd]
}

func shellTokenBounds(cmd string, idx int) (int, int) {
	start := idx
	for start > 0 && !isShellTokenBoundary(cmd[start-1]) {
		start--
	}
	end := idx
	for end < len(cmd) && !isShellTokenBoundary(cmd[end]) {
		end++
	}
	return start, end
}

// isUnixAbsolutePathMatchStart 在以 "/" 开头的正则匹配确实是绝对路径
// token（而非 "skills/foo.py" 这类相对路径内部的分隔符）时返回 true。
func isUnixAbsolutePathMatchStart(cmd string, idx int) bool {
	if idx <= 0 {
		return true
	}

	prev := cmd[idx-1]
	if isShellTokenBoundary(prev) || prev == '=' || prev == ',' || prev == '(' || prev == '[' || prev == '{' {
		return true
	}

	j := idx - 1
	for j >= 0 && !isShellTokenBoundary(cmd[j]) {
		j--
	}
	prefix := cmd[j+1 : idx]

	return strings.HasPrefix(prefix, "-") && !strings.Contains(prefix, "=")
}

// isShellTokenBoundary 在 b 是 shell 命令中用于分隔 token 的字节
// （空格、制表符、冒号、分号、管道符等）时返回 true。
func isShellTokenBoundary(b byte) bool {
	switch b {
	case ' ', '\t', ':', ';', '|', '&', '<', '>', '\'', '"', '`', '\n', '\r':
		return true
	}
	return false
}

// looksLikeDomain 在 s 看起来像 DNS 域名时返回 true：
// 至少包含一个点、以字母或数字开头，
// 且不以常见文件扩展名结尾。
func looksLikeDomain(s string) bool {
	if len(s) < 3 || !strings.ContainsRune(s, '.') {
		return false
	}
	first := s[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return false
	}
	// 排除以常见文件/编程语言扩展名结尾的 token，
	// 例如 "script.py"、"main.go"、"app.exe"。
	if idx := strings.LastIndexByte(s, '.'); idx >= 0 {
		ext := strings.ToLower(s[idx+1:])
		if commonFileExtension(ext) {
			return false
		}
	}
	return true
}

// commonFileExtension 在 ext 是强烈表明本地文件（而非域名 TLD）的
// 文件扩展名时返回 true。
func commonFileExtension(ext string) bool {
	switch ext {
	case "py", "js", "ts", "tsx", "jsx", "go", "rs", "rb", "php",
		"java", "c", "cpp", "h", "hpp", "cs", "swift", "kt", "scala",
		"sh", "bash", "zsh", "fish", "ps1", "bat", "cmd",
		"txt", "md", "rst", "log", "json", "yaml", "yml", "toml",
		"xml", "html", "css", "scss", "ini", "cfg", "conf", "env",
		"exe", "dll", "so", "dylib", "lib", "a", "o", "obj",
		"zip", "tar", "gz", "bz2", "xz", "7z", "rar",
		"png", "jpg", "jpeg", "gif", "svg", "ico", "bmp", "webp",
		"mp3", "mp4", "wav", "avi", "mov", "mkv", "flac",
		"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx",
		"pub", "pem", "key", "crt", "cer", "p12", "pfx",
		"bak", "tmp", "swp", "lock",
		"ttf", "otf", "woff", "woff2", "eot",
		"deb", "rpm", "apk", "msi", "dmg",
		"sql", "sqlite", "db":
		return true
	}
	return false
}

// localPathExists 在给定 token 相对于 cwd 能解析到
// 已存在的文件系统条目时返回 true。
func localPathExists(cwd, token string) bool {
	info, err := os.Lstat(filepath.Join(cwd, token))
	return err == nil && info != nil
}

func (t *ExecTool) SetTimeout(timeout time.Duration) {
	t.timeout = timeout
}

func (t *ExecTool) SetRestrictToWorkspace(restrict bool) {
	t.restrictToWorkspace = restrict
}

func (t *ExecTool) SetAllowPatterns(patterns []string) error {
	t.allowPatterns = make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("invalid allow pattern %q: %w", p, err)
		}
		t.allowPatterns = append(t.allowPatterns, re)
	}
	return nil
}
