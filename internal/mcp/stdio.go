package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sharedprocess "github.com/trobrock/notch/internal/process"
)

type rpcReply struct {
	response rpcResponse
	err      error
}

type lockedBuffer struct {
	mu        sync.Mutex
	b         bytes.Buffer
	truncated bool
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLen := len(p)
	remaining := sharedprocess.OutputLimit - b.b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		_, _ = b.b.Write(p)
	} else if len(p) != 0 {
		b.truncated = true
	}
	return originalLen, nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	message := strings.TrimSpace(b.b.String())
	if b.truncated {
		message += "\n[stderr truncated]"
	}
	return message
}

type stdioClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr lockedBuffer

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan rpcReply
	deadErr error
	nextID  atomic.Int64
	wait    chan error
	once    sync.Once
}

func newStdioClient(ctx context.Context, cfg ServerConfig) (*stdioClient, error) {
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Env = sharedprocess.MinimalEnvironment(cfg.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open server stdout: %w", err)
	}
	c := &stdioClient{
		cmd:     cmd,
		stdin:   stdin,
		pending: make(map[string]chan rpcReply),
		wait:    make(chan error, 1),
	}
	cmd.Stderr = &c.stderr // Always drain stderr; it is included in process errors.
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", cfg.Command, err)
	}
	go c.readLoop(stdout)
	go func() {
		err := cmd.Wait()
		if err != nil {
			message := c.stderr.String()
			if message != "" {
				err = fmt.Errorf("MCP server exited: %w; stderr: %s", err, message)
			} else {
				err = fmt.Errorf("MCP server exited: %w", err)
			}
		} else {
			err = io.EOF
		}
		c.fail(err)
		c.wait <- err
	}()
	return c, nil
}

func (c *stdioClient) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	dead := c.deadErr
	c.mu.Unlock()
	if dead != nil {
		return dead
	}
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("write MCP stdio request: %w", err)
	}
	return nil
}

func (c *stdioClient) call(ctx context.Context, method string, params any, dst any) error {
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	if c.deadErr != nil {
		err := c.deadErr
		c.mu.Unlock()
		return err
	}
	c.pending[key] = ch
	c.mu.Unlock()

	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.removePending(key)
		return err
	}
	select {
	case reply := <-ch:
		if err := ctx.Err(); err != nil {
			c.cancelRequest(id, err)
			return err
		}
		if reply.err != nil {
			return reply.err
		}
		if err := validateResponse(reply.response, id); err != nil {
			return err
		}
		if reply.response.Error != nil {
			return reply.response.Error
		}
		if dst != nil && len(reply.response.Result) != 0 {
			if err := json.Unmarshal(reply.response.Result, dst); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.removePending(key)
		c.cancelRequest(id, ctx.Err())
		return ctx.Err()
	}
}

func (c *stdioClient) cancelRequest(id int64, reason error) {
	// Cancellation is best effort and must not delay returning to the caller.
	go func() {
		_ = c.notify(context.Background(), "notifications/cancelled", map[string]any{
			"requestId": id,
			"reason":    reason.Error(),
		})
	}()
}

func (c *stdioClient) notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.write(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", method, params})
}

func (c *stdioClient) removePending(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

func (c *stdioClient) readLoop(reader io.Reader) {
	decoder := json.NewDecoder(reader)
	for {
		var response rpcResponse
		if err := decoder.Decode(&response); err != nil {
			if !errors.Is(err, io.EOF) {
				c.fail(fmt.Errorf("read MCP stdio response: %w", err))
			}
			return
		}
		if len(response.ID) == 0 || bytes.Equal(response.ID, []byte("null")) {
			continue // server notification
		}
		if response.Method != "" {
			c.replyToServerRequest(response)
			continue
		}
		key := strings.Trim(string(response.ID), `"`)
		c.mu.Lock()
		ch := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if ch != nil {
			ch <- rpcReply{response: response}
		}
	}
}

func (c *stdioClient) replyToServerRequest(request rpcResponse) {
	response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
	if request.Method == "ping" {
		response["result"] = map[string]any{}
	} else {
		response["error"] = map[string]any{"code": -32601, "message": "method not supported by client"}
	}
	_ = c.write(response)
}

func (c *stdioClient) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	c.mu.Lock()
	if c.deadErr != nil {
		c.mu.Unlock()
		return
	}
	c.deadErr = err
	pending := c.pending
	c.pending = make(map[string]chan rpcReply)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcReply{err: err}
	}
}

func (c *stdioClient) close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		select {
		case <-c.wait:
		case <-time.After(500 * time.Millisecond):
			_ = c.cmd.Process.Kill()
			<-c.wait
		}
	})
	return nil
}
