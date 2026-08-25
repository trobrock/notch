package extension

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trobrock/notch/internal/model"
	sharedprocess "github.com/trobrock/notch/internal/process"
)

const maxPluginMessage = 16 << 20 // JSON-RPC is line delimited; permit messages much larger than Scanner's 64 KiB default.

// Manifest is the contents of an extension's plugin.json file.
type Manifest struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Enabled bool     `json:"enabled"`
}

// Plugin is a running executable extension. Close should be called when the
// extension is no longer needed.
type Plugin struct {
	Name     string
	Dir      string
	Manifest Manifest

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex

	nextID   atomic.Uint64
	mu       sync.Mutex
	pending  map[string]*pendingCall
	canceled map[string]bool
	stopErr  error
	done     chan struct{}
	waitDone chan struct{}
	stop     sync.Once
	lease    *Registration
	host     Host
	ctx      context.Context
	cancel   context.CancelFunc
}

type pendingCall struct {
	ch       chan callResult
	onUpdate func(string)
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
	if len(e.Data) != 0 && string(e.Data) != "null" {
		return fmt.Sprintf("JSON-RPC error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

type initializeResult struct {
	Tools    []model.ToolDefinition `json:"tools"`
	Hooks    []json.RawMessage      `json:"hooks"`
	Commands []commandDefinition    `json:"commands"`
}

type commandDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DiscoverAndLoad finds plugin.json files below dirs, starts enabled plugins,
// initializes them, and registers the returned tools, hooks, and commands.
// A bad plugin does not prevent other plugins from loading; its error is
// returned in warnings.
func DiscoverAndLoad(ctx context.Context, dirs []string, registry *Registry, host Host) ([]*Plugin, []error) {
	if registry == nil {
		return nil, []error{errors.New("extension registry is nil")}
	}

	paths, warnings := discoverManifests(dirs)
	plugins := make([]*Plugin, 0, len(paths))
	for _, path := range paths {
		manifest, err := readManifest(path)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if !manifest.Enabled {
			continue
		}
		plugin, err := startPlugin(ctx, path, manifest, registry, host)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("plugin %q (%s): %w", manifest.Name, path, err))
			continue
		}
		plugins = append(plugins, plugin)
	}
	return plugins, warnings
}

func discoverManifests(dirs []string) ([]string, []error) {
	seen := make(map[string]bool)
	var paths []string
	var warnings []error
	for _, root := range dirs {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("extension directory %q: %w", root, err))
			continue
		}
		err = filepath.WalkDir(abs, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				warnings = append(warnings, fmt.Errorf("discover extensions at %s: %w", path, walkErr))
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.IsDir() && entry.Name() == "plugin.json" && !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			warnings = append(warnings, fmt.Errorf("discover extensions in %s: %w", root, err))
		}
	}
	sort.Strings(paths)
	return paths, warnings
}

func readManifest(path string) (Manifest, error) {
	var manifest Manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("invalid manifest: %w", err)
	}
	manifest.Name = strings.TrimSpace(manifest.Name)
	if manifest.Name == "" {
		return manifest, errors.New("manifest requires a name")
	}
	if len(manifest.Command) == 0 || strings.TrimSpace(manifest.Command[0]) == "" {
		return manifest, errors.New("manifest requires a command")
	}
	return manifest, nil
}

