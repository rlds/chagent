package channels

import (
	"fmt"
	"sync"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
)

// ChannelFactory 是一个构造函数，根据配置和消息总线创建 Channel。
// 每个 channel 子包通过 init() 注册一个或多个工厂。
// channelName 是该 channel 实例的配置 map 键（可能与 channel 类型不同）。
// channelType 是用于查找 Channel 配置的 channel 类型字符串。
type ChannelFactory func(channelName, channelType string, cfg *config.Config, bus *bus.MessageBus) (Channel, error)

var (
	factoriesMu sync.RWMutex
	factories   = map[string]ChannelFactory{}
)

// RegisterFactory 注册一个具名 channel 工厂。由子包的 init() 函数调用。
func RegisterFactory(name string, f ChannelFactory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[name] = f
}

// RegisterSafeFactory 是一个便捷封装，负责处理 GetDecoded() 的错误检查
// 和类型断言，减少 channel init() 函数中的样板代码。
//
// 用法：
//
//	func init() {
//	    channels.RegisterSafeFactory(config.ChannelTelegram,
//	        func(bc *config.Channel, c *config.TelegramSettings, b *bus.MessageBus) (channels.Channel, error) {
//	            return NewTelegramChannel(bc, c, b)
//	        })
//	}
func RegisterSafeFactory[S any](
	channelType string,
	ctor func(bc *config.Channel, settings *S, bus *bus.MessageBus) (Channel, error),
) {
	RegisterFactory(channelType, func(channelName, _ string, cfg *config.Config, b *bus.MessageBus) (Channel, error) {
		bc := cfg.Channels[channelName]
		if bc == nil {
			return nil, fmt.Errorf("channel %q: config not found", channelName)
		}
		decoded, err := bc.GetDecoded()
		if err != nil {
			return nil, fmt.Errorf("channel %q: failed to decode settings: %w", channelName, err)
		}
		settings, ok := decoded.(*S)
		if !ok {
			return nil, fmt.Errorf("channel %q: expected %T settings, got %T", channelName, (*S)(nil), decoded)
		}
		return ctor(bc, settings, b)
	})
}

// getFactory 按名称查找 channel 工厂。
func getFactory(name string) (ChannelFactory, bool) {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	f, ok := factories[name]
	return f, ok
}

// GetRegisteredFactoryNames 返回所有已注册 channel 工厂名称的切片。
func GetRegisteredFactoryNames() []string {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	return names
}
