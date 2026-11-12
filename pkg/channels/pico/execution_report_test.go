package pico

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/channels"
)

func TestPicoChannelSendExecutionReportWritesFrame(t *testing.T) {
	ch := newTestPicoChannel(t)
	ch.SetRunning(true)

	conn, received, cleanup := newTestPicoWebSocket(t)
	defer cleanup()
	ch.addConnForTest(&picoConn{id: "conn-report", sessionID: "session-1", conn: conn})

	payload := map[string]any{
		"event_id":       "exec-abc",
		"tool_name":      "skill",
		"execution_type": "skill",
		"skill_name":     "code-review",
		"status":         "success",
		"duration_ms":    int64(12),
	}
	if err := ch.SendExecutionReport(context.Background(), "pico:session-1", payload); err != nil {
		t.Fatalf("SendExecutionReport() error = %v", err)
	}

	select {
	case msg := <-received:
		if msg.Type != TypeExecutionReport {
			t.Fatalf("msg.Type = %q, want %q", msg.Type, TypeExecutionReport)
		}
		if msg.ID != "exec-abc" {
			t.Fatalf("msg.ID = %q, want exec-abc", msg.ID)
		}
		if msg.SessionID != "session-1" {
			t.Fatalf("msg.SessionID = %q, want session-1", msg.SessionID)
		}
		if got := msg.Payload["message_id"]; got != "exec-abc" {
			t.Fatalf("payload.message_id = %v, want exec-abc", got)
		}
		if got := msg.Payload[PayloadKeyKind]; got != MessageKindExecutionReport {
			t.Fatalf("payload.kind = %v, want %q", got, MessageKindExecutionReport)
		}
		if got := msg.Payload["tool_name"]; got != "skill" {
			t.Fatalf("payload.tool_name = %v, want skill", got)
		}
		if got := msg.Payload["skill_name"]; got != "code-review" {
			t.Fatalf("payload.skill_name = %v, want code-review", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution.report frame")
	}
}

func TestPicoChannelSendExecutionReportWithoutConnection(t *testing.T) {
	ch := newTestPicoChannel(t)
	ch.SetRunning(true)

	started := time.Now()
	err := ch.SendExecutionReport(context.Background(), "pico:missing", map[string]any{"event_id": "exec-1"})
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("SendExecutionReport() blocked for %s without a connection", elapsed)
	}
	if err == nil {
		t.Fatal("expected error when no connection is available")
	}
	if !errors.Is(err, channels.ErrSendFailed) {
		t.Fatalf("error = %v, want ErrSendFailed", err)
	}
}

func TestPicoChannelSendExecutionReportNotRunning(t *testing.T) {
	ch := newTestPicoChannel(t)
	err := ch.SendExecutionReport(context.Background(), "pico:session-1", map[string]any{"event_id": "exec-1"})
	if !errors.Is(err, channels.ErrNotRunning) {
		t.Fatalf("error = %v, want ErrNotRunning", err)
	}
}

func TestPicoChannelSendExecutionReportEmptyChatID(t *testing.T) {
	ch := newTestPicoChannel(t)
	ch.SetRunning(true)
	err := ch.SendExecutionReport(context.Background(), "  ", map[string]any{"event_id": "exec-1"})
	if !errors.Is(err, channels.ErrSendFailed) {
		t.Fatalf("error = %v, want ErrSendFailed", err)
	}
}

func TestPicoClientChannelSendExecutionReportWritesFrame(t *testing.T) {
	conn, received, cleanup := newTestPicoWebSocket(t)
	defer cleanup()

	client := &PicoClientChannel{
		BaseChannel: channels.NewBaseChannel("pico_client", nil, bus.NewMessageBus(), nil),
		conn:        &picoConn{id: "client-conn", conn: conn},
	}
	client.SetRunning(true)

	if err := client.SendExecutionReport(context.Background(), "pico_client:session-9", map[string]any{
		"event_id":  "exec-client",
		"tool_name": "mcp_chess_agent_channel_list",
	}); err != nil {
		t.Fatalf("SendExecutionReport() error = %v", err)
	}

	select {
	case msg := <-received:
		if msg.Type != TypeExecutionReport {
			t.Fatalf("msg.Type = %q, want %q", msg.Type, TypeExecutionReport)
		}
		if msg.SessionID != "session-9" {
			t.Fatalf("msg.SessionID = %q, want session-9", msg.SessionID)
		}
		if msg.ID != "exec-client" {
			t.Fatalf("msg.ID = %q, want exec-client", msg.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution.report frame")
	}
}

func TestPicoClientChannelSendExecutionReportWithoutConnection(t *testing.T) {
	client := &PicoClientChannel{
		BaseChannel: channels.NewBaseChannel("pico_client", nil, bus.NewMessageBus(), nil),
	}
	client.SetRunning(true)
	err := client.SendExecutionReport(context.Background(), "pico_client:session-9", map[string]any{"event_id": "exec-1"})
	if !errors.Is(err, channels.ErrSendFailed) {
		t.Fatalf("error = %v, want ErrSendFailed", err)
	}
}
