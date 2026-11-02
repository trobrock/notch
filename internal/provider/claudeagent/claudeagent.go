// Package claudeagent runs the installed Claude executable through its
// stream-json SDK protocol. It lets Notch use a Claude subscription without
// reading or copying Claude's OAuth credentials.
package claudeagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/trobrock/notch/internal/model"
)

const serverName = "notch"

// Config configures the Claude Agent provider.
type Config struct {
	Executable string
	Directory  string
	BaseURL    string
	Env        []string
}

// New returns a provider that uses the user's installed Claude login.
func New(cfg Config) model.Provider {
	return &provider{cfg: cfg}
}

// AbortPendingTurn stops a Claude process left waiting for tool results when a
// Notch prompt ends early because of cancellation or a run limit.
func (p *provider) AbortPendingTurn() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stop()
}

type provider struct {
	cfg Config
	mu  sync.Mutex
	run *run
}

type run struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan envelope
	done   chan error
	write  sync.Mutex

	mu          sync.Mutex
	tools       []model.ToolDefinition
	toolQueue   []toolIdentity
	pending     map[string]pendingCall
	results     map[string]model.Block
	accounted   usage
	initialized chan error
}

type toolIdentity struct {
	ID   string
	Name string
}

type pendingCall struct {
	RequestID string
	MessageID any
}

