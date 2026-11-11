package pico

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/channels"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/media"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newRunningChannel(t *testing.T, mutate func(*config.PicoSettings)) (*PicoChannel, *bus.MessageBus) {
	t.Helper()
	bc := &config.Channel{Type: config.ChannelPico, Enabled: true}
	cfg := &config.PicoSettings{}
	cfg.SetToken("test-token")
	if mutate != nil {
		mutate(cfg)
	}
	msgBus := bus.NewMessageBus()
	ch, err := NewPicoChannel(bc, cfg, msgBus)
	if err != nil {
		t.Fatalf("NewPicoChannel: %v", err)
	}
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Stop(context.Background()) })
	return ch, msgBus
}

func newPicoHTTPServer(t *testing.T, ch *PicoChannel) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(ch.ServeHTTP))
	t.Cleanup(srv.Close)
	return srv
}

func picoWSURL(srv *httptest.Server, query string) string {
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/pico/ws"
	if query != "" {
		base += "?" + query
	}
	return base
}

func dialPico(t *testing.T, srv *httptest.Server, query string, header http.Header,
	subprotocols ...string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = subprotocols
	dialer.HandshakeTimeout = 3 * time.Second
	return dialer.Dial(picoWSURL(srv, query), header)
}

func mustDialPico(t *testing.T, srv *httptest.Server, query string, header http.Header,
	subprotocols ...string) *websocket.Conn {
	t.Helper()
	conn, resp, err := dialPico(t, srv, query, header, subprotocols...)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial failed (status=%d): %v", status, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func authHeader() http.Header {
	return http.Header{"Authorization": []string{"Bearer test-token"}}
}

func readPicoMessage(t *testing.T, conn *websocket.Conn, timeout time.Duration) PicoMessage {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var msg PicoMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read message: %v", err)
	}
	return msg
}

