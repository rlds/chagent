package events

import (
	"context"
	"errors"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

const defaultSubscriberBuffer = 16

var (
	// ErrBusClosed 在向已关闭的事件总线发起订阅时返回。
	ErrBusClosed = errors.New("events: bus is closed")
	// ErrNilHandler 在未提供 handler 的情况下发起订阅时返回。
	ErrNilHandler = errors.New("events: handler is nil")
)

// Handler 处理投递给某个订阅的运行时事件。
type Handler func(context.Context, Event) error

// SubscribeOptions 控制订阅接收事件的方式。
type SubscribeOptions struct {
	Name         string
	Buffer       int
	Priority     int
	Concurrency  ConcurrencyKind
	Backpressure BackpressurePolicy
	// Timeout 限制订阅 worker 等待单次 handler 调用的时长。
	// handler 仍应遵守 ctx 取消；超时的调用会继续运行，
	// 直到其 handler 返回。
	Timeout     time.Duration
	PanicPolicy PanicPolicy
}

// ConcurrencyKind 控制 handler 订阅处理已排队事件的方式。
type ConcurrencyKind string

const (
	// Concurrent 为每个事件单独启动 goroutine 处理。
	Concurrent ConcurrencyKind = "concurrent"
	// Locked 按订阅顺序串行处理事件。
	Locked ConcurrencyKind = "locked"
	// Keyed 预留给按 key 串行处理的场景，当前行为等同于 Locked。
	Keyed ConcurrencyKind = "keyed"
)

// BackpressurePolicy 控制订阅队列已满时的投递行为。
type BackpressurePolicy string

const (
	// DropNewest 在队列已满时丢弃正在发布的事件。
	DropNewest BackpressurePolicy = "drop_newest"
	// DropOldest 丢弃一个已排队事件，并将正在发布的事件入队。
	DropOldest BackpressurePolicy = "drop_oldest"
	// Block 等待队列有空闲容量，直到 Publish 的 context 被取消。
	Block BackpressurePolicy = "block"
)

// PanicPolicy 控制 handler 发生 panic 时的行为。
type PanicPolicy string

const (
	// RecoverAndLog 捕获 handler 的 panic，并记录到订阅统计中。
	RecoverAndLog PanicPolicy = "recover_and_log"
	// Crash 让 handler 的 panic 从 worker goroutine 中向外传播。
	Crash PanicPolicy = "crash"
)

// Subscription 表示一个活动的事件订阅。
type Subscription interface {
	ID() uint64
	Name() string
	Close() error
	Done() <-chan struct{}
	Stats() SubscriberStats
}

type subscriberCounters struct {
	received atomic.Uint64
	handled  atomic.Uint64
	failed   atomic.Uint64
	dropped  atomic.Uint64
	panicked atomic.Uint64
	timedOut atomic.Uint64
}

type eventSubscription struct {
	bus     *EventBus
	id      uint64
	name    string
	opts    SubscribeOptions
	filters []Filter
	handler Handler
	once    bool

	ch      chan Event
	done    chan struct{}
	closing chan struct{}

	closeOnce sync.Once
	doneOnce  sync.Once
	mu        sync.RWMutex
	closed    bool
	wg        sync.WaitGroup
	blockWG   sync.WaitGroup

	counters subscriberCounters
}

type handlerResult struct {
	err      error
	panicked bool
}

func normalizeSubscribeOptions(opts SubscribeOptions) SubscribeOptions {
	if opts.Buffer <= 0 {
		opts.Buffer = defaultSubscriberBuffer
	}
	if opts.Concurrency == "" {
		opts.Concurrency = Locked
	}
	if opts.Backpressure == "" {
		opts.Backpressure = DropNewest
	}
	if opts.PanicPolicy == "" {
		opts.PanicPolicy = RecoverAndLog
	}
	return opts
}

func newSubscription(
	bus *EventBus,
	id uint64,
	filters []Filter,
	opts SubscribeOptions,
	handler Handler,
	once bool,
) *eventSubscription {
	opts = normalizeSubscribeOptions(opts)
	return &eventSubscription{
		bus:     bus,
		id:      id,
		name:    opts.Name,
		opts:    opts,
		filters: append([]Filter(nil), filters...),
		handler: handler,
		once:    once,
		ch:      make(chan Event, opts.Buffer),
		done:    make(chan struct{}),
		closing: make(chan struct{}),
	}
}

// ID 返回订阅标识符。
func (s *eventSubscription) ID() uint64 {
	if s == nil {
		return 0
	}
	return s.id
}

// Name 返回订阅名称。
func (s *eventSubscription) Name() string {
	if s == nil {
		return ""
	}
	return s.name
}

// Close 移除订阅并关闭其投递通道。
func (s *eventSubscription) Close() error {
	if s == nil || s.bus == nil {
		return nil
	}
	s.bus.unsubscribe(s.id)
	return nil
}

// Done 返回一个通道，订阅停止处理后会关闭该通道。
func (s *eventSubscription) Done() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.done
}

// Stats 返回订阅计数器的快照。
func (s *eventSubscription) Stats() SubscriberStats {
	if s == nil {
		return SubscriberStats{}
	}
	return SubscriberStats{
		ID:       s.id,
		Name:     s.name,
		Received: s.counters.received.Load(),
		Handled:  s.counters.handled.Load(),
		Failed:   s.counters.failed.Load(),
		Dropped:  s.counters.dropped.Load(),
		Panicked: s.counters.panicked.Load(),
		TimedOut: s.counters.timedOut.Load(),
	}
}

