// Package plugin provides the SDK used to write executable Notch extensions.
//
// Plugins communicate with Notch over line-delimited JSON-RPC 2.0 on stdin and
// stdout. Diagnostic output must therefore be written to stderr.
package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

const maxMessageSize = 16 << 20

// ToolDefinition is the JSON schema advertised for a tool.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolResult is returned by a tool invocation.
type ToolResult struct {
	Content string         `json:"content"`
	IsError bool           `json:"is_error,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// Progress reports a status update for the current tool invocation.
type Progress func(message string)

// ToolHandler handles a tool invocation. args contains the JSON object supplied
// by the model. Host calls can be made with the helpers in this package or with
// ClientFromContext.
type ToolHandler func(ctx context.Context, args json.RawMessage, progress Progress) (ToolResult, error)

// Tool describes and implements one model-callable tool. Definition is
// provided as a convenience for code that builds definitions separately; the
// direct fields take precedence when non-zero.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Definition  ToolDefinition
	Execute     ToolHandler
}

// HookHandler handles an event. Its result is merged into the event by Notch.
type HookHandler func(ctx context.Context, event map[string]any) (map[string]any, error)

// Hook describes an event hook. Name and Event are aliases; Name takes
// precedence.
type Hook struct {
	Name   string
	Event  string
	Handle HookHandler
}

// CommandHandler handles the argument text following a command.
type CommandHandler func(ctx context.Context, args string) (string, error)

// Command describes and implements one user command.
type Command struct {
	Name        string
	Description string
	Execute     CommandHandler
}

// Extension is the complete set of capabilities advertised by a plugin.
type Extension struct {
	Tools    []Tool
	Hooks    []Hook
	Commands []Command
}

// ExecResult is the result of a host process invocation.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Host is the set of operations supplied by Notch to an executing plugin.
type Host interface {
	CWD(context.Context) (string, error)
	Exec(context.Context, string, []string) (stdout, stderr string, exitCode int, err error)
	Input(context.Context, string, string) (string, error)
	Select(context.Context, string, []string) (string, error)
	Notify(context.Context, string, string) error
}

type clientContextKey struct{}
type requestIDContextKey struct{}

// Client is a concurrency-safe client for calls from a plugin to its host. A
// Client is installed in every handler context by Serve.
type Client struct {
	reader io.Reader
	writer io.Writer

	writeMu sync.Mutex
	nextID  atomic.Uint64

	mu       sync.Mutex
	pending  map[string]chan callResult
	requests map[string]context.CancelFunc
	closed   chan struct{}
	closeOne sync.Once
	closeErr error
}

type callResult struct {
	result json.RawMessage
	err    error
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) > 0 && string(e.Data) != "null" {
		return fmt.Sprintf("JSON-RPC error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// NewClient creates a protocol client over r and w. Most plugins should use
// Serve; NewClient and (*Client).Serve are useful for embedding and tests.
func NewClient(r io.Reader, w io.Writer) *Client {
	return &Client{
		reader: r, writer: w,
		pending:  make(map[string]chan callResult),
		requests: make(map[string]context.CancelFunc),
		closed:   make(chan struct{}),
	}
}

// Serve runs an extension over stdin and stdout until the host disconnects or
// ctx is cancelled.
func Serve(ctx context.Context, extension Extension) error {
	return NewClient(os.Stdin, os.Stdout).Serve(ctx, extension)
}

// ServeIO is like Serve but uses the supplied protocol streams.
func ServeIO(ctx context.Context, r io.Reader, w io.Writer, extension Extension) error {
	return NewClient(r, w).Serve(ctx, extension)
}

// Serve processes host requests. It is safe for handlers and host calls to run
// concurrently.
func (c *Client) Serve(ctx context.Context, extension Extension) error {
	if c == nil || c.reader == nil || c.writer == nil {
		return errors.New("plugin: protocol reader and writer are required")
	}
	if err := validate(extension); err != nil {
		return err
	}
	go func() {
		select {
		case <-ctx.Done():
			c.shutdown(ctx.Err())
		case <-c.closed:
		}
	}()
	go c.readLoop(ctx, extension)

	<-c.closed
	c.mu.Lock()
	err := c.closeErr
	c.mu.Unlock()
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (c *Client) readLoop(ctx context.Context, extension Extension) {
	scanner := bufio.NewScanner(c.reader)
	scanner.Buffer(make([]byte, 64*1024), maxMessageSize)
	for scanner.Scan() {
		select {
		case <-c.closed:
			return
		default:
		}
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			c.shutdown(fmt.Errorf("plugin protocol error: invalid JSON: %w", err))
			return
		}
		if message.JSONRPC != "2.0" {
			c.shutdown(fmt.Errorf("plugin protocol error: unsupported jsonrpc version %q", message.JSONRPC))
			return
		}
		if message.Method == "$/cancelRequest" {
			c.handleCancel(message.Params)
			continue
		}
		if message.Method != "" {
			if len(message.ID) != 0 { // Unknown notifications need no response.
				c.startRequest(ctx, extension, message)
			}
			continue
		}
		if len(message.ID) == 0 {
			c.shutdown(errors.New("plugin protocol error: response has no id"))
			return
		}
		c.handleResponse(message)
	}
	if err := scanner.Err(); err != nil {
		c.shutdown(fmt.Errorf("plugin protocol error: read stdin: %w", err))
	} else {
		c.shutdown(io.EOF)
	}
}

func validate(extension Extension) error {
	tools := make(map[string]bool)
	for _, tool := range extension.Tools {
		definition := tool.definition()
		if definition.Name == "" || tool.Execute == nil {
			return errors.New("plugin: every tool requires a name and execute function")
		}
		if tools[definition.Name] {
			return fmt.Errorf("plugin: duplicate tool %q", definition.Name)
		}
		tools[definition.Name] = true
	}
	commands := make(map[string]bool)
	for _, command := range extension.Commands {
		if command.Name == "" || command.Execute == nil {
			return errors.New("plugin: every command requires a name and execute function")
		}
		if commands[command.Name] {
			return fmt.Errorf("plugin: duplicate command %q", command.Name)
		}
		commands[command.Name] = true
	}
	for _, hook := range extension.Hooks {
		if hook.name() == "" || hook.Handle == nil {
			return errors.New("plugin: every hook requires a name and handle function")
		}
	}
	return nil
}

func (t Tool) definition() ToolDefinition {
	definition := t.Definition
	if t.Name != "" {
		definition.Name = t.Name
	}
	if t.Description != "" {
		definition.Description = t.Description
	}
	if t.InputSchema != nil {
		definition.InputSchema = t.InputSchema
	}
	return definition
}

func (h Hook) name() string {
	if h.Name != "" {
		return h.Name
	}
	return h.Event
}

func (c *Client) startRequest(parent context.Context, extension Extension, message rpcMessage) {
	ctx, cancel := context.WithCancel(parent)
	ctx = context.WithValue(ctx, clientContextKey{}, c)
	ctx = context.WithValue(ctx, requestIDContextKey{}, append(json.RawMessage(nil), message.ID...))
	key := string(message.ID)
	c.mu.Lock()
	c.requests[key] = cancel
	c.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			c.mu.Lock()
			delete(c.requests, key)
			c.mu.Unlock()
		}()
		result, rpcErr := c.dispatch(ctx, extension, message.Method, message.Params)
		response := rpcMessage{JSONRPC: "2.0", ID: message.ID}
		if rpcErr != nil {
			response.Error = rpcErr
		} else {
			data, err := json.Marshal(result)
			if err != nil {
				response.Error = &rpcError{Code: -32603, Message: err.Error()}
			} else {
				response.Result = data
			}
		}
		_ = c.write(response)
	}()
}

func (c *Client) dispatch(ctx context.Context, extension Extension, method string, params json.RawMessage) (any, *rpcError) {
	badParams := func(err error) (any, *rpcError) {
		return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	handlerError := func(err error) (any, *rpcError) {
		code := -32000
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			code = -32800
		}
		return nil, &rpcError{Code: code, Message: err.Error()}
	}

	switch method {
	case "initialize":
		tools := make([]ToolDefinition, len(extension.Tools))
		for i, tool := range extension.Tools {
			tools[i] = tool.definition()
		}
		hooks := make([]map[string]string, len(extension.Hooks))
		for i, hook := range extension.Hooks {
			hooks[i] = map[string]string{"name": hook.name()}
		}
		commands := make([]map[string]string, len(extension.Commands))
		for i, command := range extension.Commands {
			commands[i] = map[string]string{"name": command.Name, "description": command.Description}
		}
		return struct {
			Tools    []ToolDefinition    `json:"tools"`
			Hooks    []map[string]string `json:"hooks"`
			Commands []map[string]string `json:"commands"`
		}{tools, hooks, commands}, nil

	case "tool.execute":
		var value struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		for _, tool := range extension.Tools {
			if tool.definition().Name == value.Name {
				result, err := tool.Execute(ctx, value.Args, func(message string) { _ = c.Update(ctx, message) })
				if err != nil {
					return handlerError(err)
				}
				return result, nil
			}
		}
		return nil, &rpcError{Code: -32601, Message: "tool not found: " + value.Name}

	case "hook.handle":
		var value struct {
			Name  string         `json:"name"`
			Event map[string]any `json:"event"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		for _, hook := range extension.Hooks {
			if hook.name() == value.Name {
				result, err := hook.Handle(ctx, value.Event)
				if err != nil {
					return handlerError(err)
				}
				if result == nil {
					result = map[string]any{}
				}
				return result, nil
			}
		}
		return nil, &rpcError{Code: -32601, Message: "hook not found: " + value.Name}

	case "command.execute":
		var value struct {
			Name string `json:"name"`
			Args string `json:"args"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		for _, command := range extension.Commands {
			if command.Name == value.Name {
				result, err := command.Execute(ctx, value.Args)
				if err != nil {
					return handlerError(err)
				}
				return result, nil
			}
		}
		return nil, &rpcError{Code: -32601, Message: "command not found: " + value.Name}
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
	}
}

func (c *Client) handleCancel(params json.RawMessage) {
	var value struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(params, &value) != nil {
		return
	}
	c.mu.Lock()
	cancel := c.requests[string(value.ID)]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Client) handleResponse(message rpcMessage) {
	key := string(message.ID)
	c.mu.Lock()
	pending := c.pending[key]
	if pending != nil {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if pending == nil {
		return
	} // A response to a cancelled host call may be late.
	if message.Error != nil {
		pending <- callResult{err: message.Error}
		return
	}
	if len(message.Result) == 0 {
		pending <- callResult{err: errors.New("plugin protocol error: response has neither result nor error")}
		return
	}
	pending <- callResult{result: message.Result}
}

func (c *Client) write(message rpcMessage) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.writer.Write(data); err != nil {
		return fmt.Errorf("plugin: write protocol message: %w", err)
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	id := json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("plugin-%d", c.nextID.Add(1))))
	key := string(id)
	response := make(chan callResult, 1)
	c.mu.Lock()
	select {
	case <-c.closed:
		err := c.closeErr
		c.mu.Unlock()
		return err
	default:
	}
	c.pending[key] = response
	c.mu.Unlock()

	data, err := json.Marshal(params)
	if err != nil {
		c.removePending(key)
		return err
	}
	if err := c.write(rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: data}); err != nil {
		c.removePending(key)
		return err
	}
	select {
	case got := <-response:
		if got.err != nil {
			return got.err
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(got.result, result); err != nil {
			return fmt.Errorf("plugin: invalid %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		if c.removePending(key) {
			params, _ := json.Marshal(struct {
				ID json.RawMessage `json:"id"`
			}{id})
			_ = c.write(rpcMessage{JSONRPC: "2.0", Method: "$/cancelRequest", Params: params})
		}
		return ctx.Err()
	case <-c.closed:
		c.mu.Lock()
		err := c.closeErr
		c.mu.Unlock()
		return err
	}
}

func (c *Client) removePending(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pending[key]; !ok {
		return false
	}
	delete(c.pending, key)
	return true
}

func (c *Client) shutdown(err error) {
	c.closeOne.Do(func() {
		if err == nil {
			err = io.EOF
		}
		c.mu.Lock()
		c.closeErr = err
		pending := c.pending
		c.pending = make(map[string]chan callResult)
		requests := c.requests
		c.requests = make(map[string]context.CancelFunc)
		c.mu.Unlock()
		for _, cancel := range requests {
			cancel()
		}
		for _, response := range pending {
			response <- callResult{err: err}
		}
		close(c.closed)
	})
}

// Update sends a progress notification associated with the host request that
// created ctx. It returns an error if ctx is not a tool handler context.
func (c *Client) Update(ctx context.Context, message string) error {
	id, ok := ctx.Value(requestIDContextKey{}).(json.RawMessage)
	if !ok || len(id) == 0 {
		return errors.New("plugin: progress requires a handler context")
	}
	params, _ := json.Marshal(struct {
		ID      json.RawMessage `json:"id"`
		Message string          `json:"message"`
	}{id, message})
	return c.write(rpcMessage{JSONRPC: "2.0", Method: "tool.update", Params: params})
}

// ClientFromContext returns the host client installed in a handler context.
func ClientFromContext(ctx context.Context) (*Client, bool) {
	client, ok := ctx.Value(clientContextKey{}).(*Client)
	return client, ok
}

// HostFromContext returns the Host installed in a handler context.
func HostFromContext(ctx context.Context) (Host, bool) {
	client, ok := ClientFromContext(ctx)
	if !ok {
		return nil, false
	}
	return client, true
}

func clientFor(ctx context.Context) (*Client, error) {
	client, ok := ClientFromContext(ctx)
	if !ok {
		return nil, errors.New("plugin: no host client in context")
	}
	return client, nil
}

// CWD returns the host's current working directory.
func (c *Client) CWD(ctx context.Context) (string, error) {
	var result string
	err := c.call(ctx, "host.cwd", struct{}{}, &result)
	return result, err
}

// Exec asks the host to execute command with args.
func (c *Client) Exec(ctx context.Context, command string, args []string) (stdout, stderr string, exitCode int, err error) {
	var result ExecResult
	err = c.call(ctx, "host.exec", struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}{command, args}, &result)
	return result.Stdout, result.Stderr, result.ExitCode, err
}

// Input asks the host to display a text input.
func (c *Client) Input(ctx context.Context, prompt, placeholder string) (string, error) {
	var result string
	err := c.call(ctx, "host.ui.input", struct {
		Prompt      string `json:"prompt"`
		Placeholder string `json:"placeholder"`
	}{prompt, placeholder}, &result)
	return result, err
}

// Select asks the host to display a choice list.
func (c *Client) Select(ctx context.Context, prompt string, options []string) (string, error) {
	var result string
	err := c.call(ctx, "host.ui.select", struct {
		Prompt  string   `json:"prompt"`
		Options []string `json:"options"`
	}{prompt, options}, &result)
	return result, err
}

// Notify asks the host to display a notification.
func (c *Client) Notify(ctx context.Context, message, level string) error {
	var result any
	return c.call(ctx, "host.ui.notify", struct {
		Message string `json:"message"`
		Level   string `json:"level"`
	}{message, level}, &result)
}

// CWD calls CWD on the host in ctx.
func CWD(ctx context.Context) (string, error) {
	client, err := clientFor(ctx)
	if err != nil {
		return "", err
	}
	return client.CWD(ctx)
}

// Exec calls Exec on the host in ctx.
func Exec(ctx context.Context, command string, args ...string) (stdout, stderr string, exitCode int, err error) {
	client, err := clientFor(ctx)
	if err != nil {
		return "", "", 0, err
	}
	return client.Exec(ctx, command, args)
}

// Input calls Input on the host in ctx.
func Input(ctx context.Context, prompt, placeholder string) (string, error) {
	client, err := clientFor(ctx)
	if err != nil {
		return "", err
	}
	return client.Input(ctx, prompt, placeholder)
}

// Select calls Select on the host in ctx.
func Select(ctx context.Context, prompt string, options []string) (string, error) {
	client, err := clientFor(ctx)
	if err != nil {
		return "", err
	}
	return client.Select(ctx, prompt, options)
}

// Notify calls Notify on the host in ctx.
func Notify(ctx context.Context, message, level string) error {
	client, err := clientFor(ctx)
	if err != nil {
		return err
	}
	return client.Notify(ctx, message, level)
}