func sendJSON(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	if err := conn.WriteJSON(value); err != nil {
		t.Fatalf("write message: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func testDataURL(mimeType string, payload []byte) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(payload)
}

func newTestOutboundMessage(chatID, content string, raw map[string]string) bus.OutboundMessage {
	return bus.OutboundMessage{
		Channel: "pico",
		ChatID:  chatID,
		Content: content,
		Context: bus.InboundContext{Raw: raw},
	}
}

// ---------------------------------------------------------------------------
// HTTP endpoint branches
// ---------------------------------------------------------------------------

func TestServeHTTP_ErrorBranches(t *testing.T) {
	// Not running channel: /ws and /media both return 503.
	stopped := newTestPicoChannel(t)
	stoppedSrv := newPicoHTTPServer(t, stopped)
	resp, err := http.Get(stoppedSrv.URL + "/pico/ws")
	if err != nil {
		t.Fatalf("GET /pico/ws: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("not-running /ws status = %d, want 503", resp.StatusCode)
	}
	resp, err = http.Get(stoppedSrv.URL + "/pico/media/abc")
	if err != nil {
		t.Fatalf("GET /pico/media: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("not-running /media status = %d, want 503", resp.StatusCode)
	}

	ch, _ := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, ch)

	// Unknown path -> 404.
	resp, err = http.Get(srv.URL + "/pico/unknown")
	if err != nil {
		t.Fatalf("GET /pico/unknown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", resp.StatusCode)
	}

	// No token -> 401 for both ws and media.
	resp, err = http.Get(srv.URL + "/pico/ws")
	if err != nil {
		t.Fatalf("GET /pico/ws: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthorized /ws status = %d, want 401", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/pico/media/abc")
	if err != nil {
		t.Fatalf("GET /pico/media: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthorized /media status = %d, want 401", resp.StatusCode)
	}

	// Empty media ref -> 404.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/pico/media/", nil)
	req.Header = authHeader()
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET empty media: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("empty media ref status = %d, want 404", resp.StatusCode)
	}

	// No media store -> 503.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/pico/media/abc", nil)
	req.Header = authHeader()
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET media without store: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("nil media store status = %d, want 503", resp.StatusCode)
	}

	// Media store present but ref unknown -> 404.
	ch.SetMediaStore(media.NewFileMediaStore())
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/pico/media/missing", nil)
	req.Header = authHeader()
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET unknown media: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown media ref status = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// WebSocket auth / upgrade
// ---------------------------------------------------------------------------

func TestWebSocket_AuthVariants(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, ch)

	// 1. Authorization header.
	conn := mustDialPico(t, srv, "session_id=auth-header", authHeader())
	sendJSON(t, conn, PicoMessage{Type: TypePing, ID: "p1"})
	if msg := readPicoMessage(t, conn, 2*time.Second); msg.Type != TypePong || msg.ID != "p1" {
		t.Fatalf("header auth ping -> %+v", msg)
	}

	// 2. Sec-WebSocket-Protocol token.<token>.
	subConn, resp, err := dialPico(t, srv, "session_id=auth-sub", nil, "token.test-token")
	if err != nil {
		t.Fatalf("subprotocol dial: %v (status=%v)", err, resp)
	}
	defer subConn.Close()
	if got := subConn.Subprotocol(); got != "token.test-token" {
		t.Errorf("echoed subprotocol = %q, want token.test-token", got)
	}
	sendJSON(t, subConn, PicoMessage{Type: TypePing, ID: "p2"})
	if msg := readPicoMessage(t, subConn, 2*time.Second); msg.Type != TypePong {
		t.Fatalf("subprotocol auth ping -> %+v", msg)
	}

	// Wrong subprotocol token is rejected.
	if _, resp, err := dialPico(t, srv, "session_id=auth-bad-sub", nil, "token.wrong"); err == nil {
		t.Fatal("expected dial failure for wrong subprotocol token")
	} else if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong subprotocol status = %d, want 401", resp.StatusCode)
	}

	// 3. Query token: denied by default.
	if _, resp, err := dialPico(t, srv, "session_id=auth-query&token=test-token", nil); err == nil {
		t.Fatal("query token should be rejected when AllowTokenQuery=false")
	} else if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("query token status = %d, want 401", resp.StatusCode)
	}

	chQuery, _ := newRunningChannel(t, func(cfg *config.PicoSettings) {
		cfg.AllowTokenQuery = true
	})
	srvQuery := newPicoHTTPServer(t, chQuery)
	queryConn := mustDialPico(t, srvQuery, "session_id=auth-query-ok&token=test-token", nil)
	sendJSON(t, queryConn, PicoMessage{Type: TypePing, ID: "p3"})
	if msg := readPicoMessage(t, queryConn, 2*time.Second); msg.Type != TypePong {
		t.Fatalf("query auth ping -> %+v", msg)
	}
}

func TestWebSocket_AllowOriginsCheck(t *testing.T) {
	ch, _ := newRunningChannel(t, func(cfg *config.PicoSettings) {
		cfg.AllowOrigins = []string{"https://allowed.example"}
	})
	check := ch.upgrader.CheckOrigin

	req, _ := http.NewRequest(http.MethodGet, "http://localhost/pico/ws", nil)
	if check(req) {
		t.Error("missing Origin should be rejected when allow list configured")
	}
	req.Header.Set("Origin", "https://evil.example")
	if check(req) {
		t.Error("non-allowed Origin should be rejected")
	}
	req.Header.Set("Origin", "https://allowed.example")
	if !check(req) {
		t.Error("allowed Origin should be accepted")
	}

	chWildcard, _ := newRunningChannel(t, func(cfg *config.PicoSettings) {
		cfg.AllowOrigins = []string{"*"}
	})
	req.Header.Set("Origin", "https://anything.example")
	if !chWildcard.upgrader.CheckOrigin(req) {
		t.Error("wildcard origin should accept any Origin")
	}
}

func TestWebSocket_ConnectionLimitAndNotFoundToken(t *testing.T) {
	ch, _ := newRunningChannel(t, func(cfg *config.PicoSettings) {
		cfg.MaxConnections = 1
	})
	srv := newPicoHTTPServer(t, ch)
	first := mustDialPico(t, srv, "session_id=limit-1", authHeader())
	sendJSON(t, first, PicoMessage{Type: TypePing, ID: "l1"})
	if msg := readPicoMessage(t, first, 2*time.Second); msg.Type != TypePong {
		t.Fatalf("first conn ping -> %+v", msg)
	}
	if _, resp, err := dialPico(t, srv, "session_id=limit-2", authHeader()); err == nil {
		t.Fatal("second connection should be rejected by MaxConnections=1")
	} else if resp != nil && resp.StatusCode != http.StatusServiceUnavailable &&
		resp.StatusCode != http.StatusBadRequest &&
		resp.StatusCode != http.StatusTooManyRequests &&
		resp.StatusCode != http.StatusInternalServerError {
		t.Logf("second connection rejected with status %d", resp.StatusCode)
	}

	// authenticate with an empty configured token always returns false.
	empty := &PicoChannel{config: &config.PicoSettings{}}
	req, _ := http.NewRequest(http.MethodGet, "/pico/ws", nil)
	req.Header = authHeader()
	if empty.authenticate(req) {
		t.Error("authenticate with empty token must be false")
	}
}

// ---------------------------------------------------------------------------
// WebSocket read loop / protocol messages
// ---------------------------------------------------------------------------

func TestWebSocket_ReadLoopProtocolMessages(t *testing.T) {
	ch, msgBus := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, ch)
	conn := mustDialPico(t, srv, "session_id=proto", authHeader())

	// ping -> pong
	sendJSON(t, conn, PicoMessage{Type: TypePing, ID: "ping-1", Timestamp: 123})
	pong := readPicoMessage(t, conn, 2*time.Second)
	if pong.Type != TypePong || pong.ID != "ping-1" {
		t.Fatalf("pong = %+v", pong)
	}

	// invalid JSON -> error invalid_message
	if err := conn.WriteMessage(websocket.TextMessage, []byte("{not-json")); err != nil {
		t.Fatalf("write invalid json: %v", err)
	}
	errMsg := readPicoMessage(t, conn, 2*time.Second)
	if errMsg.Type != TypeError || errMsg.Payload["code"] != "invalid_message" {
		t.Fatalf("invalid json error = %+v", errMsg)
	}

	// unknown type -> error unknown_type
	sendJSON(t, conn, PicoMessage{Type: "nope", ID: "u1"})
	errMsg = readPicoMessage(t, conn, 2*time.Second)
	if errMsg.Type != TypeError || errMsg.Payload["code"] != "unknown_type" {
		t.Fatalf("unknown type error = %+v", errMsg)
	}

	// message.send with content -> inbound bus
	sendJSON(t, conn, PicoMessage{Type: TypeMessageSend, ID: "m1", SessionID: "proto",
		Payload: map[string]any{PayloadKeyContent: "hello"}})
	select {
	case inbound := <-msgBus.InboundChan():
		if inbound.Content != "hello" || inbound.Context.ChatID != "pico:proto" {
			t.Fatalf("inbound = %+v", inbound)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected inbound message")
	}

	// media.send with content -> inbound
	sendJSON(t, conn, PicoMessage{Type: TypeMediaSend, ID: "m2", SessionID: "proto",
		Payload: map[string]any{PayloadKeyContent: "media hello"}})
	select {
	case inbound := <-msgBus.InboundChan():
		if inbound.Content != "media hello" {
			t.Fatalf("media inbound = %+v", inbound)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected inbound media message")
	}

	// empty content -> error empty_content with request id
	sendJSON(t, conn, PicoMessage{Type: TypeMessageSend, ID: "m3", SessionID: "proto",
		Payload: map[string]any{}})
	errMsg = readPicoMessage(t, conn, 2*time.Second)
	if errMsg.Type != TypeError || errMsg.Payload["code"] != "empty_content" ||
		errMsg.Payload["request_id"] != "m3" {
		t.Fatalf("empty content error = %+v", errMsg)
	}

	// invalid media payload -> error invalid_media with request id
	sendJSON(t, conn, PicoMessage{Type: TypeMessageSend, ID: "m4", SessionID: "proto",
		Payload: map[string]any{PayloadKeyContent: "x", "media": 42}})
	errMsg = readPicoMessage(t, conn, 2*time.Second)
	if errMsg.Type != TypeError || errMsg.Payload["code"] != "invalid_media" {
		t.Fatalf("invalid media error = %+v", errMsg)
	}

	// large frame is processed without crashing the loop.
	large := strings.Repeat("x", 1<<20)
	sendJSON(t, conn, PicoMessage{Type: "nope", ID: "large", Payload: map[string]any{"data": large}})
	if errMsg = readPicoMessage(t, conn, 3*time.Second); errMsg.Payload["code"] != "unknown_type" {
		t.Fatalf("large frame error = %+v", errMsg)
	}

	// Client close removes the connection from the registry.
	_ = conn.Close()
	waitFor(t, 3*time.Second, "connection removal", func() bool {
		return ch.currentConnCount() == 0
	})
}

func TestWebSocket_DisconnectReconnectAndStop(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, ch)

	conn1 := mustDialPico(t, srv, "session_id=reconnect", authHeader())
	conn2 := mustDialPico(t, srv, "session_id=reconnect", authHeader())
	if got := ch.currentConnCount(); got != 2 {
		t.Fatalf("currentConnCount = %d, want 2", got)
	}
	_ = conn1.Close()
	waitFor(t, 3*time.Second, "one connection removed", func() bool {
		return ch.currentConnCount() == 1
	})

	// Remaining connection still receives broadcasts.
	if _, err := ch.Send(context.Background(), newTestOutboundMessage("pico:reconnect", "still-here", nil)); err != nil {
		t.Fatalf("Send after disconnect: %v", err)
	}
	msg := readPicoMessage(t, conn2, 2*time.Second)
	if msg.Type != TypeMessageCreate || msg.Payload[PayloadKeyContent] != "still-here" {
		t.Fatalf("broadcast = %+v", msg)
	}

	// Stop closes live connections.
	_ = ch.Stop(context.Background())
	_ = conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn2.ReadMessage(); err == nil {
		t.Error("expected read error after channel Stop")
	}
	if got := ch.currentConnCount(); got != 0 {
		t.Errorf("currentConnCount after Stop = %d, want 0", got)
	}
}

func TestWebSocket_ReadDeadlineClosesIdleConnection(t *testing.T) {
	ch, _ := newRunningChannel(t, func(cfg *config.PicoSettings) {
		cfg.ReadTimeout = 1
		cfg.PingInterval = 30
	})
	srv := newPicoHTTPServer(t, ch)
	conn := mustDialPico(t, srv, "session_id=idle", authHeader())
	// Keep the client silent; the server-side read deadline should expire.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
	waitFor(t, 3*time.Second, "idle connection closed", func() bool {
		return ch.currentConnCount() == 0
	})
}

// ---------------------------------------------------------------------------
// Send / edit / delete / typing / placeholder
// ---------------------------------------------------------------------------

func TestSend_ErrorBranchesAndMessageVariants(t *testing.T) {
	stopped := newTestPicoChannel(t)
	if _, err := stopped.Send(context.Background(), newTestOutboundMessage("pico:s", "x", nil)); err == nil {
		t.Error("Send on stopped channel should fail")
	}

	ch, _ := newRunningChannel(t, nil)
	// No connection for session -> send failure.
	if _, err := ch.Send(context.Background(), newTestOutboundMessage("pico:none", "x", nil)); err == nil {
		t.Error("Send without connections should fail")
	}

	srv := newPicoHTTPServer(t, ch)
	conn := mustDialPico(t, srv, "session_id=send", authHeader())

	// Plain message with model name.
	ids, err := ch.Send(context.Background(), newTestOutboundMessage(
		"pico:send", "plain", map[string]string{PayloadKeyModelName: "mock-model"}))
	if err != nil || len(ids) != 1 {
		t.Fatalf("plain Send = %v, %v", ids, err)
	}
	msg := readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageCreate || msg.Payload[PayloadKeyContent] != "plain" {
		t.Fatalf("plain message = %+v", msg)
	}
	if msg.Payload[PayloadKeyModelName] != "mock-model" {
		t.Errorf("model_name payload = %v", msg.Payload[PayloadKeyModelName])
	}
	if _, ok := msg.Payload["message_id"]; !ok {
		t.Error("message_id missing")
	}

	// Thought message (new kind + legacy bool).
	if _, err = ch.Send(context.Background(), newTestOutboundMessage(
		"pico:send", "thinking", map[string]string{"message_kind": MessageKindThought})); err != nil {
		t.Fatalf("thought Send: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Payload[PayloadKeyKind] != MessageKindThought || msg.Payload[PayloadKeyThought] != true {
		t.Fatalf("thought payload = %+v", msg.Payload)
	}

	// Tool calls with valid raw JSON payload.
	toolCalls := `[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]`
	if _, err = ch.Send(context.Background(), newTestOutboundMessage(
		"pico:send", "", map[string]string{"message_kind": MessageKindToolCalls,
			PayloadKeyToolCalls: toolCalls})); err != nil {
		t.Fatalf("tool_calls Send: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Payload[PayloadKeyKind] != MessageKindToolCalls {
		t.Fatalf("tool_calls kind = %+v", msg.Payload)
	}
	if _, ok := msg.Payload[PayloadKeyToolCalls]; !ok {
		t.Errorf("tool_calls payload missing: %+v", msg.Payload)
	}

	// Invalid tool call JSON is dropped (no tool_calls payload, no error).
	if _, err = ch.Send(context.Background(), newTestOutboundMessage(
		"pico:send", "", map[string]string{"message_kind": MessageKindToolCalls,
			PayloadKeyToolCalls: "{not-json"})); err != nil {
		t.Fatalf("bad tool_calls Send: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if _, ok := msg.Payload[PayloadKeyToolCalls]; ok {
		t.Errorf("invalid tool_calls should be dropped: %+v", msg.Payload)
	}

	// Usage + context usage payloads.
	usage := &protocoltypes.UsageInfo{
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		CachedTokens: 3, ReasoningTokens: 2,
		CacheReadInputTokens: 4, CacheCreationInputTokens: 1,
	}
	contextUsage := &bus.ContextUsage{
		UsedTokens: 100, TotalTokens: 1000, HistoryTokens: 50,
		CompressAtTokens: 800, SummarizeAtTokens: 600, UsedPercent: 12,
	}
	outbound := newTestOutboundMessage("pico:send", "with-usage", nil)
	outbound.Usage = usage
	outbound.ContextUsage = contextUsage
	if _, err = ch.Send(context.Background(), outbound); err != nil {
		t.Fatalf("usage Send: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	usagePayload, _ := msg.Payload[PayloadKeyUsage].(map[string]any)
	if usagePayload == nil || usagePayload["prompt_tokens"] != float64(10) ||
		usagePayload["cached_tokens"] != float64(3) || usagePayload["reasoning_tokens"] != float64(2) {
		t.Fatalf("usage payload = %+v", msg.Payload[PayloadKeyUsage])
	}
	if msg.Payload[PayloadKeyContextUsage] == nil {
		t.Error("context_usage payload missing")
	}

	// Tool feedback: first send creates animated progress message and tracks it,
	// the final send edits the tracked message instead of creating a new one.
	if _, err = ch.Send(context.Background(), newTestOutboundMessage(
		"pico:send", "running tool", map[string]string{"message_kind": "tool_feedback"})); err != nil {
		t.Fatalf("tool feedback Send: %v", err)
	}
	feedbackMsg := readPicoMessage(t, conn, 2*time.Second)
	trackedID, tracked := ch.currentToolFeedbackMessage("pico:send")
	if !tracked {
		t.Fatalf("tool feedback message not tracked: %+v", feedbackMsg)
	}
	if _, err = ch.Send(context.Background(), newTestOutboundMessage("pico:send", "done", nil)); err != nil {
		t.Fatalf("final Send: %v", err)
	}
	finalMsg := readPicoMessage(t, conn, 2*time.Second)
	if finalMsg.Type != TypeMessageUpdate || finalMsg.Payload["message_id"] != trackedID ||
		finalMsg.Payload[PayloadKeyContent] != "done" {
		t.Fatalf("finalized tool feedback = %+v", finalMsg)
	}
	if _, tracked := ch.currentToolFeedbackMessage("pico:send"); tracked {
		t.Error("tool feedback tracking should be cleared after finalize")
	}
}

func TestEditDeleteTypingPlaceholder(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, ch)
	conn := mustDialPico(t, srv, "session_id=edit", authHeader())

	// EditMessage.
	if err := ch.EditMessage(context.Background(), "pico:edit", "msg-1", "edited"); err != nil {
		t.Fatalf("EditMessage: %v", err)
	}
	msg := readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageUpdate || msg.Payload[PayloadKeyContent] != "edited" ||
		msg.Payload["message_id"] != "msg-1" {
		t.Fatalf("edit message = %+v", msg)
	}

	// EditMessageWithPayload with nil payload adds empty content.
	if err := ch.EditMessageWithPayload(context.Background(), "pico:edit", "msg-2", nil); err != nil {
		t.Fatalf("EditMessageWithPayload: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageUpdate || msg.Payload[PayloadKeyContent] != "" {
		t.Fatalf("edit payload = %+v", msg)
	}

	// DeleteMessage.
	if err := ch.DeleteMessage(context.Background(), "pico:edit", "msg-3"); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageDelete || msg.Payload["message_id"] != "msg-3" {
		t.Fatalf("delete message = %+v", msg)
	}

	// StartTyping returns a stop function broadcasting typing.start/stop.
	stop, err := ch.StartTyping(context.Background(), "pico:edit")
	if err != nil {
		t.Fatalf("StartTyping: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeTypingStart {
		t.Fatalf("typing start = %+v", msg)
	}
	stop()
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeTypingStop {
		t.Fatalf("typing stop = %+v", msg)
	}

	// Errors for a session with no connections.
	if err := ch.EditMessage(context.Background(), "pico:none", "m", "x"); err == nil {
		t.Error("EditMessage without connections should fail")
	}
	if err := ch.DeleteMessage(context.Background(), "pico:none", "m"); err == nil {
		t.Error("DeleteMessage without connections should fail")
	}
	if _, err := ch.StartTyping(context.Background(), "pico:none"); err == nil {
		t.Error("StartTyping without connections should fail")
	}

	// Placeholder disabled -> empty id, nil error.
	if id, err := ch.SendPlaceholder(context.Background(), "pico:edit"); id != "" || err != nil {
		t.Fatalf("disabled placeholder = %q, %v", id, err)
	}

	// Placeholder enabled -> message.create with placeholder marker.
	ch.bc.Placeholder = config.PlaceholderConfig{Enabled: true}
	ch.bc.Placeholder.Text = config.FlexibleStringSlice{"thinking..."}
	id, err := ch.SendPlaceholder(context.Background(), "pico:edit")
	if err != nil || id == "" {
		t.Fatalf("enabled placeholder = %q, %v", id, err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Payload[PayloadKeyPlaceholder] != true || msg.Payload["message_id"] != id {
		t.Fatalf("placeholder message = %+v", msg)
	}
	if _, err := ch.SendPlaceholder(context.Background(), "pico:none"); err == nil {
		t.Error("placeholder without connections should fail")
	}

	// Dismiss tracked tool feedback deletes the progress message.
	ch.RecordToolFeedbackMessage("pico:edit", "progress-1", "running")
	if err := ch.DeleteMessage(context.Background(), "pico:edit", "unused"); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	_ = readPicoMessage(t, conn, 2*time.Second)
	ch.DismissToolFeedbackMessage(context.Background(), "pico:edit")
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageDelete || msg.Payload["message_id"] != "progress-1" {
		t.Fatalf("dismiss progress = %+v", msg)
	}
	// Unknown/empty ids are no-ops.
	ch.dismissTrackedToolFeedbackMessage(context.Background(), "", "x")
	ch.DismissToolFeedbackMessage(context.Background(), "pico:no-track")
}

// ---------------------------------------------------------------------------
// Streamer
// ---------------------------------------------------------------------------

func TestStreamer_LifecycleAndThrottling(t *testing.T) {
	// BeginStream error branches.
	ch, _ := newRunningChannel(t, nil)
	if _, err := ch.BeginStream(context.Background(), "pico:s"); err == nil {
		t.Error("BeginStream should fail when streaming disabled")
	}
	stopped := newTestPicoChannel(t)
	stopped.config.Streaming.Enabled = true
	if _, err := stopped.BeginStream(context.Background(), "pico:s"); err == nil {
		t.Error("BeginStream should fail when channel not running")
	}
	var nilStreamer *picoStreamer
	nilStreamer.SetModelName("x")
	nilStreamer.SetUsage(nil)
	nilStreamer.Cancel(context.Background())

	// Running + streaming enabled.
	streamCh, _ := newRunningChannel(t, func(cfg *config.PicoSettings) {
		cfg.Streaming.Enabled = true
		cfg.Streaming.ThrottleSeconds = 60
		cfg.Streaming.MinGrowthChars = 100
	})
	srv := newPicoHTTPServer(t, streamCh)
	conn := mustDialPico(t, srv, "session_id=stream", authHeader())

	streamer, err := streamCh.BeginStream(context.Background(), "pico:stream")
	if err != nil {
		t.Fatalf("BeginStream: %v", err)
	}
	s := streamer.(*picoStreamer)
	s.SetModelName("mock-model")
	s.SetUsage(&protocoltypes.UsageInfo{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3})

	if err := s.Update(context.Background(), "chunk one"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	msg := readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageCreate || msg.Payload[PayloadKeyContent] != "chunk one" ||
		msg.Payload[PayloadKeyModelName] != "mock-model" {
		t.Fatalf("stream create = %+v", msg)
	}
	if _, ok := msg.Payload[PayloadKeyUsage]; !ok {
		t.Error("stream usage payload missing")
	}

	// Immediate growth below throttle/minGrowth is skipped (no new frame).
	if err := s.Update(context.Background(), "chunk one +"); err != nil {
		t.Fatalf("throttled Update: %v", err)
	}
	// Empty content with no message id is a no-op.
	emptyStreamer := &picoStreamer{channel: streamCh, chatID: "pico:stream"}
	if err := emptyStreamer.updateLocked(context.Background(), "  ", false, nil); err != nil {
		t.Fatalf("empty update: %v", err)
	}

	// Force finalize sends message.update even under throttle.
	if err := s.FinalizeWithContext(context.Background(), "chunk one final",
		&bus.ContextUsage{UsedTokens: 5, TotalTokens: 10, UsedPercent: 50}); err != nil {
		t.Fatalf("FinalizeWithContext: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageUpdate || msg.Payload[PayloadKeyContent] != "chunk one final" {
		t.Fatalf("stream finalize = %+v", msg)
	}
	if msg.Payload[PayloadKeyContextUsage] == nil {
		t.Error("finalize context_usage missing")
	}

	// Reasoning stream create + update + finalize.
	if err := s.UpdateReasoning(context.Background(), "reasoning one"); err != nil {
		t.Fatalf("UpdateReasoning: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageCreate || msg.Payload[PayloadKeyKind] != MessageKindThought {
		t.Fatalf("reasoning create = %+v", msg)
	}
	if err := s.UpdateReasoning(context.Background(), "reasoning one more"); err != nil {
		t.Fatalf("reasoning update: %v", err)
	}
	if err := s.FinalizeReasoning(context.Background(), "reasoning final"); err != nil {
		t.Fatalf("FinalizeReasoning: %v", err)
	}
	msg = readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageUpdate || msg.Payload[PayloadKeyKind] != MessageKindThought {
		t.Fatalf("reasoning finalize = %+v", msg)
	}

	// Cancel deletes the streamed message(s).
	before := time.Now()
	s.Cancel(context.Background())
	if time.Since(before) > 5*time.Second {
		t.Fatal("Cancel took too long")
	}
}

// ---------------------------------------------------------------------------
// Media send / download
// ---------------------------------------------------------------------------

func storeTestMedia(t *testing.T, store *media.FileMediaStore, name, contentType, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write media: %v", err)
	}
	ref, err := store.Store(path, media.MediaMeta{Filename: name, ContentType: contentType}, "scope")
	if err != nil {
		t.Fatalf("store media: %v", err)
	}
	return ref
}

func TestSendMedia_SuccessAndErrorBranches(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)
	if _, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{ChatID: "pico:s"}); err == nil {
		t.Error("SendMedia without store should fail")
	}
	ch.SetMediaStore(media.NewFileMediaStore())
	if _, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "pico:s", Parts: []bus.MediaPart{{Ref: "media://missing"}},
	}); err == nil {
		t.Error("SendMedia with undeliverable refs should fail")
	}

	srv := newPicoHTTPServer(t, ch)
	conn := mustDialPico(t, srv, "session_id=media", authHeader())
	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)
	imageRef := storeTestMedia(t, store, "photo.png", "image/png", "png-bytes")
	fileRef := storeTestMedia(t, store, "doc.bin", "", "file-bytes")

	ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID:  "pico:media",
		Context: bus.InboundContext{Raw: map[string]string{PayloadKeyModelName: "mock-model"}},
		Parts: []bus.MediaPart{
			{Ref: imageRef, Caption: "a caption"},
			{Ref: fileRef, Filename: "renamed.bin", ContentType: "application/octet-stream"},
		},
	})
	if err != nil || len(ids) != 1 {
		t.Fatalf("SendMedia = %v, %v", ids, err)
	}
	msg := readPicoMessage(t, conn, 2*time.Second)
	if msg.Type != TypeMessageCreate || msg.Payload[PayloadKeyContent] != "a caption" {
		t.Fatalf("media message = %+v", msg)
	}
	attachments, _ := msg.Payload["attachments"].([]any)
	if len(attachments) != 2 {
		t.Fatalf("attachments = %+v", attachments)
	}
	first, _ := attachments[0].(map[string]any)
	if first["type"] != "image" || !strings.HasPrefix(fmt.Sprint(first["url"]), "/pico/media/") {
		t.Fatalf("image attachment = %+v", first)
	}
	if msg.Payload[PayloadKeyModelName] != "mock-model" {
		t.Errorf("media model_name = %v", msg.Payload[PayloadKeyModelName])
	}

	// Dismiss tracked tool feedback after media delivery.
	ch.RecordToolFeedbackMessage("pico:media", "progress-media", "running")
	if _, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "pico:media", Parts: []bus.MediaPart{{Ref: imageRef}},
	}); err != nil {
		t.Fatalf("tracked SendMedia: %v", err)
	}
	_ = readPicoMessage(t, conn, 2*time.Second)
	deleteMsg := readPicoMessage(t, conn, 2*time.Second)
	if deleteMsg.Type != TypeMessageDelete || deleteMsg.Payload["message_id"] != "progress-media" {
		t.Fatalf("media dismiss = %+v", deleteMsg)
	}

	// Not running -> ErrNotRunning.
	_ = ch.Stop(context.Background())
	if _, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{ChatID: "pico:media"}); err == nil {
		t.Error("SendMedia on stopped channel should fail")
	}
}

func TestMediaDownload_SuccessInlineAndAttachment(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)
	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)
	imageRef := storeTestMedia(t, store, "photo.png", "image/png", "png-bytes")
	svgRef := storeTestMedia(t, store, "vector.svg", "image/svg+xml", "<svg/>")
	srv := newPicoHTTPServer(t, ch)

	imageID := strings.TrimPrefix(imageRef, "media://")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/pico/media/"+imageID, nil)
	req.Header = authHeader()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download image: %v", err)
	}
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body[:n]) != "png-bytes" {
		t.Fatalf("image download = %d %q", resp.StatusCode, body[:n])
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Errorf("image content disposition = %q, want inline", cd)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("image content type = %q", ct)
	}

	svgID := strings.TrimPrefix(svgRef, "media://")
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/pico/media/"+svgID, nil)
	req.Header = authHeader()
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download svg: %v", err)
	}
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("svg content disposition = %q, want attachment", cd)
	}
}