func (s *eventSubscription) run(ctx context.Context) {
	defer func() {
		s.wg.Wait()
		s.closeDone()
	}()

	for evt := range s.ch {
		s.dispatch(ctx, evt)
		if s.once {
			_ = s.Close()
			return
		}
	}
}

func (s *eventSubscription) dispatch(ctx context.Context, evt Event) {
	switch s.opts.Concurrency {
	case Concurrent:
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("events: subscriber %q goroutine panic recovered: %v\n%s", s.name, r, debug.Stack())
				}
			}()
			s.handle(ctx, evt)
		}()
	case Keyed:
		// TODO: 当运行时事件需要按 scope 保序、同时跨 scope 并发时，
		// 改用 keyed 执行器替换此处。
		s.handle(ctx, evt)
	default:
		s.handle(ctx, evt)
	}
}

func (s *eventSubscription) handle(ctx context.Context, evt Event) {
	if ctx == nil {
		ctx = context.Background()
	}

	if s.opts.Timeout <= 0 {
		s.recordHandlerResult(ctx, s.invokeHandler(ctx, evt))
		return
	}

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	done := make(chan handlerResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf(
					"events: subscriber %q timeout-handler goroutine panic recovered: %v\n%s",
					s.name, r, debug.Stack(),
				)
				done <- handlerResult{panicked: true}
			}
		}()
		done <- s.invokeHandler(ctx, evt)
	}()

	select {
	case result := <-done:
		s.recordHandlerResult(ctx, result)
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.counters.timedOut.Add(1)
		}
		s.counters.failed.Add(1)
	}
}

func (s *eventSubscription) invokeHandler(ctx context.Context, evt Event) (result handlerResult) {
	if s.opts.PanicPolicy != Crash {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.counters.panicked.Add(1)
				result.panicked = true
				log.Printf("events: subscriber %q recovered panic: %v", s.name, recovered)
			}
		}()
	}

	result.err = s.handler(ctx, evt)
	return result
}

func (s *eventSubscription) recordHandlerResult(ctx context.Context, result handlerResult) {
	if result.panicked {
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.counters.timedOut.Add(1)
	}
	if result.err != nil {
		s.counters.failed.Add(1)
		return
	}
	s.counters.handled.Add(1)
}

func (s *eventSubscription) watchContext(ctx context.Context) {
	if ctx == nil {
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf(
					"events: subscriber %q watchContext goroutine panic recovered: %v\n%s",
					s.name, r, debug.Stack(),
				)
			}
		}()
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
}

func (s *eventSubscription) closeInput() {
	s.closeOnce.Do(func() {
		close(s.closing)
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.blockWG.Wait()
		s.mu.Lock()
		close(s.ch)
		s.mu.Unlock()
		if s.handler == nil {
			s.closeDone()
		}
	})
}

func (s *eventSubscription) closeDone() {
	s.doneOnce.Do(func() {
		close(s.done)
	})
}

type deliveryResult struct {
	delivered int
	dropped   int
	blocked   int
	closed    bool
}

func (s *eventSubscription) enqueue(ctx context.Context, evt Event, nonBlocking bool) deliveryResult {
	if ctx == nil {
		ctx = context.Background()
	}

	if nonBlocking {
		return s.enqueueNonBlocking(evt)
	}

	if s.opts.Backpressure == Block {
		return s.enqueueBlocking(ctx, evt)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return deliveryResult{closed: true}
	}

	s.counters.received.Add(1)

	switch s.opts.Backpressure {
	case DropOldest:
		return s.enqueueDropOldest(evt)
	default:
		return s.enqueueDropNewest(evt)
	}
}

func (s *eventSubscription) enqueueBlocking(ctx context.Context, evt Event) deliveryResult {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return deliveryResult{closed: true}
	}
	s.blockWG.Add(1)
	s.counters.received.Add(1)
	s.mu.Unlock()

	defer s.blockWG.Done()
	return s.enqueueBlock(ctx, evt)
}

func (s *eventSubscription) enqueueNonBlocking(evt Event) deliveryResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return deliveryResult{closed: true}
	}

	s.counters.received.Add(1)
	if s.opts.Backpressure == DropOldest {
		return s.enqueueDropOldest(evt)
	}
	return s.enqueueDropNewest(evt)
}

func (s *eventSubscription) enqueueDropNewest(evt Event) deliveryResult {
	select {
	case <-s.closing:
		return deliveryResult{closed: true}
	default:
	}

	select {
	case s.ch <- evt:
		return deliveryResult{delivered: 1}
	default:
		s.counters.dropped.Add(1)
		return deliveryResult{dropped: 1}
	}
}

func (s *eventSubscription) enqueueDropOldest(evt Event) deliveryResult {
	select {
	case <-s.closing:
		return deliveryResult{closed: true}
	default:
	}

	select {
	case s.ch <- evt:
		return deliveryResult{delivered: 1}
	default:
	}

	dropped := 0
	select {
	case <-s.ch:
		s.counters.dropped.Add(1)
		dropped = 1
	default:
	}

	select {
	case <-s.closing:
		return deliveryResult{dropped: dropped, closed: true}
	case s.ch <- evt:
		return deliveryResult{delivered: 1, dropped: dropped}
	default:
		s.counters.dropped.Add(1)
		return deliveryResult{dropped: dropped + 1}
	}
}

func (s *eventSubscription) enqueueBlock(ctx context.Context, evt Event) deliveryResult {
	select {
	case <-s.closing:
		return deliveryResult{closed: true}
	case s.ch <- evt:
		return deliveryResult{delivered: 1}
	case <-ctx.Done():
		s.counters.dropped.Add(1)
		return deliveryResult{dropped: 1, blocked: 1}
	}
}
