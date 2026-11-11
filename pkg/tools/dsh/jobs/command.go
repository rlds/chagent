package jobs

import (
	"os/exec"
	"regexp"
	"strings"
)

// shellCommand 构造平台相关的 shell 调用。任务只运行一条命令字符串，
// 因此需要 shell 来支持管道/重定向，与 DSH 的 bash 生产者一致。
func shellCommand(command string) *exec.Cmd {
	if shellExecutable() == "" {
		// 防御性处理：空 shell 会让 cmd.Start panic；调用方会把它转换为
		// job_start_failed 错误。
		return exec.Command(command)
	}
	return exec.Command(shellExecutable(), shellArgs(command)...)
}

// defaultDenyPatterns 与 exec 工具（pkg/tools/shell.go 的
// `defaultDenyPatterns`）强制执行的危险命令模式一致。由于 exec 的辅助函数
// 未导出，这里复制了这些模式；配置了自定义 exec 拒绝模式的调用方应通过
// Options.DenyPatterns 传入。
var defaultDenyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+-[rf]{1,2}\b`),
	regexp.MustCompile(`\bdel\s+/[fq]\b`),
	regexp.MustCompile(`\brmdir\s+/s\b`),
	regexp.MustCompile(`(^|[^-\w])\b(format|mkfs|diskpart)\b\s`),
	regexp.MustCompile(`\bdd\s+if=`),
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

// pathTraversalPattern 匹配 `..` 路径片段（包括 Windows 分隔符），
// 与 exec 工具的工作区防护一致。
var pathTraversalPattern = regexp.MustCompile(`\.\.(?:[\\/]\.\.)*[\\/]`)

// absolutePathPattern 捕获命令字符串中的绝对路径候选。它有意保持保守：
// 带引号的绝对路径会被捕获，URL 会被单独过滤，而相对路径（`./build`、
// `src/x`）保持不变。
var absolutePathPattern = regexp.MustCompile(`(?:^|[\s"'=:()|&;<>])(/[^\s"'` + "`" + `|&;<>()]+)`)

// urlSchemePattern 报告路径匹配项前方紧邻的文本是否为 URL scheme
// （`https://`），其 `//host/path` 不能进行沙箱检查。
var urlSchemePattern = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*:$`)

// pseudoDevPaths 是命令可能合法引用、但位于工作区之外的设备路径。
var pseudoDevPaths = map[string]struct{}{
	"/dev/null":    {},
	"/dev/zero":    {},
	"/dev/random":  {},
	"/dev/urandom": {},
	"/dev/stdin":   {},
	"/dev/stdout":  {},
	"/dev/stderr":  {},
}

// validateCommand 强制执行管理器的命令策略：
//
//  1. 非空（由调用方检查）；
//  2. 与 exec 工具相同的 allowlist 语义（非空的
//     Options.AllowPatterns 要求匹配；匹配项还会使命令免于 deny 模式）；
//  3. 危险命令模式（与 exec 等价的默认值 + Options.DenyPatterns）；
//  4. 不允许 `..` 路径穿越，也不允许工作区外的绝对路径
//     （除非 Options.Unrestricted）。
func (m *Manager) validateCommand(command string) error {
	lower := strings.ToLower(command)

	explicitlyAllowed := false
	for _, pattern := range m.userAllow {
		if pattern.MatchString(lower) {
			explicitlyAllowed = true
			break
		}
	}
	if !explicitlyAllowed {
		for _, pattern := range defaultDenyPatterns {
			if pattern.MatchString(lower) {
				return newError("command_denied", "命令被安全守卫拒绝（命中危险模式）")
			}
		}
		for _, pattern := range m.userDeny {
			if pattern.MatchString(lower) {
				return newError("command_denied", "命令被安全守卫拒绝（命中自定义危险模式）")
			}
		}
	}
	if len(m.userAllow) > 0 && !explicitlyAllowed {
		return newError("command_denied", "命令不在 allow 列表中")
	}
	if m.opts.Unrestricted {
		return nil
	}
	if pathTraversalPattern.MatchString(command) {
		return newError("command_denied", "命令被安全守卫拒绝（检测到路径穿越）")
	}
	if !m.workspaceContainsAllAbsolutePaths(command) {
		return newError("command_denied", "命令被安全守卫拒绝（命令中引用了工作区外的绝对路径）")
	}
	return nil
}

// workspaceContainsAllAbsolutePaths 报告命令中的每个绝对路径候选是否都解析到
// 工作区内。URL 和标准 /dev 伪文件豁免，与 exec 工具的 URL 豁免一致。
func (m *Manager) workspaceContainsAllAbsolutePaths(command string) bool {
	for _, match := range absolutePathPattern.FindAllStringSubmatchIndex(command, -1) {
		start, end := match[2], match[3]
		raw := command[start:end]
		if _, ok := pseudoDevPaths[raw]; ok {
			continue
		}
		// scheme 之后的 `//host/path` 是 URL，不是文件系统路径。
		if before := command[:start]; urlSchemePattern.MatchString(before) {
			continue
		}
		// 无 scheme 的 `//` 网络路径和 UNC 路径不是本管理器支持的
		// 工作区文件系统引用。
		if strings.HasPrefix(raw, "//") {
			continue
		}
		clean := raw
		if !m.pathInsideWorkspace(clean) {
			return false
		}
	}
	return true
}

// pathInsideWorkspace 报告绝对路径 p 在清理后是否仍位于工作区内。这里有意
// 不跟随符号链接：命令仅按文本边界拒绝，与 exec 工具的预执行防护一致，
// 而不是其运行时沙箱。
func (m *Manager) pathInsideWorkspace(p string) bool {
	if m.workspace == "" {
		return false
	}
	cleaned := cleanPath(p)
	if cleaned == "" {
		return false
	}
	return pathHasPrefix(cleaned, m.workspace)
}