// ---------------------------------------------------------------------------
// inline image parsing
// ---------------------------------------------------------------------------

func TestParseInlineImageMedia(t *testing.T) {
	validURL := testDataURL("image/png", []byte("png"))
	valid := []byte("png")

	tests := []struct {
		name    string
		payload map[string]any
		want    int
		wantErr bool
	}{
		{"empty payload", nil, 0, false},
		{"string media", map[string]any{"media": validURL}, 1, false},
		{"string slice media", map[string]any{"media": []string{validURL}}, 1, false},
		{"any slice media", map[string]any{"media": []any{validURL, map[string]any{"url": validURL}}}, 2, false},
		{"object data_url", map[string]any{"media": map[string]any{"data_url": validURL}}, 0, true},
		{"invalid media type", map[string]any{"media": 42}, 0, true},
		{"invalid object", map[string]any{"media": map[string]any{}}, 0, true},
		{"non image data url", map[string]any{"media": "data:text/plain;base64,cG5n"}, 0, true},
		{"not base64", map[string]any{"media": "data:image/png,abc"}, 0, true},
		{"bad base64", map[string]any{"media": "data:image/png;base64,!!!!"}, 0, true},
		{"empty data", map[string]any{"media": "data:image/png;base64,"}, 0, true},
		{"attachments not array", map[string]any{"attachments": "x"}, 0, true},
		{"attachments element not object", map[string]any{"attachments": []any{"x"}}, 0, true},
		{"attachment non-image skipped", map[string]any{"attachments": []any{
			map[string]any{"type": "file", "url": "https://example.com/x"}}}, 0, false},
		{"attachment image valid", map[string]any{"attachments": []any{
			map[string]any{"type": "image", "url": validURL}}}, 1, false},
		{"attachment image missing url", map[string]any{"attachments": []any{
			map[string]any{"type": "image"}}}, 0, true},
		{"attachment http url skipped", map[string]any{"attachments": []any{
			map[string]any{"url": "https://example.com/x.png"}}}, 0, false},
		{"media and attachments", map[string]any{
			"media": validURL, "attachments": []any{map[string]any{"url": validURL}}}, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInlineImageMedia(tt.payload)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(got) != tt.want {
				t.Fatalf("len = %d, want %d", len(got), tt.want)
			}
		})
	}
	_ = valid
}

