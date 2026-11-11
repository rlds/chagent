package pico

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/channels"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/media"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// TestWebhookPath verifies the shared-mux mount path.
func TestWebhookPath(t *testing.T) {
	ch := newTestPicoChannel(t)
	if got := ch.WebhookPath(); got != "/pico/" {
		t.Errorf("WebhookPath = %q, want /pico/", got)
	}
}

// TestNewPicoChannel_RequiresToken covers the constructor error branch.
func TestNewPicoChannel_RequiresToken(t *testing.T) {
	bc := &config.Channel{Type: config.ChannelPico, Enabled: true}
	cfg := &config.PicoSettings{}
	if _, err := NewPicoChannel(bc, cfg, bus.NewMessageBus()); err == nil {
		t.Error("NewPicoChannel without token should fail")
	}
}

// TestProgressNilBranches covers the defensive nil-progress branches.
func TestProgressNilBranches(t *testing.T) {
	ch := &PicoChannel{}
	if _, ok := ch.currentToolFeedbackMessage("chat"); ok {
		t.Error("nil progress current should be false")
	}
	if _, _, ok := ch.takeToolFeedbackMessage("chat"); ok {
		t.Error("nil progress take should be false")
	}
	ch.RecordToolFeedbackMessage("chat", "id", "content")
	ch.ClearToolFeedbackMessage("chat")
	ch.DismissToolFeedbackMessage(context.Background(), "chat")
}

// TestToolFeedbackFinalizeBranches covers direct finalize edge cases.
func TestToolFeedbackFinalizeBranches(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)

	// No tracked message -> not handled.
	if ids, handled := ch.finalizeTrackedToolFeedbackMessage(
		context.Background(), "pico:none", "x", ch.editMessagePayload, nil, nil,
	); handled || ids != nil {
		t.Errorf("untracked finalize = %v, %v", ids, handled)
	}

	// Nil edit function -> not handled.
	ch.RecordToolFeedbackMessage("pico:noedit", "msg-1", "running")
	if ids, handled := ch.finalizeTrackedToolFeedbackMessage(
		context.Background(), "pico:noedit", "x", nil, nil, nil,
	); handled || ids != nil {
		t.Errorf("nil edit finalize = %v, %v", ids, handled)
	}
	// The tracked entry is consumed before the nil edit check (defensive branch).
	if _, tracked := ch.currentToolFeedbackMessage("pico:noedit"); tracked {
		t.Error("nil edit finalize should consume the tracked message")
	}

	// Edit failure re-records the tracked message so a retry is possible.
	ch.RecordToolFeedbackMessage("pico:failedit", "msg-2", "running")
	failingEdit := func(context.Context, string, string, map[string]any, *bus.ContextUsage) error {
		return errors.New("edit failed")
	}
	if ids, handled := ch.finalizeTrackedToolFeedbackMessage(
		context.Background(), "pico:failedit", "x", failingEdit, nil, nil,
	); handled || ids != nil {
		t.Errorf("failed edit finalize = %v, %v", ids, handled)
	}
	if _, tracked := ch.currentToolFeedbackMessage("pico:failedit"); !tracked {
		t.Error("failed edit should re-record tracked message")
	}

	// An empty payload map gets the content filled in.
	ch.RecordToolFeedbackMessage("pico:emptypayload", "msg-3", "running")
	var captured map[string]any
	edit := func(_ context.Context, _ string, _ string, payload map[string]any, _ *bus.ContextUsage) error {
		captured = payload
		return nil
	}
	if _, handled := ch.finalizeTrackedToolFeedbackMessage(
		context.Background(), "pico:emptypayload", "final", edit, map[string]any{}, nil,
	); !handled {
		t.Fatal("expected handled finalize")
	}
	if captured[PayloadKeyContent] != "final" {
		t.Errorf("payload content = %v", captured[PayloadKeyContent])
	}

	// FinalizeToolFeedbackMessage early return for a thought message.
	if ids, handled := ch.FinalizeToolFeedbackMessage(context.Background(),
		newTestOutboundMessage("pico:send", "x", map[string]string{"message_kind": MessageKindThought})); handled || ids != nil {
		t.Errorf("thought finalize = %v, %v", ids, handled)
	}
}

