// Chagent - 超轻量级个人 AI 智能体

package agent

import (
	"github.com/rlds/chagent/pkg/agent/interfaces"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/media"
	"github.com/rlds/chagent/pkg/providers"
)

// Pipeline 持有 Pipeline 各方法使用的运行时依赖。
// 它由 runTurn 通过 NewPipeline 构造，并传给各个子方法，
// 以便协调器委派各阶段的执行。
type Pipeline struct {
	Bus            interfaces.MessageBus
	Cfg            *config.Config
	ContextManager ContextManager
	Hooks          *HookManager
	Fallback       *providers.FallbackChain
	ChannelManager interfaces.ChannelManager
	MediaStore     media.MediaStore
	Steering       any // TODO: *Steering
	al             *AgentLoop
}

// NewPipeline 从 AgentLoop 实例创建 Pipeline。
func NewPipeline(al *AgentLoop) *Pipeline {
	return &Pipeline{
		Bus:            al.bus,
		Cfg:            al.GetConfig(),
		ContextManager: al.contextManager,
		Hooks:          al.hooks,
		Fallback:       al.fallback,
		ChannelManager: al.channelManager,
		MediaStore:     al.mediaStore,
		Steering:       al.steering,
		al:             al,
	}
}
