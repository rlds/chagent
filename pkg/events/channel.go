package events

import "context"

// EventChannel 是 EventBus 上的一个过滤视图。
type EventChannel interface {
	Filter(filter Filter) EventChannel
	OfKind(kinds ...Kind) EventChannel
	KindPrefix(prefix string) EventChannel
	Source(component string, names ...string) EventChannel
	Scope(scope ScopeFilter) EventChannel

	Subscribe(ctx context.Context, opts SubscribeOptions, handler Handler) (Subscription, error)
	SubscribeChan(ctx context.Context, opts SubscribeOptions) (Subscription, <-chan Event, error)
	SubscribeOnce(ctx context.Context, opts SubscribeOptions, handler Handler) (Subscription, error)
}

type eventChannel struct {
	bus     *EventBus
	filters []Filter
}

// Filter 返回一个追加了 filter 的新 EventChannel。
func (c eventChannel) Filter(filter Filter) EventChannel {
	filters := append([]Filter(nil), c.filters...)
	if filter != nil {
		filters = append(filters, filter)
	}
	return eventChannel{bus: c.bus, filters: filters}
}

// OfKind 返回一个新的 EventChannel，匹配 kinds 中的任意一种事件。
func (c eventChannel) OfKind(kinds ...Kind) EventChannel {
	return c.Filter(MatchKind(kinds...))
}

// KindPrefix 返回一个新的 EventChannel，匹配带有该 kind 前缀的事件。
func (c eventChannel) KindPrefix(prefix string) EventChannel {
	return c.Filter(MatchKindPrefix(prefix))
}

// Source 返回一个新的 EventChannel，按来源组件及可选名称进行匹配。
func (c eventChannel) Source(component string, names ...string) EventChannel {
	return c.Filter(MatchSource(component, names...))
}

// Scope 返回一个新的 EventChannel，按非空的 scope 字段进行匹配。
func (c eventChannel) Scope(scope ScopeFilter) EventChannel {
	return c.Filter(MatchScope(scope))
}

// Subscribe 为匹配该通道的事件注册 handler。
func (c eventChannel) Subscribe(ctx context.Context, opts SubscribeOptions, handler Handler) (Subscription, error) {
	if handler == nil {
		return nil, ErrNilHandler
	}
	return c.bus.subscribe(ctx, c.filters, opts, handler, false)
}

// SubscribeChan 为匹配该通道的事件注册一个通道式订阅。
func (c eventChannel) SubscribeChan(ctx context.Context, opts SubscribeOptions) (Subscription, <-chan Event, error) {
	sub, err := c.bus.subscribe(ctx, c.filters, opts, nil, false)
	if err != nil {
		return nil, nil, err
	}
	return sub, sub.(*eventSubscription).ch, nil
}

// SubscribeOnce 注册 handler，并在收到第一个事件后关闭订阅。
func (c eventChannel) SubscribeOnce(ctx context.Context, opts SubscribeOptions, handler Handler) (Subscription, error) {
	if handler == nil {
		return nil, ErrNilHandler
	}
	return c.bus.subscribe(ctx, c.filters, opts, handler, true)
}
