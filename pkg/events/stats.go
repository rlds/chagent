package events

// Stats 报告 EventBus 的聚合计数器。
type Stats struct {
	Published   uint64
	Matched     uint64
	Delivered   uint64
	Dropped     uint64
	Blocked     uint64
	Closed      bool
	Subscribers int

	SubscriberStats []SubscriberStats
}

// SubscriberStats 报告单个订阅的计数器。
type SubscriberStats struct {
	ID       uint64
	Name     string
	Received uint64
	Handled  uint64
	Failed   uint64
	Dropped  uint64
	Panicked uint64
	TimedOut uint64
}