func TestValidateInlineImageDataURL(t *testing.T) {
	valid := testDataURL("image/png", []byte("png"))
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"valid png", valid, false},
		{"valid jpeg", testDataURL("image/jpeg", []byte("jpg")), false},
		{"empty", "", true},
		{"http url", "https://example.com/a.png", true},
		{"missing comma", "data:image/png;base64", true},
		{"no base64 marker", "data:image/png,abc", true},
		{"unsupported mime", "data:image/tiff;base64," + base64.StdEncoding.EncodeToString([]byte("x")), true},
		{"invalid base64", "data:image/png;base64,!!!", true},
		{"oversized", "data:image/png;base64," + base64.StdEncoding.EncodeToString(
			make([]byte, config.DefaultMaxMediaSize+1)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateInlineImageDataURL(tc.value); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestInlineImageValueAndAttachments(t *testing.T) {
	if v, err := inlineImageValue(" data:image/png;base64,cG5n "); err != nil || !strings.HasPrefix(v, "data:") {
		t.Fatalf("string value = %q, %v", v, err)
	}
	if _, err := inlineImageValue("  "); err == nil {
		t.Error("empty string should error")
	}
	if v, err := inlineImageValue(map[string]any{"url": "data:image/png;base64,cG5n"}); err != nil || v == "" {
		t.Fatalf("url value = %q, %v", v, err)
	}
	if v, err := inlineImageValue(map[string]any{"data_url": "data:image/png;base64,cG5n"}); err != nil || v == "" {
		t.Fatalf("data_url value = %q, %v", v, err)
	}
	if _, err := inlineImageValue(map[string]any{}); err == nil {
		t.Error("object without url should error")
	}
	if _, err := inlineImageValue(42); err == nil {
		t.Error("unsupported type should error")
	}

	valid := testDataURL("image/png", []byte("png"))
	media, err := parseInlineImageAttachments([]any{
		map[string]any{"type": "image", "url": valid},
		map[string]any{"type": "file", "url": "https://example.com/x"},
		map[string]any{"url": "https://example.com/y.png"},
	})
	if err != nil || len(media) != 1 {
		t.Fatalf("attachments = %v, %v", media, err)
	}
}

// ---------------------------------------------------------------------------
// payload helpers / attachments inference / connection bookkeeping
// ---------------------------------------------------------------------------

func TestPayloadHelpers(t *testing.T) {
	payload := map[string]any{}
	setUsagePayload(payload, nil)
	if _, ok := payload[PayloadKeyUsage]; ok {
		t.Error("nil usage should not add payload")
	}
	setUsagePayload(payload, &protocoltypes.UsageInfo{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3})
	usage, _ := payload[PayloadKeyUsage].(map[string]any)
	if usage["total_tokens"] != 3 {
		t.Errorf("usage payload = %+v", usage)
	}
	if _, ok := usage["cached_tokens"]; ok {
		t.Error("zero cached tokens should be omitted")
	}

	setContextUsagePayload(payload, nil)
	if _, ok := payload[PayloadKeyContextUsage]; ok {
		t.Error("nil context usage should not add payload")
	}
	setContextUsagePayload(payload, &bus.ContextUsage{UsedTokens: 1})

	if calls, ok := picoToolCallsPayload(newTestOutboundMessage("c", "", nil)); ok || calls != nil {
		t.Errorf("empty tool calls = %v, %v", calls, ok)
	}
	if calls, ok := picoToolCallsPayload(newTestOutboundMessage("c", "", map[string]string{
		PayloadKeyToolCalls: "[]"})); ok || calls != nil {
		t.Errorf("empty array tool calls = %v, %v", calls, ok)
	}
	if _, ok := picoToolCallsPayload(newTestOutboundMessage("c", "", map[string]string{
		PayloadKeyToolCalls: "{bad"})); ok {
		t.Error("invalid tool calls should not parse")
	}

	// Message kind helpers.
	if outboundMessageIsThought(bus.OutboundMessage{}) ||
		outboundMessageIsToolFeedback(bus.OutboundMessage{}) ||
		outboundMessageIsToolCalls(bus.OutboundMessage{}) {
		t.Error("empty context should not match any kind")
	}
	if !outboundMessageIsThought(newTestOutboundMessage("c", "", map[string]string{"message_kind": " THOUGHT "})) {
		t.Error("thought detection should trim/case-fold")
	}
	if !outboundMessageIsToolFeedback(newTestOutboundMessage("c", "", map[string]string{"message_kind": "Tool_Feedback"})) {
		t.Error("tool feedback detection should trim/case-fold")
	}
	if outboundMessageFinalizesTrackedToolFeedback(newTestOutboundMessage("c", "", map[string]string{"message_kind": MessageKindThought})) {
		t.Error("thought message must not finalize tool feedback")
	}

	// truncate.
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("truncate long = %q", got)
	}
}

func TestAttachmentInferenceAndMediaRefs(t *testing.T) {
	cases := []struct{ filename, contentType, want string }{
		{"a.png", "", "image"},
		{"a.JPG", "application/octet-stream", "image"},
		{"a.mp3", "", "audio"},
		{"a.mp4", "", "video"},
		{"a.pdf", "", "file"},
		{"a.bin", "image/webp", "image"},
		{"a.bin", "audio/ogg", "audio"},
		{"a.bin", "video/mp4", "video"},
		{"a", "", "file"},
	}
	for _, tc := range cases {
		if got := picoInferAttachmentType(tc.filename, tc.contentType); got != tc.want {
			t.Errorf("infer(%q,%q) = %q, want %q", tc.filename, tc.contentType, got, tc.want)
		}
	}
	if picoAllowsInlineDisplay("vector.svg", "image/svg+xml") {
		t.Error("svg must not render inline")
	}
	if picoAllowsInlineDisplay("a.png", "image/png") != true {
		t.Error("png should render inline")
	}
	if picoAllowsInlineDisplay("a.bin", "application/octet-stream") {
		t.Error("binary must not render inline")
	}

	if _, err := picoMediaRefID(""); err == nil {
		t.Error("empty ref should error")
	}
	if _, err := picoMediaRefID("media://a/b"); err == nil {
		t.Error("ref with slash should error")
	}
	if id, err := picoMediaRefID("media://abc"); err != nil || id != "abc" {
		t.Errorf("media ref id = %q, %v", id, err)
	}
	if url, err := picoDownloadURLForRef("media://abc"); err != nil || url != "/pico/media/abc" {
		t.Errorf("download url = %q, %v", url, err)
	}
	if _, err := picoDownloadURLForRef("media://a/b"); err == nil {
		t.Error("download url for invalid ref should error")
	}
}

func TestConnectionBookkeeping(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)

	// Limit enforcement and unknown-id removal.
	if _, err := ch.createAndAddConnection(nil, "sess", 0); err == nil {
		t.Error("createAndAddConnection with maxConns=0 should fail")
	}
	pc, err := ch.createAndAddConnection(nil, "sess", 5)
	if err != nil {
		t.Fatalf("createAndAddConnection: %v", err)
	}
	if ch.removeConnection("unknown") != nil {
		t.Error("removeConnection(unknown) should return nil")
	}
	if got := ch.sessionConnectionsSnapshot("sess"); len(got) != 1 {
		t.Fatalf("session snapshot = %v", got)
	}
	if got := ch.sessionConnectionsSnapshot("other"); got != nil {
		t.Errorf("unknown session snapshot = %v", got)
	}
	if removed := ch.removeConnection(pc.id); removed != pc {
		t.Error("removeConnection should return the removed connection")
	}
	if got := ch.sessionConnectionsSnapshot("sess"); got != nil {
		t.Errorf("session snapshot after removal = %v", got)
	}

	// takeAllConnections clears indexes.
	_, _ = ch.createAndAddConnection(nil, "s1", 10)
	_, _ = ch.createAndAddConnection(nil, "s2", 10)
	if got := ch.currentConnCount(); got != 2 {
		t.Fatalf("currentConnCount = %d, want 2", got)
	}
	all := ch.takeAllConnections()
	if len(all) != 2 {
		t.Fatalf("takeAllConnections = %d, want 2", len(all))
	}
	if ch.currentConnCount() != 0 {
		t.Error("connections should be cleared")
	}
}

