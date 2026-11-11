package channels

import "net/http"

// WebhookHandler 是一个可选接口，面向通过 HTTP webhook 接收消息的通道。
// Manager 会发现实现此接口的通道，
// 并将它们注册到共享的 HTTP 服务器上。
type WebhookHandler interface {
	// WebhookPath 返回将该处理器挂载到共享服务器上的路径。
	// 示例："/webhook/line"、"/webhook/wecom"
	WebhookPath() string
	http.Handler // ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// HealthChecker 是一个可选接口，面向在共享 HTTP 服务器上
// 暴露健康检查端点的通道。
type HealthChecker interface {
	HealthPath() string
	HealthHandler(w http.ResponseWriter, r *http.Request)
}
