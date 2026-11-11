package providers

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strings"
	"syscall"
)

// Go HTTP 错误消息中的常见模式
var httpStatusPatterns = []*regexp.Regexp{
	regexp.MustCompile(`status[:\s]+(\d{3})`),
	regexp.MustCompile(`http[/\s]+\d*\.?\d*\s+(\d{3})`),
	regexp.MustCompile(`\b([3-5]\d{2})\b`),
}

// errorPattern 定义用于错误分类的单个模式（字符串或正则表达式）。
type errorPattern struct {
	substring string
	regex     *regexp.Regexp
}

func substr(s string) errorPattern { return errorPattern{substring: s} }
func rxp(r string) errorPattern    { return errorPattern{regex: regexp.MustCompile("(?i)" + r)} }

// 按 FailoverReason 组织的错误模式，与 OpenClaw 生产环境保持一致（约 40 个模式）。
var (
	rateLimitPatterns = []errorPattern{
		rxp(`rate[_ ]limit`),
		substr("too many requests"),
		substr("429"),
		substr("exceeded your current quota"),
		rxp(`exceeded.*quota`),
		rxp(`resource has been exhausted`),
		rxp(`resource.*exhausted`),
		substr("resource_exhausted"),
		substr("quota exceeded"),
		substr("usage limit"),
	}

	overloadedPatterns = []errorPattern{
		rxp(`overloaded_error`),
		rxp(`"type"\s*:\s*"overloaded_error"`),
		substr("overloaded"),
	}

	timeoutPatterns = []errorPattern{
		substr("timeout"),
		substr("timed out"),
		substr("deadline exceeded"),
		substr("context deadline exceeded"),
	}

	networkPatterns = []errorPattern{
		substr("connection reset"),
		substr("reset by peer"),
		substr("connection refused"),
		substr("connection aborted"),
		substr("broken pipe"),
		substr("use of closed network connection"),
		substr("network is unreachable"),
		substr("host is unreachable"),
		substr("no such host"),
		substr("temporary failure in name resolution"),
		substr("server misbehaving"),
		substr("read tcp"),
		substr("write tcp"),
		substr("dial tcp"),
		substr("tls:"),
		substr("x509:"),
		substr("certificate"),
		substr("handshake"),
		substr("unexpected eof"),
		substr("read: eof"),
		substr("write: eof"),
	}

	billingPatterns = []errorPattern{
		rxp(`\b402\b`),
		substr("payment required"),
		substr("insufficient credits"),
		substr("credit balance"),
		substr("plans & billing"),
		substr("insufficient balance"),
	}

	authPatterns = []errorPattern{
		rxp(`invalid[_ ]?api[_ ]?key`),
		substr("incorrect api key"),
		substr("invalid token"),
		substr("authentication"),
		substr("re-authenticate"),
		substr("oauth token refresh failed"),
		substr("unauthorized"),
		substr("forbidden"),
		substr("access denied"),
		substr("expired"),
		substr("token has expired"),
		rxp(`\b401\b`),
		rxp(`\b403\b`),
		substr("no credentials found"),
		substr("no api key found"),
	}

	formatPatterns = []errorPattern{
		substr("string should match pattern"),
		substr("tool_use.id"),
		substr("tool_use_id"),
		substr("messages.1.content.1.tool_use.id"),
		substr("invalid request format"),
		// 智谱 API 错误码 1210：参数错误（例如图片格式不兼容）
		substr("error code: 1210"),
		substr("error code 1210"),
		substr("zhipu api error code: 1210"),
	}
	contextOverflowPatterns = []errorPattern{
		rxp(`context[_ ]?length[_ ]?exceeded`),
		rxp(`context[_ ]?window[_ ]?exceeded`),
		substr("maximum context length"),
		substr("token limit"),
		substr("too many tokens"),
		substr("prompt is too long"),
		substr("request too large"),
	}

	imageDimensionPatterns = []errorPattern{
		rxp(`image dimensions exceed max`),
	}

	imageSizePatterns = []errorPattern{
		rxp(`image exceeds.*mb`),
	}

	// 映射为超时的瞬时 HTTP 状态码（服务端故障）。
	transientStatusCodes = map[int]bool{
		500: true, 502: true, 503: true,
		521: true, 522: true, 523: true, 524: true,
		529: true,
	}
)

