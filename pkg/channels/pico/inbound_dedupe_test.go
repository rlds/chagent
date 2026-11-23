package pico

import (
	"testing"
	"time"
)

// TestInboundMessageIDIsIdempotentWithAck 是 Client 断线重投的服务端契约：
// 同一 session + message_id 只投递一次；重复消息只回 message.ack，不进入 Agent 执行队列。
func TestInboundMessageIDIsIdempotentWithAck(t *testing.T) {
	ch, msgBus := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, ch)
	conn := mustDialPico(t, srv, "session_id=dedupe", authHeader())

	payload := map[string]any{
		PayloadKeyContent: "执行任务",
		"message_id":      "msg-dedupe-1",
		"ack":             true,
	}
	sendJSON(t, conn, PicoMessage{Type: TypeMessageSend, ID: "req-1", SessionID: "dedupe", Payload: payload})

	// 第一次：入队 + ack。
	select {
	case inbound := <-msgBus.InboundChan():
		if inbound.Content != "执行任务" {
			t.Fatalf("inbound content = %q", inbound.Content)
		}
		if inbound.Context.MessageID != "msg-dedupe-1" {
			t.Fatalf("inbound MessageID = %q, want msg-dedupe-1", inbound.Context.MessageID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected first inbound message")
	}
	ack := readPicoMessage(t, conn, 2*time.Second)
	if ack.Type != TypeMessageAck {
		t.Fatalf("first ack type = %q, want %q", ack.Type, TypeMessageAck)
	}
	if ack.Payload["message_id"] != "msg-dedupe-1" {
		t.Fatalf("ack message_id = %v", ack.Payload["message_id"])
	}

	// 第二次（同一 message_id，模拟 1006 重投）：只回 ack，不再入队。
	sendJSON(t, conn, PicoMessage{Type: TypeMessageSend, ID: "req-2", SessionID: "dedupe", Payload: payload})
	ack2 := readPicoMessage(t, conn, 2*time.Second)
	if ack2.Type != TypeMessageAck || ack2.Payload["message_id"] != "msg-dedupe-1" {
		t.Fatalf("duplicate ack = %+v", ack2)
	}
	select {
	case extra := <-msgBus.InboundChan():
		t.Fatalf("duplicate message must not be re-executed: %+v", extra)
	case <-time.After(300 * time.Millisecond):
	}

	// 不同 message_id 仍正常入队。
	payload2 := map[string]any{PayloadKeyContent: "第二个任务", "message_id": "msg-dedupe-2", "ack": true}
	sendJSON(t, conn, PicoMessage{Type: TypeMessageSend, ID: "req-3", SessionID: "dedupe", Payload: payload2})
	select {
	case inbound := <-msgBus.InboundChan():
		if inbound.Content != "第二个任务" {
			t.Fatalf("second inbound content = %q", inbound.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected second inbound message")
	}
	if ack3 := readPicoMessage(t, conn, 2*time.Second); ack3.Type != TypeMessageAck {
		t.Fatalf("third ack = %+v", ack3)
	}
}

// TestInboundWithoutMessageIDStillDelivered 兼容旧 Client：没有 message_id/ack 时
// 不做去重、不回 ack，保持原有行为。
func TestInboundWithoutMessageIDStillDelivered(t *testing.T) {
	ch, msgBus := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, ch)
	conn := mustDialPico(t, srv, "session_id=legacy", authHeader())

	sendJSON(t, conn, PicoMessage{Type: TypeMessageSend, ID: "legacy-1", SessionID: "legacy",
		Payload: map[string]any{PayloadKeyContent: "legacy hello"}})
	select {
	case inbound := <-msgBus.InboundChan():
		if inbound.Content != "legacy hello" {
			t.Fatalf("legacy inbound = %+v", inbound)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected legacy inbound message")
	}
	// 相同 ID 再来一次（旧 Client 无 message_id）：仍应入队，不去重。
	sendJSON(t, conn, PicoMessage{Type: TypeMessageSend, ID: "legacy-1", SessionID: "legacy",
		Payload: map[string]any{PayloadKeyContent: "legacy hello"}})
	select {
	case <-msgBus.InboundChan():
	case <-time.After(2 * time.Second):
		t.Fatal("legacy message without message_id must still be delivered")
	}
}

// TestAcceptInboundMessageExpiry 校验幂等表按 30 分钟过期，避免无界增长。
func TestAcceptInboundMessageExpiry(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)
	if !ch.acceptInboundMessage("s", "m") {
		t.Fatal("first should be accepted")
	}
	if ch.acceptInboundMessage("s", "m") {
		t.Fatal("duplicate should be rejected")
	}
	ch.inboundSeenMu.Lock()
	ch.inboundSeen["s|m"] = time.Now().Add(-31 * time.Minute)
	ch.inboundSeenMu.Unlock()
	if !ch.acceptInboundMessage("s", "m") {
		t.Fatal("expired entry should be accepted again")
	}
}