// ---------------------------------------------------------------------------
// factory init paths
// ---------------------------------------------------------------------------

func TestInitFactories_CreateChannels(t *testing.T) {
	rawPico := `{"enabled":true,"type":"pico","settings":{"token":"factory-token"}}`
	var picoChannel config.Channel
	if err := json.Unmarshal([]byte(rawPico), &picoChannel); err != nil {
		t.Fatalf("decode pico channel: %v", err)
	}
	rawClient := `{"enabled":true,"type":"pico_client","settings":{"url":"ws://127.0.0.1:1/pico/ws"}}`
	var clientChannel config.Channel
	if err := json.Unmarshal([]byte(rawClient), &clientChannel); err != nil {
		t.Fatalf("decode pico_client channel: %v", err)
	}

	cfg := &config.Config{
		Version: 2,
		Channels: config.ChannelsConfig{
			"pico":        &picoChannel,
			"pico_client": &clientChannel,
		},
		Gateway: config.GatewayConfig{Host: "127.0.0.1", Port: 0},
	}
	manager, err := channels.NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer func() { _ = manager.StopAll(context.Background()) }()

	found := 0
	for _, name := range manager.GetEnabledChannels() {
		if name == "pico" || name == "pico_client" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("factory-created channels = %v, want pico + pico_client", manager.GetEnabledChannels())
	}
}
