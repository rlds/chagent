package pico

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/channels"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/identity"
	"github.com/rlds/chagent/pkg/logger"
)

// PicoClientChannel 连接到远程 Pico Protocol WebSocket 服务器。
type PicoClientChannel struct {
	*channels.BaseChannel
	config *config.PicoClientSettings
	conn   *picoConn
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

// NewPicoClientChannel 创建一个新的 Pico Protocol 客户端 channel。
func NewPicoClientChannel(
	bc *config.Channel,
	cfg *config.PicoClientSettings,
	messageBus *bus.MessageBus,
) (*PicoClientChannel, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("pico_client url is required")
	}

	base := channels.NewBaseChannel("pico_client", cfg, messageBus, bc.AllowFrom)

	return &PicoClientChannel{
		BaseChannel: base,
		config:      cfg,
	}, nil
}

// Start 拨号连接远程服务器并开始读取。
func (c *PicoClientChannel) Start(ctx context.Context) error {
	logger.InfoC("pico_client", "Starting Pico Client channel")
	c.ctx, c.cancel = context.WithCancel(ctx)

	if err := c.dial(); err != nil {
		c.cancel()
		return fmt.Errorf("pico_client initial connect: %w", err)
	}

	c.SetRunning(true)
	go c.reconnectLoop()

	logger.InfoCF("pico_client", "Connected", map[string]any{"url": c.config.URL})
	return nil
}

// Stop 关闭连接。
func (c *PicoClientChannel) Stop(ctx context.Context) error {
	logger.InfoC("pico_client", "Stopping Pico Client channel")
	c.SetRunning(false)
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Lock()
	if c.conn != nil {
		c.conn.close()
	}
	c.mu.Unlock()
	logger.InfoC("pico_client", "Pico Client channel stopped")
	return nil
}

func (c *PicoClientChannel) dial() error {
	header := http.Header{}
	if c.config.Token.String() != "" {
		header.Set("Authorization", "Bearer "+c.config.Token.String())
	}

	ws, resp, err := websocket.DefaultDialer.DialContext(c.ctx, c.config.URL, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return err
	}

	connCtx, connCancel := context.WithCancel(c.ctx)

	pc := &picoConn{
		id:        uuid.New().String(),
		conn:      ws,
		sessionID: c.config.SessionID,
		cancel:    connCancel,
	}
	if pc.sessionID == "" {
		pc.sessionID = uuid.New().String()
	}

	c.mu.Lock()
	c.conn = pc
	c.mu.Unlock()

	go c.readLoop(connCtx, pc)
	return nil
}

// reconnectLoop 在连接断开时重新拨号。
func (c *PicoClientChannel) reconnectLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.mu.Lock()
		pc := c.conn
		c.mu.Unlock()

		if pc == nil || pc.closed.Load() {
			backoff := 5 * time.Second
			logger.InfoC("pico_client", "Reconnecting...")
			if err := c.dial(); err != nil {
				logger.WarnCF("pico_client", "Reconnect failed", map[string]any{
					"error": err.Error(),
				})
				select {
				case <-c.ctx.Done():
					return
				case <-time.After(backoff):
				}
				continue
			}
			logger.InfoC("pico_client", "Reconnected")
		}

		select {
		case <-c.ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}

func (c *PicoClientChannel) readLoop(connCtx context.Context, pc *picoConn) {
	defer pc.close()

	readTimeout := time.Duration(c.config.ReadTimeout) * time.Second
	if readTimeout <= 0 {
		readTimeout = 60 * time.Second
	}

	_ = pc.conn.SetReadDeadline(time.Now().Add(readTimeout))
	pc.conn.SetPongHandler(func(string) error {
		return pc.conn.SetReadDeadline(time.Now().Add(readTimeout))
	})

	pingInterval := time.Duration(c.config.PingInterval) * time.Second
	if pingInterval <= 0 {
		pingInterval = 30 * time.Second
	}
	go c.pingLoop(connCtx, pc, pingInterval)

	for {
		select {
		case <-connCtx.Done():
			return
		default:
		}

		_, raw, err := pc.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(
				err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
			) {
				logger.DebugCF("pico_client", "Read error", map[string]any{
					"error": err.Error(),
				})
			}
			return
		}

		_ = pc.conn.SetReadDeadline(time.Now().Add(readTimeout))

		var msg PicoMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		c.handleInbound(pc, msg)
	}
}