// ClassifyError 将错误归类为带原因说明的 FailoverError。
// 若错误无法归类则返回 nil（未知错误不应触发回退）。
func ClassifyError(err error, provider, model string) *FailoverError {
	if err == nil {
		return nil
	}

	// 上下文取消：用户中止，绝不回退。
	if err == context.Canceled {
		return nil
	}

	// 上下文超过截止时间：按超时处理，始终回退。
	if err == context.DeadlineExceeded {
		return &FailoverError{
			Reason:   FailoverTimeout,
			Provider: provider,
			Model:    model,
			Wrapped:  err,
		}
	}

	msg := strings.ToLower(err.Error())

	// 即使 provider 未暴露结构化的 HTTP 状态码，
	// 具体的传输层错误也应继续执行回退链。
	if reason := classifyByErrorType(err); reason != "" {
		return &FailoverError{
			Reason:   reason,
			Provider: provider,
			Model:    model,
			Wrapped:  err,
		}
	}

	// 图片尺寸/大小错误：不可重试，也不回退。
	if IsImageDimensionError(msg) || IsImageSizeError(msg) {
		return &FailoverError{
			Reason:   FailoverFormat,
			Provider: provider,
			Model:    model,
			Wrapped:  err,
		}
	}

	// 首先尝试提取 HTTP 状态码。
	if status := extractHTTPStatus(msg); status > 0 {
		if reason := classifyByStatus(status); reason != "" {
			return &FailoverError{
				Reason:   reason,
				Provider: provider,
				Model:    model,
				Status:   status,
				Wrapped:  err,
			}
		}
	}

	// 消息模式匹配（优先级顺序与 OpenClaw 一致）。
	if reason := classifyByMessage(msg); reason != "" {
		return &FailoverError{
			Reason:   reason,
			Provider: provider,
			Model:    model,
			Wrapped:  err,
		}
	}

	return nil
}

// classifyByErrorType 在应用消息启发式判断之前，
// 将具体的传输层错误类型映射为可重试的回退原因。
func classifyByErrorType(err error) FailoverReason {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return FailoverNetwork
	}

	for _, transportErr := range []error{
		syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.ECONNREFUSED,
		syscall.ETIMEDOUT,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
		syscall.EPIPE,
	} {
		if errors.Is(err, transportErr) {
			if transportErr == syscall.ETIMEDOUT {
				return FailoverTimeout
			}
			return FailoverNetwork
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return FailoverTimeout
		}
		return FailoverNetwork
	}

	return ""
}

// classifyByStatus 将 HTTP 状态码映射为 FailoverReason。
func classifyByStatus(status int) FailoverReason {
	switch {
	case status == 401 || status == 403:
		return FailoverAuth
	case status == 402:
		return FailoverBilling
	case status == 408:
		return FailoverTimeout
	case status == 429:
		return FailoverRateLimit
	case status == 400:
		return FailoverFormat
	case transientStatusCodes[status]:
		return FailoverTimeout
	}
	return ""
}

// classifyByMessage 将错误消息与模式进行匹配。
// 优先级顺序很重要（来自 OpenClaw 的 classifyFailoverReason）。
func classifyByMessage(msg string) FailoverReason {
	if matchesAny(msg, rateLimitPatterns) {
		return FailoverRateLimit
	}
	if matchesAny(msg, overloadedPatterns) {
		return FailoverRateLimit // 过载按 rate_limit 处理
	}
	if matchesAny(msg, billingPatterns) {
		return FailoverBilling
	}
	if matchesAny(msg, timeoutPatterns) {
		return FailoverTimeout
	}
	if matchesAny(msg, networkPatterns) {
		return FailoverNetwork
	}
	if matchesAny(msg, authPatterns) {
		return FailoverAuth
	}
	if matchesAny(msg, formatPatterns) {
		return FailoverFormat
	}
	if matchesAny(msg, contextOverflowPatterns) {
		return FailoverContextOverflow
	}
	return ""
}

// extractHTTPStatus 从错误消息中提取 HTTP 状态码。
// 查找诸如 "status: 429"、"status 429"、"http/1.1 429"、"http 429" 或单独出现的 "429" 等模式。
func extractHTTPStatus(msg string) int {
	for _, p := range httpStatusPatterns {
		if m := p.FindStringSubmatch(msg); len(m) > 1 {
			return parseDigits(m[1])
		}
	}
	return 0
}

// IsImageDimensionError 在消息表示图片尺寸错误时返回 true。
func IsImageDimensionError(msg string) bool {
	return matchesAny(msg, imageDimensionPatterns)
}

// IsImageSizeError 在消息表示图片文件大小错误时返回 true。
func IsImageSizeError(msg string) bool {
	return matchesAny(msg, imageSizePatterns)
}

// matchesAny 检查 msg 是否匹配任一模式。
func matchesAny(msg string, patterns []errorPattern) bool {
	for _, p := range patterns {
		if p.regex != nil {
			if p.regex.MatchString(msg) {
				return true
			}
		} else if p.substring != "" {
			if strings.Contains(msg, p.substring) {
				return true
			}
		}
	}
	return false
}

// parseDigits 将数字字符串转换为 int。
func parseDigits(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