type envelope struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	Request        json.RawMessage `json:"request,omitempty"`
	Response       json.RawMessage `json:"response,omitempty"`
	Event          json.RawMessage `json:"event,omitempty"`
	Message        json.RawMessage `json:"message,omitempty"`
	Result         string          `json:"result,omitempty"`
	Errors         []string        `json:"errors,omitempty"`
	IsError        bool            `json:"is_error,omitempty"`
	Usage          usage           `json:"usage,omitempty"`
	StopReason     string          `json:"stop_reason,omitempty"`
	Version        string          `json:"claude_code_version,omitempty"`
	APIErrorStatus int             `json:"api_error_status,omitempty"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u *usage) add(other usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadInputTokens += other.CacheReadInputTokens
	u.CacheCreationInputTokens += other.CacheCreationInputTokens
}

func (u usage) subtract(other usage) usage {
	return usage{
		InputTokens:              max(0, u.InputTokens-other.InputTokens),
		OutputTokens:             max(0, u.OutputTokens-other.OutputTokens),
		CacheReadInputTokens:     max(0, u.CacheReadInputTokens-other.CacheReadInputTokens),
		CacheCreationInputTokens: max(0, u.CacheCreationInputTokens-other.CacheCreationInputTokens),
	}
}

func (p *provider) Stream(ctx context.Context, req model.Request, onEvent func(model.StreamEvent)) (model.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.run == nil {
		r, err := p.start(ctx, req)
		if err != nil {
			return model.Response{}, err
		}
		p.run = r
	} else {
		p.run.setTools(req.Tools)
		if err := p.run.resolveResults(req.Messages); err != nil {
			p.stop()
			return model.Response{}, err
		}
	}

	response, complete, err := p.run.readStep(ctx, onEvent)
	if err != nil || complete {
		p.stop()
	}
	return response, err
}

func (p *provider) start(ctx context.Context, req model.Request) (*run, error) {
	executable := strings.TrimSpace(p.cfg.Executable)
	if executable == "" {
		executable = strings.TrimSpace(os.Getenv("NOTCH_CLAUDE_PATH"))
	}
	if executable == "" {
		var err error
		executable, err = exec.LookPath("claude")
		if err != nil {
			return nil, errors.New("Claude Agent requires Claude Code on PATH; install it and run `claude auth login`")
		}
	}
	args := []string{
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--include-partial-messages", "--permission-mode", "dontAsk", "--tools=",
		"--allowedTools", "mcp__notch__*", "--await-initialize", "--no-session-persistence",
		"--setting-sources=", "--strict-mcp-config", "--settings", `{"disableAllHooks":true}`,
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	switch req.ReasoningLevel {
	case "off":
		args = append(args, "--thinking=disabled")
	case "minimal", "low":
		args = append(args, "--effort", "low")
	case "medium":
		args = append(args, "--effort", "medium")
	case "high":
		args = append(args, "--effort", "high")
	case "xhigh":
		args = append(args, "--effort", "xhigh")
	}
	cmd := exec.Command(executable, args...)
	cmd.Dir = p.cfg.Directory
	cmd.Env = os.Environ()
	if p.cfg.Env != nil {
		cmd.Env = append([]string(nil), p.cfg.Env...)
	}
	cmd.Env = setEnv(cmd.Env, "CLAUDE_CODE_ENTRYPOINT", "notch")
	cmd.Env = setEnv(cmd.Env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS", "1")
	cmd.Env = setEnv(cmd.Env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	cmd.Env = setEnv(cmd.Env, "DISABLE_AUTO_COMPACT", "1")
	cmd.Env = setEnv(cmd.Env, "ENABLE_TOOL_SEARCH", "false")
	cmd.Env = setEnv(cmd.Env, "MAX_MCP_OUTPUT_TOKENS", "200000")
	if strings.TrimSpace(p.cfg.BaseURL) != "" {
		cmd.Env = setEnv(cmd.Env, "ANTHROPIC_BASE_URL", strings.TrimRight(strings.TrimSpace(p.cfg.BaseURL), "/"))
	}
	if req.MaxTokens > 0 {
		cmd.Env = setEnv(cmd.Env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS", fmt.Sprint(req.MaxTokens))
	}
	cmd.Env = removeEnv(cmd.Env, "CLAUDE_CODE_SIMPLE")
	cmd.Env = removeEnv(cmd.Env, "CLAUDE_CODE_EFFORT_LEVEL")
	cmd.Env = removeEnv(cmd.Env, "MCP_TOOL_TIMEOUT")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Claude Agent: %w", err)
	}
	r := &run{
		cmd: cmd, stdin: stdin, events: make(chan envelope, 64), done: make(chan error, 1),
		pending: make(map[string]pendingCall), results: make(map[string]model.Block), initialized: make(chan error, 1),
	}
	r.setTools(req.Tools)
	go r.read(stdout, &stderr)
	if err := r.initialize(ctx, req.SystemPrompt); err != nil {
		r.close()
		return nil, err
	}
	if err := r.writeJSON(map[string]any{
		"type": "user", "uuid": newID(), "priority": "next", "parent_tool_use_id": nil, "client_composed": true,
		"message": map[string]any{"role": "user", "content": initialPrompt(req.Messages)},
	}); err != nil {
		r.close()
		return nil, err
	}
	return r, nil
}

func (p *provider) stop() {
	if p.run != nil {
		p.run.close()
		p.run = nil
	}
}

func (r *run) initialize(ctx context.Context, system string) error {
	hooks := map[string]any{
		"PreToolUse":  []any{map[string]any{"matcher": "mcp__notch__*", "hookCallbackIds": []string{"notch-pre"}}},
		"PostToolUse": []any{map[string]any{"matcher": "mcp__notch__*", "hookCallbackIds": []string{"notch-post"}}},
	}
	request := map[string]any{"subtype": "initialize", "hooks": hooks, "sdkMcpServers": []string{serverName}, "skills": []string{}}
	if strings.TrimSpace(system) != "" {
		request["appendSystemPrompt"] = system
	}
	if err := r.writeJSON(map[string]any{"type": "control_request", "request_id": "notch-init", "request": request}); err != nil {
		return err
	}
	select {
	case err := <-r.initialized:
		return err
	case err := <-r.done:
		return r.exitError(err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *run) setTools(tools []model.ToolDefinition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = append([]model.ToolDefinition(nil), tools...)
}

func (r *run) read(stdout io.Reader, stderr *bytes.Buffer) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var msg envelope
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if msg.Type == "control_response" {
			var response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Error     string `json:"error"`
			}
			_ = json.Unmarshal(msg.Response, &response)
			if response.RequestID == "notch-init" {
				if response.Subtype == "success" {
					r.initialized <- nil
				} else {
					r.initialized <- fmt.Errorf("initialize Claude Agent: %s", response.Error)
				}
				continue
			}
		}
		if msg.Type == "control_request" {
			r.handleControl(msg)
			continue
		}
		r.events <- msg
	}
	err := scanner.Err()
	if err == nil {
		err = r.cmd.Wait()
	} else {
		_ = r.cmd.Wait()
	}
	if text := strings.TrimSpace(stderr.String()); err != nil && text != "" {
		err = fmt.Errorf("%w: %s", err, text)
	}
	r.done <- err
	close(r.events)
}

func (r *run) handleControl(msg envelope) {
	var request struct {
		Subtype    string          `json:"subtype"`
		CallbackID string          `json:"callback_id"`
		Input      json.RawMessage `json:"input"`
		ServerName string          `json:"server_name"`
		Message    json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(msg.Request, &request); err != nil {
		r.respondError(msg.RequestID, err.Error())
		return
	}
	switch request.Subtype {
	case "hook_callback":
		if request.CallbackID == "notch-pre" {
			var input struct {
				ToolUseID string `json:"tool_use_id"`
				ToolName  string `json:"tool_name"`
			}
			_ = json.Unmarshal(request.Input, &input)
			r.mu.Lock()
			r.toolQueue = append(r.toolQueue, toolIdentity{ID: input.ToolUseID, Name: input.ToolName})
			r.mu.Unlock()
		}
		r.respondSuccess(msg.RequestID, map[string]any{})
	case "mcp_message":
		r.handleMCP(msg.RequestID, request.ServerName, request.Message)
	default:
		r.respondError(msg.RequestID, "unsupported control request: "+request.Subtype)
	}
}

func (r *run) handleMCP(requestID, name string, raw json.RawMessage) {
	if name != serverName {
		r.respondError(requestID, "unknown MCP server: "+name)
		return
	}
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		r.respondError(requestID, err.Error())
		return
	}
	var result any
	switch message.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(message.Params, &params)
		if params.ProtocolVersion == "" {
			params.ProtocolVersion = "2025-06-18"
		}
		result = map[string]any{"protocolVersion": params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": serverName, "version": "1.0.0"}}
	case "notifications/initialized":
		r.respondSuccess(requestID, map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": map[string]any{}}})
		return
	case "tools/list":
		r.mu.Lock()
		tools := append([]model.ToolDefinition(nil), r.tools...)
		r.mu.Unlock()
		items := make([]map[string]any, 0, len(tools))
		for _, tool := range tools {
			description := truncateRunes(tool.Description, 2048)
			items = append(items, map[string]any{"name": shortToolName(tool.Name), "description": description, "inputSchema": tool.InputSchema, "annotations": map[string]any{"readOnlyHint": false}, "_meta": map[string]any{"anthropic/alwaysLoad": true, "anthropic/maxResultSizeChars": 500000}})
		}
		result = map[string]any{"tools": items}
	case "tools/call":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			r.respondError(requestID, "decode MCP tool call: "+err.Error())
			return
		}
		r.mu.Lock()
		registered := false
		for _, definition := range r.tools {
			if shortToolName(definition.Name) == params.Name {
				registered = true
				break
			}
		}
		if !registered {
			r.mu.Unlock()
			r.respondError(requestID, "Claude called an unregistered Notch tool: "+params.Name)
			return
		}
		if len(r.toolQueue) == 0 {
			r.mu.Unlock()
			r.respondError(requestID, "Claude invoked a Notch tool without a PreToolUse ID")
			return
		}
		tool := r.toolQueue[0]
		r.toolQueue = r.toolQueue[1:]
		if strings.TrimPrefix(tool.Name, "mcp__notch__") != params.Name {
			r.mu.Unlock()
			r.respondError(requestID, fmt.Sprintf("Claude tool call mismatch: hook named %q, MCP called %q", tool.Name, params.Name))
			return
		}
		if block, ok := r.results[tool.ID]; ok {
			delete(r.results, tool.ID)
			r.mu.Unlock()
			r.respondMCPResult(requestID, message.ID, block)
			return
		}
		r.pending[tool.ID] = pendingCall{RequestID: requestID, MessageID: message.ID}
		r.mu.Unlock()
		return
	default:
		r.respondSuccess(requestID, map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32601, "message": "method not found"}}})
		return
	}
	r.respondSuccess(requestID, map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result}})
}

func (r *run) resolveResults(messages []model.Message) error {
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type != "tool_result" && block.Type != "function_call_output" {
				continue
			}
			r.mu.Lock()
			call, ok := r.pending[block.ToolUseID]
			if ok {
				delete(r.pending, block.ToolUseID)
			} else {
				r.results[block.ToolUseID] = block
			}
			r.mu.Unlock()
			if ok {
				r.respondMCPResult(call.RequestID, call.MessageID, block)
			}
		}
	}
	return nil
}

func (r *run) respondMCPResult(requestID string, messageID any, block model.Block) {
	text := block.Text
	if text == "" {
		text = "Tool completed with no output."
	}
	result := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	if block.IsError {
		result["isError"] = true
	}
	r.respondSuccess(requestID, map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": messageID, "result": result}})
}

func (r *run) readStep(ctx context.Context, onEvent func(model.StreamEvent)) (model.Response, bool, error) {
	var response model.Response
	var content []model.Block
	var stepUsage usage
	seenUsage := make(map[string]bool)
	for {
		select {
		case msg, ok := <-r.events:
			if !ok {
				select {
				case err := <-r.done:
					return model.Response{}, true, r.exitError(err)
				default:
					return model.Response{}, true, errors.New("Claude Agent stopped before completing the turn")
				}
			}
			switch msg.Type {
			case "system":
				if msg.Subtype == "init" && msg.Version != "" && !versionAtLeast(msg.Version, "2.1.248") {
					return model.Response{}, true, fmt.Errorf("Claude Code %s is too old; version 2.1.248 or later is required; run `claude update`", msg.Version)
				}
			case "stream_event":
				var event struct {
					Type  string `json:"type"`
					Usage usage  `json:"usage"`
					Delta struct {
						Type     string `json:"type"`
						Text     string `json:"text"`
						Thinking string `json:"thinking"`
					} `json:"delta"`
				}
				_ = json.Unmarshal(msg.Event, &event)
				if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
					onEvent(model.StreamEvent{Type: "text_delta", Text: event.Delta.Text})
				}
				if event.Delta.Type == "thinking_delta" && event.Delta.Thinking != "" {
					onEvent(model.StreamEvent{Type: "reasoning_delta", Text: event.Delta.Thinking})
				}
				if event.Type == "message_delta" {
					stepUsage = event.Usage
				}
				if event.Type == "message_stop" && hasToolUse(content) {
					response.Content = append([]model.Block(nil), content...)
					response.StopReason = "tool_use"
					response.InputTokens = stepUsage.InputTokens
					response.OutputTokens = stepUsage.OutputTokens
					response.CacheReadTokens = stepUsage.CacheReadInputTokens
					response.CacheWriteTokens = stepUsage.CacheCreationInputTokens
					r.accounted.add(stepUsage)
					return response, false, nil
				}
			case "assistant":
				var message struct {
					ID      string `json:"id"`
					Content []struct {
						Type      string          `json:"type"`
						Text      string          `json:"text"`
						Thinking  string          `json:"thinking"`
						Signature string          `json:"signature"`
						ID        string          `json:"id"`
						Name      string          `json:"name"`
						Input     json.RawMessage `json:"input"`
					} `json:"content"`
					Usage usage `json:"usage"`
				}
				if err := json.Unmarshal(msg.Message, &message); err != nil {
					return model.Response{}, true, err
				}
				if !seenUsage[message.ID] {
					seenUsage[message.ID] = true
					stepUsage.add(message.Usage)
				}
				for _, block := range message.Content {
					switch block.Type {
					case "text":
						content = append(content, model.Block{Type: "text", Text: block.Text})
					case "thinking":
						content = append(content, model.Block{Type: "thinking", Text: block.Thinking, Signature: block.Signature})
					case "tool_use":
						short := strings.TrimPrefix(block.Name, "mcp__notch__")
						name := r.originalToolName(short)
						if name == "" {
							continue
						}
						content = append(content, model.Block{Type: "tool_use", ID: block.ID, Name: name, Arguments: block.Input})
					}
				}
			case "result":
				if msg.Subtype != "success" || msg.IsError || msg.APIErrorStatus != 0 {
					text := strings.TrimSpace(msg.Result)
					if text == "" {
						text = strings.Join(msg.Errors, "; ")
					}
					if text == "" && msg.APIErrorStatus != 0 {
						text = fmt.Sprintf("Claude Agent API request failed with HTTP %d", msg.APIErrorStatus)
					}
					if text == "" {
						text = "Claude Agent returned an error"
					}
					return model.Response{}, true, errors.New(text)
				}
				response.Content = append([]model.Block(nil), content...)
				if len(response.Content) == 0 && msg.Result != "" {
					response.Content = []model.Block{{Type: "text", Text: msg.Result}}
				}
				response.StopReason = msg.StopReason
				if response.StopReason == "" {
					response.StopReason = "end_turn"
				}
				finalUsage := msg.Usage.subtract(r.accounted)
				response.InputTokens = finalUsage.InputTokens
				response.OutputTokens = finalUsage.OutputTokens
				response.CacheReadTokens = finalUsage.CacheReadInputTokens
				response.CacheWriteTokens = finalUsage.CacheCreationInputTokens
				return response, true, nil
			}
		case <-ctx.Done():
			return model.Response{}, true, ctx.Err()
		}
	}
}

func (r *run) respondSuccess(requestID string, response any) {
	_ = r.writeJSON(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": requestID, "response": response}})
}

func (r *run) respondError(requestID, message string) {
	_ = r.writeJSON(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": requestID, "error": message}})
}

func (r *run) writeJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	r.write.Lock()
	defer r.write.Unlock()
	_, err = r.stdin.Write(append(data, '\n'))
	return err
}

func (r *run) close() {
	_ = r.stdin.Close()
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
}

func (r *run) exitError(err error) error {
	if err == nil {
		return errors.New("Claude Agent exited before completing the turn")
	}
	return fmt.Errorf("Claude Agent: %w", err)
}

func initialPrompt(messages []model.Message) string {
	if len(messages) == 0 {
		return "Continue."
	}
	last := messages[len(messages)-1]
	if len(messages) == 1 && last.Role == "user" {
		return blocksText(last.Content)
	}
	var out strings.Builder
	out.WriteString("Continue this Notch conversation. The transcript below is authoritative. Do not repeat completed work.\n\n<notch_transcript>\n")
	for _, message := range messages {
		fmt.Fprintf(&out, "[%s]\n%s\n", message.Role, blocksText(message.Content))
	}
	out.WriteString("</notch_transcript>")
	return out.String()
}

func blocksText(blocks []model.Block) string {
	var out strings.Builder
	for _, block := range blocks {
		switch block.Type {
		case "text":
			out.WriteString(block.Text)
		case "thinking":
			// Provider reasoning is intentionally not replayed as user-visible text.
		case "tool_use", "function_call":
			fmt.Fprintf(&out, "\nTool call %s (%s): %s", block.ID, block.Name, string(block.Arguments))
		case "tool_result", "function_call_output":
			status := ""
			if block.IsError {
				status = " (error)"
			}
			fmt.Fprintf(&out, "\nTool result%s for %s: %s", status, block.ToolUseID, block.Text)
		}
	}
	return out.String()
}

func hasToolUse(blocks []model.Block) bool {
	for _, block := range blocks {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

func versionAtLeast(value, minimum string) bool {
	parse := func(value string) [3]int {
		value = strings.SplitN(strings.SplitN(value, "+", 2)[0], "-", 2)[0]
		var result [3]int
		_, _ = fmt.Sscanf(value, "%d.%d.%d", &result[0], &result[1], &result[2])
		return result
	}
	current, required := parse(value), parse(minimum)
	for i := range current {
		if current[i] != required[i] {
			return current[i] > required[i]
		}
	}
	return true
}

func (r *run) originalToolName(short string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tool := range r.tools {
		if shortToolName(tool.Name) == short {
			return tool.Name
		}
	}
	return ""
}

func shortToolName(name string) string {
	const limit = 64 - len("mcp__notch__")
	valid := name != "" && len(name) <= limit
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			valid = false
			break
		}
	}
	if valid {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(sum[:])[:10]
	var cleaned strings.Builder
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			cleaned.WriteRune(char)
		} else {
			cleaned.WriteByte('_')
		}
		if cleaned.Len() >= limit-11 {
			break
		}
	}
	return cleaned.String() + "_" + hash
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func newID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "notch-message"
	}
	return hex.EncodeToString(data[:])
}

func setEnv(env []string, key, value string) []string {
	env = removeEnv(env, key)
	return append(env, key+"="+value)
}

func removeEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return out
}