// TestStreamerCancelBranches covers Cancel's reasoning-only and nil-channel paths.
func TestStreamerCancelBranches(t *testing.T) {
	// nil channel -> no panic, no-op.
	if s := (&picoStreamer{chatID: "pico:x"}); s != nil {
		s.Cancel(context.Background())
	}

	streamCh, _ := newRunningChannel(t, func(cfg *config.PicoSettings) {
		cfg.Streaming.Enabled = true
	})
	srv := newPicoHTTPServer(t, streamCh)
	conn := mustDialPico(t, srv, "session_id=cancel", authHeader())
	streamer, err := streamCh.BeginStream(context.Background(), "pico:cancel")
	if err != nil {
		t.Fatalf("BeginStream: %v", err)
	}
	s := streamer.(*picoStreamer)
	// Reasoning only (no message id) -> only reasoning delete.
	s.reasoningID = "reasoning-1"
	s.Cancel(context.Background())
	if msg := readPicoMessage(t, conn, 2*time.Second); msg.Type != TypeMessageDelete ||
		msg.Payload["message_id"] != "reasoning-1" {
		t.Fatalf("reasoning-only cancel = %+v", msg)
	}
	if s.reasoningID != "" {
		t.Error("reasoning id should be cleared")
	}

	// Message + reasoning -> both deleted.
	s.messageID = "message-1"
	s.reasoningID = "reasoning-2"
	s.Cancel(context.Background())
	if msg := readPicoMessage(t, conn, 2*time.Second); msg.Payload["message_id"] != "message-1" {
		t.Fatalf("cancel message delete = %+v", msg)
	}
	if msg := readPicoMessage(t, conn, 2*time.Second); msg.Payload["message_id"] != "reasoning-2" {
		t.Fatalf("cancel reasoning delete = %+v", msg)
	}
	if s.messageID != "" || s.reasoningID != "" {
		t.Errorf("stream ids should be cleared: %q %q", s.messageID, s.reasoningID)
	}
}

// TestHandleMessageSend_SenderNotAllowed ensures the allowlist blocks inbound.
func TestHandleMessageSend_SenderNotAllowed(t *testing.T) {
	msgBus := bus.NewMessageBus()
	bc := &config.Channel{Type: config.ChannelPico, Enabled: true,
		AllowFrom: config.FlexibleStringSlice{"someone-else"}}
	cfg := &config.PicoSettings{}
	cfg.SetToken("test-token")
	ch, err := NewPicoChannel(bc, cfg, msgBus)
	if err != nil {
		t.Fatalf("NewPicoChannel: %v", err)
	}
	ch.ctx = context.Background()
	ch.handleMessageSend(&picoConn{id: "c1", sessionID: "s1"}, PicoMessage{
		Type: TypeMessageSend, ID: "m1", Payload: map[string]any{PayloadKeyContent: "hi"}})
	select {
	case inbound := <-msgBus.InboundChan():
		t.Fatalf("allowlist should block inbound, got %+v", inbound)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHandleWebSocket_UpgradeFailure covers the upgrader error branch: a valid
// token but no WebSocket handshake headers.
func TestHandleWebSocket_UpgradeFailure(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, ch)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/pico/ws", nil)
	req.Header = authHeader()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET ws without upgrade: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("upgrade failure status = %d, want 400", resp.StatusCode)
	}
}

// TestMediaDownload_OpenFailure covers the os.Open failure branch.
func TestMediaDownload_OpenFailure(t *testing.T) {
	ch, _ := newRunningChannel(t, nil)
	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)
	dir := t.TempDir()
	path := dir + "/gone.png"
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ref, err := store.Store(path, media.MediaMeta{Filename: "gone.png", ContentType: "image/png"}, "scope")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	srv := newPicoHTTPServer(t, ch)
	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/pico/media/"+strings.TrimPrefix(ref, "media://"), nil)
	req.Header = authHeader()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET deleted media: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("deleted media status = %d, want 500", resp.StatusCode)
	}
}

// TestParseInlineImageValues_StringSliceInvalid covers the []string error branch.
func TestParseInlineImageValues_StringSliceInvalid(t *testing.T) {
	if _, err := parseInlineImageValues([]string{"https://example.com/a.png"}); err == nil {
		t.Error("[]string with non-data URL should error")
	}
	if _, err := parseInlineImageValues([]string{testDataURL("image/png", []byte("png"))}); err != nil {
		t.Errorf("[]string with valid data URL should pass: %v", err)
	}
}

