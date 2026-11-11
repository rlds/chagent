package channels

import (
	"net/http"
	"strings"
	"sync"
)

// dynamicServeMux 是一个 http.Handler，支持在无需重新创建服务器的情况下
// 动态注册与注销处理器。
type dynamicServeMux struct {
	mu       sync.RWMutex
	handlers map[string]http.Handler
}

func newDynamicServeMux() *dynamicServeMux {
	return &dynamicServeMux{
		handlers: make(map[string]http.Handler),
	}
}

// Handle 为给定模式注册处理器。
func (dm *dynamicServeMux) Handle(pattern string, handler http.Handler) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.handlers[pattern] = handler
}

// HandleFunc 为给定模式注册处理器函数。
func (dm *dynamicServeMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	dm.Handle(pattern, http.HandlerFunc(handler))
}

// Unhandle 移除给定模式的处理器。
func (dm *dynamicServeMux) Unhandle(pattern string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	delete(dm.handlers, pattern)
}

// ServeHTTP 将请求分发给其模式与请求 URL 路径最匹配的处理器。
// 它同时支持精确路径匹配和子树（以斜杠结尾）前缀匹配，
// 冲突时选择最长的前缀。
func (dm *dynamicServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	path := r.URL.Path

	// 优先进行精确匹配。
	if h, ok := dm.handlers[path]; ok {
		h.ServeHTTP(w, r)
		return
	}

	// 最长子树前缀匹配（即以 "/" 结尾的模式）。
	var bestLen int
	var bestHandler http.Handler
	for pattern, handler := range dm.handlers {
		if strings.HasSuffix(pattern, "/") && strings.HasPrefix(path, pattern) {
			if len(pattern) > bestLen {
				bestLen = len(pattern)
				bestHandler = handler
			}
		}
	}

	if bestHandler != nil {
		bestHandler.ServeHTTP(w, r)
		return
	}

	http.NotFound(w, r)
}