func (c *PicoClientChannel) pingLoop(connCtx context.Context, pc *picoConn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-connCtx.Done():
			return
		case <-ticker.C:
			if pc.closed.Load() {
				return
			}
			pc.writeMu.Lock()
			err := pc.conn.WriteMessage(websocket.PingMessage, nil)
			pc.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// handleInbound 处理来自远程服务器的消息。
// 在客户端模式下，服务器发送 message.create（响应），客户端
// 发送 message.send（用户输入）。我们将服务器发来的 message.create
// 视为入站用户消息，送入 agent 循环。
func (c *PicoClientChannel) handleInbound(pc *picoConn, msg PicoMessage) {
	switch msg.Type {
	case TypePong:
		// 对 ping 的响应，忽略
	case TypeMessageCreate:
		// 服务器发来消息 — 视为入站消息
		c.handleServerMessage(pc, msg)
	case TypeMediaCreate:
		c.handleServerMessage(pc, msg)
	default:
		logger.DebugCF("pico_client", "Ignoring message type", map[string]any{
			"type": msg.Type,
		})
	}
}

func (c *PicoClientChannel) handleServerMessage(pc *picoConn, msg PicoMessage) {
	if isThoughtPayload(msg.Payload) {
		return
	}

	content, _ := msg.Payload[PayloadKeyContent].(string)
	media, err := parseInlineImageMedia(msg.Payload)
	if err != nil {
		logger.WarnCF("pico_client", "Ignoring invalid media payload", map[string]any{
			"error": err.Error(),
		})
		if strings.TrimSpace(content) == "" {
			return
		}
		media = nil
	}
	if strings.TrimSpace(content) == "" && len(media) == 0 {
		return
	}

	sessionID := msg.SessionID
	if sessionID == "" {
		sessionID = pc.sessionID
	}

	chatID := "pico_client:" + sessionID
	senderID := "pico-remote"
	sender := bus.SenderInfo{
		Platform:    "pico_client",
		PlatformID:  senderID,
		CanonicalID: identity.BuildCanonicalID("pico_client", senderID),
	}

	if !c.IsAllowedSender(sender) {
		return
	}

	inboundCtx := bus.InboundContext{
		Channel:   "pico_client",
		ChatID:    chatID,
		ChatType:  "direct",
		SenderID:  senderID,
		MessageID: msg.ID,
		Raw: map[string]string{
			"platform":   "pico_client",
			"session_id": sessionID,
		},
	}

	c.HandleInboundContext(c.ctx, chatID, content, media, inboundCtx, sender)
}

// Send 向远程服务器发送消息。
func (c *PicoClientChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}
	c.mu.Lock()
	pc := c.conn
	c.mu.Unlock()
	if pc == nil || pc.closed.Load() {
		return nil, channels.ErrSendFailed
	}

	outMsg := newMessage(TypeMessageSend, map[string]any{
		PayloadKeyContent: msg.Content,
	})
	outMsg.SessionID = strings.TrimPrefix(msg.ChatID, "pico_client:")
	return nil, pc.writeJSON(outMsg)
}

// SendExecutionReport 实现 channels.ExecutionReportSender。
//
// 客户端模式下执行上报是 client → server 方向：复用已建立的长连接发送
// `execution.report` 帧。没有连接时立即返回 ErrSendFailed，调用方只记日志。
func (c *PicoClientChannel) SendExecutionReport(
	ctx context.Context,
	chatID string,
	payload map[string]any,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}
	c.mu.Lock()
	pc := c.conn
	c.mu.Unlock()
	if pc == nil || pc.closed.Load() {
		return channels.ErrSendFailed
	}

	reportPayload := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		reportPayload[key] = value
	}
	if _, ok := reportPayload[PayloadKeyKind]; !ok {
		reportPayload[PayloadKeyKind] = MessageKindExecutionReport
	}
	eventID, _ := reportPayload["event_id"].(string)
	if strings.TrimSpace(eventID) != "" {
		reportPayload["message_id"] = eventID
	}

	outMsg := newMessage(TypeExecutionReport, reportPayload)
	outMsg.ID = strings.TrimSpace(eventID)
	outMsg.SessionID = strings.TrimPrefix(chatID, config.ChannelPicoClient+":")
	return pc.writeJSON(outMsg)
}

// StartTyping 实现 channels.TypingCapable。
func (c *PicoClientChannel) StartTyping(ctx context.Context, chatID string) (func(), error) {
	c.mu.Lock()
	pc := c.conn
	c.mu.Unlock()
	if pc == nil || pc.closed.Load() {
		return func() {}, nil
	}

	startMsg := newMessage(TypeTypingStart, nil)
	startMsg.SessionID = strings.TrimPrefix(chatID, "pico_client:")
	if err := pc.writeJSON(startMsg); err != nil {
		return func() {}, err
	}
	return func() {
		c.mu.Lock()
		currentPC := c.conn
		c.mu.Unlock()
		if currentPC == nil {
			return
		}
		stopMsg := newMessage(TypeTypingStop, nil)
		stopMsg.SessionID = strings.TrimPrefix(chatID, "pico_client:")
		currentPC.writeJSON(stopMsg)
	}, nil
}