// TestInitFactories_ErrorsAndSetName covers factory error + custom-name branches.
func TestInitFactories_ErrorsAndSetName(t *testing.T) {
	// Custom channel name (not "pico") hits SetName.
	rawPico := `{"enabled":true,"type":"pico","settings":{"token":"factory-token"}}`
	var picoChannel config.Channel
	if err := json.Unmarshal([]byte(rawPico), &picoChannel); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rawClient := `{"enabled":true,"type":"pico_client","settings":{"url":"ws://127.0.0.1:1/pico/ws"}}`
	var clientChannel config.Channel
	if err := json.Unmarshal([]byte(rawClient), &clientChannel); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cfg := &config.Config{
		Version: 2,
		Channels: config.ChannelsConfig{
			"pico_custom":        &picoChannel,
			"pico_client_custom": &clientChannel,
		},
	}
	manager, err := channels.NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer func() { _ = manager.StopAll(context.Background()) }()
	names := manager.GetEnabledChannels()
	if len(names) != 2 {
		t.Errorf("enabled channels = %v", names)
	}

	// Invalid settings JSON hits the GetDecoded error branch during NewManager.
	var brokenChannel config.Channel
	if err := json.Unmarshal([]byte(`{"enabled":true,"type":"pico","settings":{"token":123}}`),
		&brokenChannel); err != nil {
		t.Fatalf("decode broken channel: %v", err)
	}
	brokenCfg := &config.Config{Version: 2, Channels: config.ChannelsConfig{"pico": &brokenChannel}}
	brokenManager, err := channels.NewManager(brokenCfg, bus.NewMessageBus(), nil)
	if err != nil {
		// Some managers may propagate factory errors; either shape is acceptable.
		return
	}
	defer func() { _ = brokenManager.StopAll(context.Background()) }()
	if names := brokenManager.GetEnabledChannels(); len(names) != 0 {
		t.Errorf("invalid pico settings should not create a channel, got %v", names)
	}
}

// ---------------------------------------------------------------------------
// pico_client channel tests (coverage for client.go pingLoop/reconnectLoop)
// ---------------------------------------------------------------------------

func newPicoClientChannel(t *testing.T, url string, mutate func(*config.PicoClientSettings)) *PicoClientChannel {
	t.Helper()
	bc := &config.Channel{Type: config.ChannelPicoClient, Enabled: true}
	cfg := &config.PicoClientSettings{URL: url}
	if mutate != nil {
		mutate(cfg)
	}
	client, err := NewPicoClientChannel(bc, cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewPicoClientChannel: %v", err)
	}
	return client
}

func TestPicoClient_StartSendReceiveAndStop(t *testing.T) {
	server, serverBus := newRunningChannel(t, func(cfg *config.PicoSettings) {
		cfg.PingInterval = 1
	})
	srv := newPicoHTTPServer(t, server)
	wsURL := picoWSURL(srv, "session_id=client-session")

	clientBus := bus.NewMessageBus()
	bc := &config.Channel{Type: config.ChannelPicoClient, Enabled: true}
	clientCfg := &config.PicoClientSettings{URL: wsURL, SessionID: "client-session",
		PingInterval: 1, ReadTimeout: 5}
	clientCfg.Token = *config.NewSecureString("test-token")
	client, err := NewPicoClientChannel(bc, clientCfg, clientBus)
	if err != nil {
		t.Fatalf("NewPicoClientChannel: %v", err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background()) })

	waitFor(t, 5*time.Second, "server-side client connection", func() bool {
		return server.currentConnCount() == 1
	})

	// Client -> server message.
	if _, err := client.Send(context.Background(), newTestOutboundMessage("pico_client:client-session", "from-client", nil)); err != nil {
		t.Fatalf("client Send: %v", err)
	}
	select {
	case inbound := <-serverBus.InboundChan():
		if inbound.Content != "from-client" {
			t.Fatalf("server inbound = %+v", inbound)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive client message")
	}

	// Server -> client message.create becomes inbound for the client.
	sessionConns := server.sessionConnectionsSnapshot("client-session")
	if len(sessionConns) != 1 {
		t.Fatalf("server session connections = %d", len(sessionConns))
	}
	if err := server.broadcastToSession("pico:client-session", newMessage(TypeMessageCreate,
		map[string]any{PayloadKeyContent: "from-server", "message_id": "srv-1"})); err != nil {
		t.Fatalf("server broadcast: %v", err)
	}
	select {
	case inbound := <-clientBus.InboundChan():
		if inbound.Content != "from-server" {
			t.Fatalf("client inbound = %+v", inbound)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not receive server message")
	}

	// Typing start/stop reaches the server connection.
	stopTyping, err := client.StartTyping(context.Background(), "pico_client:client-session")
	if err != nil {
		t.Fatalf("client StartTyping: %v", err)
	}
	stopTyping()

	// Stop disconnects and further sends fail.
	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("client Stop: %v", err)
	}
	if _, err := client.Send(context.Background(), newTestOutboundMessage("pico_client:s", "x", nil)); err == nil {
		t.Error("client Send after stop should fail")
	}
}