func startPlugin(ctx context.Context, manifestPath string, manifest Manifest, registry *Registry, host Host) (*Plugin, error) {
	pluginCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(pluginCtx, manifest.Command[0], manifest.Command[1:]...)
	cmd.Dir = filepath.Dir(manifestPath)
	cmd.Env = sharedprocess.MinimalEnvironment(nil)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, fmt.Errorf("open stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdin.Close()
		return nil, fmt.Errorf("start: %w", err)
	}

	plugin := &Plugin{
		Name: manifest.Name, Dir: filepath.Dir(manifestPath), Manifest: manifest,
		cmd: cmd, stdin: stdin, pending: make(map[string]*pendingCall), canceled: make(map[string]bool),
		done: make(chan struct{}), waitDone: make(chan struct{}), host: host,
		ctx: pluginCtx, cancel: cancel,
	}
	go plugin.readLoop(stdout)
	go func() {
		err := cmd.Wait()
		if err == nil {
			err = errors.New("plugin process exited")
		} else {
			err = fmt.Errorf("plugin process exited: %w", err)
		}
		plugin.finish(err)
		close(plugin.waitDone)
	}()

	var initialized initializeResult
	if err := plugin.call(ctx, "initialize", map[string]any{}, &initialized, nil); err != nil {
		_ = plugin.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := plugin.register(registry, initialized); err != nil {
		_ = plugin.Close()
		return nil, err
	}
	return plugin, nil
}

func (p *Plugin) register(registry *Registry, initialized initializeResult) error {
	tools := make([]Tool, 0, len(initialized.Tools))
	commands := make([]Command, 0, len(initialized.Commands))
	hooks := make([]string, 0, len(initialized.Hooks))
	seenTools, seenCommands := map[string]bool{}, map[string]bool{}

	for _, definition := range initialized.Tools {
		definition := definition
		if definition.Name == "" {
			return errors.New("initialize returned a tool without a name")
		}
		if seenTools[definition.Name] {
			return fmt.Errorf("initialize returned duplicate tool %q", definition.Name)
		}
		seenTools[definition.Name] = true
		tools = append(tools, Tool{Definition: definition, Source: p.Name, Execute: func(ctx context.Context, args json.RawMessage, onUpdate func(string)) (ToolResult, error) {
			params := struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			}{definition.Name, args}
			var raw json.RawMessage
			if err := p.call(ctx, "tool.execute", params, &raw, onUpdate); err != nil {
				return ToolResult{}, err
			}
			var result ToolResult
			if err := json.Unmarshal(raw, &result); err == nil {
				return result, nil
			}
			var content string
			if err := json.Unmarshal(raw, &content); err == nil {
				return ToolResult{Content: content}, nil
			}
			return ToolResult{}, errors.New("tool.execute returned an invalid result")
		}})
	}

	for _, definition := range initialized.Commands {
		definition := definition
		if definition.Name == "" {
			return errors.New("initialize returned a command without a name")
		}
		if seenCommands[definition.Name] {
			return fmt.Errorf("initialize returned duplicate command %q", definition.Name)
		}
		seenCommands[definition.Name] = true
		commands = append(commands, Command{Name: definition.Name, Description: definition.Description, Source: p.Name, Execute: func(ctx context.Context, args string) (string, error) {
			params := struct {
				Name string `json:"name"`
				Args string `json:"args"`
			}{definition.Name, args}
			var raw json.RawMessage
			if err := p.call(ctx, "command.execute", params, &raw, nil); err != nil {
				return "", err
			}
			var output string
			if err := json.Unmarshal(raw, &output); err == nil {
				return output, nil
			}
			var wrapped struct {
				Output  string `json:"output"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &wrapped); err != nil {
				return "", errors.New("command.execute returned an invalid result")
			}
			if wrapped.Output != "" {
				return wrapped.Output, nil
			}
			return wrapped.Content, nil
		}})
	}

	for _, raw := range initialized.Hooks {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			var definition struct {
				Name  string `json:"name"`
				Event string `json:"event"`
			}
			if err := json.Unmarshal(raw, &definition); err != nil {
				return errors.New("initialize returned an invalid hook")
			}
			name = definition.Name
			if name == "" {
				name = definition.Event
			}
		}
		if name == "" {
			return errors.New("initialize returned a hook without an event name")
		}
		hooks = append(hooks, name)
	}

	hookRegistrations := make([]HookRegistration, 0, len(hooks))
	for _, event := range hooks {
		event := event
		hookRegistrations = append(hookRegistrations, HookRegistration{Event: event, Source: p.Name, Handler: func(ctx context.Context, value map[string]any) (map[string]any, error) {
			params := struct {
				Name  string         `json:"name"`
				Event map[string]any `json:"event"`
			}{event, value}
			var result map[string]any
			if err := p.call(ctx, "hook.handle", params, &result, nil); err != nil {
				return nil, err
			}
			return result, nil
		}})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopErr != nil {
		return p.stopErr
	}
	lease, err := registry.RegisterBatch(Batch{Tools: tools, Commands: commands, Hooks: hookRegistrations})
	if err != nil {
		return err
	}
	p.lease = lease
	return nil
}

func (p *Plugin) call(ctx context.Context, method string, params, result any, onUpdate func(string)) error {
	id := p.nextID.Add(1)
	idRaw := json.RawMessage(fmt.Sprintf("%d", id))
	key := string(idRaw)
	pending := &pendingCall{ch: make(chan callResult, 1), onUpdate: onUpdate}

	p.mu.Lock()
	if p.stopErr != nil {
		err := p.stopErr
		p.mu.Unlock()
		return err
	}
	p.pending[key] = pending
	p.mu.Unlock()

	paramRaw, err := json.Marshal(params)
	if err != nil {
		p.removePending(key)
		return err
	}
	if err := p.write(rpcMessage{JSONRPC: "2.0", ID: idRaw, Method: method, Params: paramRaw}); err != nil {
		p.removePending(key)
		return err
	}

	select {
	case response := <-pending.ch:
		if response.err != nil {
			return response.err
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(response.result, result); err != nil {
			return fmt.Errorf("invalid %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		if p.cancelPending(key) {
			cancelParams, _ := json.Marshal(struct {
				ID json.RawMessage `json:"id"`
			}{idRaw})
			_ = p.write(rpcMessage{JSONRPC: "2.0", Method: "$/cancelRequest", Params: cancelParams})
		}
		return ctx.Err()
	case <-p.done:
		p.mu.Lock()
		err := p.stopErr
		p.mu.Unlock()
		if err == nil {
			err = errors.New("plugin stopped")
		}
		return err
	}
}

func (p *Plugin) removePending(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pending[key]; ok {
		delete(p.pending, key)
		return true
	}
	return false
}

func (p *Plugin) cancelPending(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pending[key]; !ok {
		return false
	}
	delete(p.pending, key)
	p.canceled[key] = true
	return true
}

func (p *Plugin) write(message rpcMessage) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("write to plugin: %w", err)
	}
	return nil
}

func (p *Plugin) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxPluginMessage)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var message rpcMessage
		if err := json.Unmarshal(line, &message); err != nil {
			p.finish(fmt.Errorf("plugin protocol error: invalid JSON: %w", err))
			return
		}
		if message.JSONRPC != "2.0" {
			p.finish(fmt.Errorf("plugin protocol error: unsupported jsonrpc version %q", message.JSONRPC))
			return
		}
		if message.Method != "" {
			p.handleRequest(message)
			continue
		}
		if len(message.ID) == 0 {
			p.finish(errors.New("plugin protocol error: response has no id"))
			return
		}
		p.handleResponse(message)
	}
	if err := scanner.Err(); err != nil {
		p.finish(fmt.Errorf("plugin protocol error: read stdout: %w", err))
		return
	}
	p.finish(io.EOF)
}

func (p *Plugin) handleResponse(message rpcMessage) {
	key := string(message.ID)
	p.mu.Lock()
	pending := p.pending[key]
	if pending != nil {
		delete(p.pending, key)
	}
	wasCanceled := p.canceled[key]
	delete(p.canceled, key)
	p.mu.Unlock()
	if pending == nil {
		if wasCanceled { // A late response after $/cancelRequest is permitted.
			return
		}
		p.finish(fmt.Errorf("plugin protocol error: response has unknown id %s", message.ID))
		return
	}
	if message.Error != nil && len(message.Result) != 0 {
		err := errors.New("plugin protocol error: response contains both result and error")
		pending.ch <- callResult{err: err}
		p.finish(err)
		return
	}
	if message.Error != nil {
		pending.ch <- callResult{err: message.Error}
		return
	}
	if len(message.Result) == 0 {
		err := errors.New("plugin protocol error: response has neither result nor error")
		pending.ch <- callResult{err: err}
		p.finish(err)
		return
	}
	pending.ch <- callResult{result: message.Result}
}

func (p *Plugin) handleRequest(message rpcMessage) {
	// Progress is a notification associated with a currently executing tool.
	if message.Method == "tool.update" {
		p.handleUpdate(message.Params)
		return
	}
	go func() {
		result, rpcErr := p.dispatchHost(message.Method, message.Params)
		if len(message.ID) == 0 { // notification
			return
		}
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
		_ = p.write(response)
	}()
}

func (p *Plugin) handleUpdate(params json.RawMessage) {
	var update struct {
		ID             json.RawMessage `json:"id"`
		RequestID      json.RawMessage `json:"request_id"`
		RequestIDCamel json.RawMessage `json:"requestId"`
		Message        string          `json:"message"`
		Content        string          `json:"content"`
		Update         string          `json:"update"`
	}
	if json.Unmarshal(params, &update) != nil {
		return
	}
	if len(update.ID) == 0 {
		update.ID = update.RequestID
	}
	if len(update.ID) == 0 {
		update.ID = update.RequestIDCamel
	}
	if update.Message == "" {
		update.Message = update.Content
	}
	if update.Message == "" {
		update.Message = update.Update
	}
	p.mu.Lock()
	pending := p.pending[string(update.ID)]
	p.mu.Unlock()
	if pending != nil && pending.onUpdate != nil {
		pending.onUpdate(update.Message)
	}
}

func (p *Plugin) dispatchHost(method string, params json.RawMessage) (any, *rpcError) {
	if p.host == nil {
		return nil, &rpcError{Code: -32601, Message: "host methods are unavailable"}
	}
	badParams := func(err error) (any, *rpcError) {
		return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	hostError := func(err error) (any, *rpcError) {
		return nil, &rpcError{Code: -32000, Message: err.Error()}
	}

	switch method {
	case "host.cwd":
		return p.host.CWD(), nil
	case "host.exec":
		var value struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		stdout, stderr, code, err := p.host.Exec(p.ctx, value.Command, value.Args)
		if err != nil {
			return hostError(err)
		}
		return struct {
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
			ExitCode int    `json:"exit_code"`
		}{stdout, stderr, code}, nil
	case "host.ui.input":
		var value struct {
			Prompt      string `json:"prompt"`
			Placeholder string `json:"placeholder"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		answer, err := p.host.Input(p.ctx, value.Prompt, value.Placeholder)
		if err != nil {
			return hostError(err)
		}
		return answer, nil
	case "host.ui.select":
		var value struct {
			Prompt  string   `json:"prompt"`
			Options []string `json:"options"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		answer, err := p.host.Select(p.ctx, value.Prompt, value.Options)
		if err != nil {
			return hostError(err)
		}
		return answer, nil
	case "host.ui.notify":
		var value struct {
			Message string `json:"message"`
			Level   string `json:"level"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		p.host.Notify(value.Message, value.Level)
		return nil, nil
	case "host.session.append":
		var value struct {
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		if len(value.Data) == 0 {
			return badParams(errors.New("data is required"))
		}
		var data any
		decoder := json.NewDecoder(bytes.NewReader(value.Data))
		decoder.UseNumber()
		if err := decoder.Decode(&data); err != nil {
			return badParams(err)
		}
		if data == nil {
			return badParams(errors.New("data must not be null"))
		}
		if err := p.host.AppendSessionEntry(value.Kind, data); err != nil {
			return hostError(err)
		}
		return nil, nil
	case "host.session.entries":
		var value struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		entries, err := p.host.SessionEntries(value.Kind)
		if err != nil {
			return hostError(err)
		}
		return entries, nil
	case "host.ui.editor_text":
		value, err := p.host.EditorText(p.ctx)
		if err != nil {
			return hostError(err)
		}
		return value, nil
	case "host.ui.set_editor_text":
		var value struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		if err := p.host.SetEditorText(p.ctx, value.Text); err != nil {
			return hostError(err)
		}
		return nil, nil
	case "host.follow_up":
		var value struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		if err := p.host.FollowUp(value.Message); err != nil {
			return hostError(err)
		}
		return nil, nil
	case "host.ui.set_status":
		var value struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		p.host.SetStatus(value.Key, value.Value)
		return nil, nil
	case "host.ui.set_panel":
		var value struct {
			Key   string   `json:"key"`
			Title string   `json:"title"`
			Lines []string `json:"lines"`
		}
		if err := json.Unmarshal(params, &value); err != nil {
			return badParams(err)
		}
		p.host.SetPanel(value.Key, value.Title, value.Lines)
		return nil, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
	}
}

func (p *Plugin) finish(err error) {
	p.stop.Do(func() {
		if err == nil {
			err = errors.New("plugin stopped")
		}
		p.mu.Lock()
		p.stopErr = err
		pending := p.pending
		p.pending = make(map[string]*pendingCall)
		p.canceled = nil
		lease := p.lease
		p.lease = nil
		p.mu.Unlock()
		// Unregister before exposing the stopped plugin to new registry lookups.
		// Close does not invoke handlers and therefore cannot acquire p.mu.
		_ = lease.Close()
		for _, call := range pending {
			call.ch <- callResult{err: err}
		}
		close(p.done)
		p.cancel()
		_ = p.stdin.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
}

// Close terminates the plugin process and waits briefly for it to be reaped.
// It is safe to call Close more than once.
func (p *Plugin) Close() error {
	if p == nil {
		return nil
	}
	p.finish(errors.New("plugin closed"))
	select {
	case <-p.waitDone:
	case <-time.After(time.Second):
		return errors.New("timed out waiting for plugin process to exit")
	}
	return nil
}
