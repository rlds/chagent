package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rlds/chagent/pkg/isolation"
)

var isolatedCommandTerminateDuration = 5 * time.Second

// isolatedCommandTransport 与 SDK 的 command transport 行为一致，但会把
// 进程启动交给 pkg/isolation，从而让 Windows 的启动后钩子也能执行。
//
// 子进程的生命周期绑定在 MCP session 上（通过 Close 释放），而不是绑定在
// 触发本次连接的那个 context 上。CommandContext 仍然用于取消时杀进程的
// 安全兜底，但传入的 context 由调用方 context 经 context.WithoutCancel
// 派生而来：reload/turn 的请求 context 会在请求/轮次结束时立即取消，绝不能
// 因此拆掉一个仍在运行的 MCP session。
type isolatedCommandTransport struct {
	Command           *exec.Cmd
	TerminateDuration time.Duration
	// Cancel 取消子进程的 context；在 Close（以及 Connect 失败）时调用，
	// 以便 MCP session 结束时释放该进程。
	Cancel context.CancelFunc
}

func (t *isolatedCommandTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	release := func(err error) (sdkmcp.Connection, error) {
		if t.Cancel != nil {
			t.Cancel()
		}
		return nil, err
	}

	stdout, err := t.Command.StdoutPipe()
	if err != nil {
		return release(err)
	}
	stdout = io.NopCloser(stdout)
	stdin, err := t.Command.StdinPipe()
	if err != nil {
		return release(err)
	}
	if err := isolation.Start(t.Command); err != nil {
		return release(err)
	}
	td := t.TerminateDuration
	if td <= 0 {
		td = isolatedCommandTerminateDuration
	}
	return newIsolatedIOConn(&isolatedPipeRWC{
		cmd:               t.Command,
		stdout:            stdout,
		stdin:             stdin,
		terminateDuration: td,
		cancel:            t.Cancel,
	}), nil
}

type isolatedPipeRWC struct {
	cmd               *exec.Cmd
	stdout            io.ReadCloser
	stdin             io.WriteCloser
	terminateDuration time.Duration
	cancel            context.CancelFunc
}

// stopProcessContext 释放子进程 context（CommandContext 的安全兜底）。
// 在进程已经退出后调用也是安全的。
func (s *isolatedPipeRWC) stopProcessContext() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *isolatedPipeRWC) Read(p []byte) (n int, err error) {
	return s.stdout.Read(p)
}

func (s *isolatedPipeRWC) Write(p []byte) (n int, err error) {
	return s.stdin.Write(p)
}

func (s *isolatedPipeRWC) Close() error {
	if err := s.stdin.Close(); err != nil {
		// 异常退出时管道可能已经关闭；仍需取消进程 context，
		// 以免子进程比 MCP session 活得更久。
		s.stopProcessContext()
		return fmt.Errorf("closing stdin: %w", err)
	}
	resChan := make(chan error, 1)
	go func() {
		resChan <- s.cmd.Wait()
	}()
	wait := func() (error, bool) {
		select {
		case err := <-resChan:
			return err, true
		case <-time.After(s.terminateDuration):
		}
		return nil, false
	}
	if err, ok := wait(); ok {
		s.stopProcessContext()
		return err
	}
	// 进程忽略了 EOF：先取消 context（由 CommandContext 杀进程），
	// 若它仍未退出，再依次退回 SIGTERM/SIGKILL。
	s.stopProcessContext()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err == nil {
		if err, ok := wait(); ok {
			return err
		}
	}
	if err := s.cmd.Process.Kill(); err != nil {
		return err
	}
	if err, ok := wait(); ok {
		return err
	}
	return fmt.Errorf("unresponsive subprocess")
}

type isolatedIOConn struct {
	writeMu   sync.Mutex
	rwc       io.ReadWriteCloser
	incoming  <-chan isolatedMsgOrErr
	queue     []jsonrpc.Message
	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

type isolatedMsgOrErr struct {
	msg json.RawMessage
	err error
}

func newIsolatedIOConn(rwc io.ReadWriteCloser) *isolatedIOConn {
	incoming := make(chan isolatedMsgOrErr)
	closed := make(chan struct{})
	go func() {
		dec := json.NewDecoder(rwc)
		for {
			var raw json.RawMessage
			err := dec.Decode(&raw)
			if err == nil {
				var tr [1]byte
				if n, readErr := dec.Buffered().Read(tr[:]); n > 0 {
					if tr[0] != '\n' && tr[0] != '\r' {
						err = fmt.Errorf("invalid trailing data at the end of stream")
					}
				} else if readErr != nil && readErr != io.EOF {
					err = readErr
				}
			}
			select {
			case incoming <- isolatedMsgOrErr{msg: raw, err: err}:
			case <-closed:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return &isolatedIOConn{rwc: rwc, incoming: incoming, closed: closed}
}

func (c *isolatedIOConn) SessionID() string { return "" }

func (c *isolatedIOConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if len(c.queue) > 0 {
		next := c.queue[0]
		c.queue = c.queue[1:]
		return next, nil
	}
	var raw json.RawMessage
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case v := <-c.incoming:
		if v.err != nil {
			return nil, v.err
		}
		raw = v.msg
	case <-c.closed:
		return nil, io.EOF
	}
	msgs, err := readIsolatedBatch(raw)
	if err != nil {
		return nil, err
	}
	c.queue = msgs[1:]
	return msgs[0], nil
}

func readIsolatedBatch(data []byte) ([]jsonrpc.Message, error) {
	var rawBatch []json.RawMessage
	if err := json.Unmarshal(data, &rawBatch); err == nil {
		if len(rawBatch) == 0 {
			return nil, fmt.Errorf("empty batch")
		}
		msgs := make([]jsonrpc.Message, 0, len(rawBatch))
		for _, raw := range rawBatch {
			msg, err := jsonrpc.DecodeMessage(raw)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, msg)
		}
		return msgs, nil
	}
	msg, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return nil, err
	}
	return []jsonrpc.Message{msg}, nil
}

func (c *isolatedIOConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}
	data = append(data, '\n')
	_, err = c.rwc.Write(data)
	return err
}

func (c *isolatedIOConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.rwc.Close()
		close(c.closed)
	})
	return c.closeErr
}

var (
	_ sdkmcp.Transport  = (*isolatedCommandTransport)(nil)
	_ sdkmcp.Connection = (*isolatedIOConn)(nil)
)