func TestPicoClient_DialErrorAndOfflineBranches(t *testing.T) {
	// Dial failure at Start.
	client := newPicoClientChannel(t, "ws://127.0.0.1:1/pico/ws", nil)
	if err := client.Start(context.Background()); err == nil {
		t.Error("client Start with unreachable URL should fail")
	}

	// Send / StartTyping with no connection.
	offline := newPicoClientChannel(t, "ws://127.0.0.1:1/pico/ws", nil)
	if _, err := offline.Send(context.Background(), newTestOutboundMessage("pico_client:s", "x", nil)); err == nil {
		t.Error("offline client Send should fail")
	}
	stop, err := offline.StartTyping(context.Background(), "pico_client:s")
	if err != nil {
		t.Errorf("offline StartTyping error = %v", err)
	}
	stop()

	// handleInbound branches without a network connection.
	pc := &picoConn{id: "pc", sessionID: "s1"}
	offline.handleInbound(pc, PicoMessage{Type: TypePong})
	offline.handleInbound(pc, PicoMessage{Type: "unknown"})
	offline.handleInbound(pc, PicoMessage{Type: TypeMessageCreate, Payload: map[string]any{
		PayloadKeyContent: "", "message_id": "empty"}})
	offline.handleInbound(pc, PicoMessage{Type: TypeMessageCreate, Payload: map[string]any{
		PayloadKeyContent: "thought", PayloadKeyKind: MessageKindThought}})
	offline.handleInbound(pc, PicoMessage{Type: TypeMessageCreate, Payload: map[string]any{
		PayloadKeyContent: "bad media", "media": 42}})
	offline.handleInbound(pc, PicoMessage{Type: TypeMediaCreate, Payload: map[string]any{
		PayloadKeyContent: "media message"}})
	// Media-only message (invalid media with empty content) is dropped.
	offline.handleInbound(pc, PicoMessage{Type: TypeMessageCreate, Payload: map[string]any{"media": 42}})
}

func TestPicoClient_SendWhenConnectionClosed(t *testing.T) {
	server, _ := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, server)
	wsURL := picoWSURL(srv, "session_id=close-session")
	client := newPicoClientChannel(t, wsURL, func(cfg *config.PicoClientSettings) {
		cfg.SessionID = "close-session"
		cfg.Token = *config.NewSecureString("test-token")
	})
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	defer func() { _ = client.Stop(context.Background()) }()
	waitFor(t, 5*time.Second, "server connection", func() bool { return server.currentConnCount() == 1 })

	// Simulate a dropped connection: close the underlying conn, keep running.
	client.mu.Lock()
	pc := client.conn
	client.mu.Unlock()
	if pc == nil {
		t.Fatal("client connection is nil")
	}
	pc.close()
	if _, err := client.Send(context.Background(), newTestOutboundMessage("pico_client:s", "x", nil)); err == nil {
		t.Error("Send on closed client connection should fail")
	}
	if stop, err := client.StartTyping(context.Background(), "pico_client:s"); err == nil {
		stop()
	}
}

// TestClientPingLoop covers client pingLoop ticks against a live server.
func TestClientPingLoop(t *testing.T) {
	server, _ := newRunningChannel(t, nil)
	srv := newPicoHTTPServer(t, server)
	wsURL := picoWSURL(srv, "session_id=ping-session")
	client := newPicoClientChannel(t, wsURL, func(cfg *config.PicoClientSettings) {
		cfg.SessionID = "ping-session"
		cfg.PingInterval = 1
		cfg.ReadTimeout = 5
		cfg.Token = *config.NewSecureString("test-token")
	})
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	defer func() { _ = client.Stop(context.Background()) }()
	waitFor(t, 5*time.Second, "server connection", func() bool { return server.currentConnCount() == 1 })
	// Give the client ping loop time to tick at least once.
	time.Sleep(1300 * time.Millisecond)
	if !client.IsRunning() {
		t.Error("client should stay running through ping/pong")
	}
	_ = client.config
	_ = protocoltypes.UsageInfo{}
}
