// Package rpc implements Notch's Pi-compatible JSONL control mode.
package rpc

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
	sharedprocess "github.com/trobrock/notch/internal/process"
	"github.com/trobrock/notch/internal/resources"
	"github.com/trobrock/notch/internal/session"
)

const maxCommandSize = 16 << 20

var (
	errCommandTooLarge  = errors.New("RPC command exceeds 16 MiB")
	errUnterminatedLine = errors.New("RPC command is not terminated by LF")
	inputNamespaceSeq   atomic.Uint64
)

// StateConfig describes static process/session state returned by get_state.
type StateConfig struct {
	Provider              string
	Profile               string
	Model                 string
	API                   string
	BaseURL               string
	ContextWindow         int
	MaxTokens             int
	SessionFile           string
	SessionID             string
	AutoCompactionEnabled bool
}

// Server is both an extension Host and a JSONL RPC server. Construct it before
// loading extensions, then call Configure once the agent and registry exist.
type Server struct {
	cwd string
	in  io.Reader
	out io.Writer

	writeMu          sync.Mutex
	mu               sync.Mutex
	runner           *agent.Agent
	registry         *extension.Registry
	catalog          *resources.Catalog
	session          *session.Session
	state            StateConfig
	active           bool
	settling         bool
	commandActive    bool
	commandExclusive bool
	commandCancel    context.CancelFunc
	asyncErr         error
	pending          []string
	compacting       bool
	cancel           context.CancelFunc
	closed           bool
	runCtx           context.Context
	runCancel        context.CancelFunc
	inputNamespace   string
	nextInputID      uint64
	inputs           map[string]*pendingInput
	wg               sync.WaitGroup
}

type pendingInput struct {
	ctx     context.Context
	kind    string
	options []string
	reply   chan inputResponse
}

type inputResponse struct {
	value string
	err   error
}

func New(in io.Reader, out io.Writer, cwd string) *Server {
	return &Server{in: in, out: out, cwd: cwd, inputNamespace: newInputNamespace()}
}

func newInputNamespace() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), inputNamespaceSeq.Add(1))
}

func (s *Server) Configure(runner *agent.Agent, registry *extension.Registry, catalog *resources.Catalog, state StateConfig) {
	s.mu.Lock()
	s.runner, s.registry, s.catalog, s.state = runner, registry, catalog, state
	s.mu.Unlock()
}

// SetSession supplies the current session used by extension persistence APIs.
func (s *Server) SetSession(current *session.Session) {
	s.mu.Lock()
	s.session = current
	s.mu.Unlock()
}

func (s *Server) CWD() string { return s.cwd }

func (s *Server) Exec(ctx context.Context, command string, args []string) (string, string, int, error) {
	return sharedprocess.Run(ctx, s.cwd, command, args)
}

func (s *Server) Input(ctx context.Context, prompt, placeholder string) (string, error) {
	return s.requestInput(ctx, "input", prompt, placeholder, nil)
}

func (s *Server) Select(ctx context.Context, prompt string, options []string) (string, error) {
	if len(options) == 0 {
		return "", errors.New("select requires options")
	}
	return s.requestInput(ctx, "select", prompt, "", append([]string(nil), options...))
}

func (s *Server) requestInput(ctx context.Context, kind, prompt, placeholder string, options []string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.runner == nil || s.registry == nil {
		s.mu.Unlock()
		return "", errors.New("interactive extension input is unavailable before the RPC server is configured")
	}
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("interactive extension input is unavailable after the RPC server closed")
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return "", err
	}

	s.nextInputID++
	requestID := fmt.Sprintf("input-%s-%d", s.inputNamespace, s.nextInputID)
	if s.inputs == nil {
		s.inputs = make(map[string]*pendingInput)
	}
	pending := &pendingInput{
		ctx: ctx, kind: kind, options: append([]string(nil), options...),
		reply: make(chan inputResponse, 1),
	}
	s.inputs[requestID] = pending
	sessionID := s.state.SessionID
	s.mu.Unlock()
	stopCancellation := context.AfterFunc(ctx, func() {
		if s.removeInput(requestID, pending) {
			pending.reply <- inputResponse{err: ctx.Err()}
		}
	})
	defer stopCancellation()

	event := map[string]any{
		"type": "input_request", "requestId": requestID, "kind": kind,
		"prompt": prompt,
	}
	if placeholder != "" {
		event["placeholder"] = placeholder
	}
	if len(options) != 0 {
		event["options"] = options
	}
	if sessionID != "" {
		event["sessionId"] = sessionID
	}
	if err := s.write(event); err != nil {
		s.removeInput(requestID, pending)
		return "", err
	}

	response := <-pending.reply
	return response.value, response.err
}

func (s *Server) removeInput(requestID string, pending *pendingInput) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inputs[requestID] != pending {
		return false
	}
	delete(s.inputs, requestID)
	return true
}

func (s *Server) Notify(message, level string) {
	if level == "" {
		level = "info"
	}
	_ = s.write(map[string]any{"type": "notification", "message": message, "level": level})
}

func (s *Server) FollowUp(message string) error {
	return s.followUpFor("", message)
}

// FollowUpFor queues a follow-up only while the expected durable session is
// still active.
func (s *Server) FollowUpFor(sessionID, message string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("follow-up session ID is empty")
	}
	return s.followUpFor(sessionID, message)
}

func (s *Server) followUpFor(expectedSessionID, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return errors.New("follow-up message is empty")
	}
	s.mu.Lock()
	runner, active := s.runner, s.active
	if runner == nil || s.closed {
		s.mu.Unlock()
		return errors.New("RPC server is not configured or has closed")
	}
	if expectedSessionID != "" && (s.session == nil || s.session.Header.ID != expectedSessionID) {
		s.mu.Unlock()
		return errors.New("active session changed before extension follow-up was delivered")
	}
	if s.settling || s.commandExclusive || (!active && len(s.pending) != 0) {
		s.pending = append(s.pending, message)
		s.startPendingLocked()
		s.mu.Unlock()
		return nil
	}
	if !active {
		if s.runCtx == nil || s.runCtx.Err() != nil {
			s.mu.Unlock()
			return errors.New("RPC server is not running")
		}
		_, release := s.startPromptLocked(s.runCtx, message)
		s.mu.Unlock()
		close(release)
		return nil
	}
	_, err := runner.FollowUp(message)
	if errors.Is(err, agent.ErrNotProcessing) {
		// The agent may have settled while RPC still considers its prompt
		// active. Preserve the wake-up for the prompt goroutine to run next.
		s.pending = append(s.pending, message)
		err = nil
	}
	s.mu.Unlock()
	return err
}

func (s *Server) Handoff(string, bool) error {
	return errors.New("extension handoff is unavailable in RPC mode")
}
func (s *Server) SetActiveTools(names []string) error {
	s.mu.Lock()
	registry := s.registry
	s.mu.Unlock()
	if registry == nil {
		return errors.New("RPC server is not configured")
	}
	if missing := registry.SetActiveTools(names); len(missing) > 0 {
		return fmt.Errorf("unknown tools: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (s *Server) SwitchModel(context.Context, string, string) (string, int, error) {
	return "", 0, errors.New("runtime model switching is unavailable in RPC mode")
}

func (s *Server) ListModels(context.Context, string, bool) ([]extension.ModelInfo, error) {
	return nil, errors.New("runtime model listing is unavailable in RPC mode")
}

func (s *Server) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return ""
	}
	return s.session.Header.ID
}

func (s *Server) AppendSessionEntry(expectedSessionID, kind string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.session
	if current == nil {
		return errors.New("session persistence is unavailable")
	}
	if expectedSessionID != "" && current.Header.ID != expectedSessionID {
		return errors.New("active session changed before extension entry was persisted")
	}
	return current.AppendCustomEntry(kind, data)
}

func (s *Server) SessionEntries(kind string) ([]json.RawMessage, error) {
	s.mu.Lock()
	current := s.session
	s.mu.Unlock()
	if current == nil {
		return nil, errors.New("session persistence is unavailable")
	}
	return current.CustomEntries(kind)
}

func (s *Server) EditorText(context.Context) (string, error) {
	return "", errors.New("prompt editor is unavailable in RPC mode")
}

func (s *Server) SetEditorText(context.Context, string) error {
	return errors.New("prompt editor is unavailable in RPC mode")
}

func (s *Server) SetStatus(key, value string) {
	_ = s.write(map[string]any{"type": "status", "key": key, "value": value})
}

func (s *Server) SetPanel(key, title string, lines []string) {
	_ = s.write(map[string]any{"type": "panel", "key": key, "title": title, "lines": lines})
}

type command struct {
	ID                json.RawMessage   `json:"id,omitempty"`
	Type              string            `json:"type"`
	Name              string            `json:"name,omitempty"`
	Args              string            `json:"args,omitempty"`
	Message           string            `json:"message,omitempty"`
	StreamingBehavior string            `json:"streamingBehavior,omitempty"`
	Images            []json.RawMessage `json:"images,omitempty"`
	RequestID         string            `json:"requestId,omitempty"`
	Value             string            `json:"value,omitempty"`
	Cancelled         bool              `json:"cancelled,omitempty"`
}

type response struct {
	ID      json.RawMessage `json:"id,omitempty"`
	Type    string          `json:"type"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    any             `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Run processes LF-delimited commands until EOF or a framing error. Prompt
// work runs asynchronously so state, steering, follow-up, and abort commands
// remain responsive while the provider or a tool is active.
func (s *Server) Run(ctx context.Context) (runErr error) {
	s.mu.Lock()
	configured := s.runner != nil && s.registry != nil
	if configured {
		s.runCtx, s.runCancel = context.WithCancel(ctx)
	}
	s.mu.Unlock()
	if !configured {
		return errors.New("RPC server is not configured")
	}
	// A live RPC stream owns an interruptible input (stdin or a pipe). Do not
	// leave command goroutines alive after cancellation or a broken output.
	stopInput := context.AfterFunc(s.runCtx, func() {
		if closer, ok := s.in.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	defer func() {
		stopInput()
		s.shutdown()
		s.wg.Wait()
		s.mu.Lock()
		if s.asyncErr != nil {
			runErr = s.asyncErr
		}
		s.mu.Unlock()
	}()
	reader := bufio.NewReaderSize(s.in, 64*1024)
	for {
		line, err := readRecord(reader)
		if s.runCtx.Err() != nil {
			return s.runCtx.Err()
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			_ = s.failure(nil, "parse", err)
			if errors.Is(err, errUnterminatedLine) {
				return nil
			}
			continue
		}
		var request command
		if err := json.Unmarshal(line, &request); err != nil {
			_ = s.failure(nil, "parse", fmt.Errorf("failed to parse command: %w", err))
			continue
		}
		if err := s.handle(ctx, request); err != nil {
			return err
		}
	}
}

func (s *Server) handle(ctx context.Context, request command) error {
	request.Type = strings.TrimSpace(request.Type)
	switch request.Type {
	case "get_state", "get_status", "status":
		return s.success(request, s.getState())
	case "get_tools":
		return s.success(request, s.getTools())
	case "extension_command":
		return s.handleExtensionCommand(ctx, request)
	case "prompt":
		return s.handlePrompt(ctx, request)
	case "steer":
		return s.queue(request, "steer")
	case "follow_up":
		return s.queue(request, "followUp")
	case "input_response":
		return s.handleInputResponse(request)
	case "abort":
		s.mu.Lock()
		cancel, commandCancel := s.cancel, s.commandCancel
		s.mu.Unlock()
		if commandCancel != nil {
			commandCancel()
		}
		if cancel != nil {
			cancel()
		}
		s.cancelInputs(context.Canceled)
		return s.success(request, nil)
	case "":
		return s.failure(request.ID, "parse", errors.New("command type is required"))
	default:
		return s.failure(request.ID, request.Type, fmt.Errorf("unknown command %q", request.Type))
	}
}

// Extension commands are user control messages, never model tool calls. This
// permits opt-in workflows in headless hosts without a workflow-specific API.
func (s *Server) handleExtensionCommand(ctx context.Context, request command) error {
	s.mu.Lock()
	registry := s.registry
	s.mu.Unlock()
	if registry == nil {
		return s.failure(request.ID, request.Type, errors.New("RPC server is unavailable"))
	}
	registered, ok := registry.Command(request.Name)
	if !ok {
		return s.failure(request.ID, request.Type, fmt.Errorf("unknown extension command %q", request.Name))
	}
	allowed := registered.AllowWhileStreaming || (registered.AllowWhileStreamingArgs != nil && registered.AllowWhileStreamingArgs(request.Args))
	s.mu.Lock()
	if s.closed || s.runCtx == nil {
		s.mu.Unlock()
		return s.failure(request.ID, request.Type, errors.New("RPC server is not running"))
	}
	if s.active && !allowed {
		s.mu.Unlock()
		return s.failure(request.ID, request.Type, errors.New("extension command is unavailable while streaming"))
	}
	if s.commandActive {
		s.mu.Unlock()
		return s.failure(request.ID, request.Type, errors.New("another extension command is running"))
	}
	commandCtx, cancel := context.WithCancel(s.runCtx)
	s.commandActive, s.commandExclusive, s.commandCancel = true, !allowed, cancel
	s.wg.Add(1)
	s.mu.Unlock()
	// Reserve commands before launching so prompts cannot bypass their streaming
	// policy. Input responses and state queries still run on the reader loop.
	go func() {
		defer s.wg.Done()
		defer cancel()
		content, err := registered.Execute(commandCtx, request.Args)
		s.mu.Lock()
		s.commandActive, s.commandExclusive, s.commandCancel = false, false, nil
		s.mu.Unlock()
		var writeErr error
		if err != nil {
			writeErr = s.failure(request.ID, request.Type, err)
		} else {
			writeErr = s.success(request, map[string]any{"content": content})
		}
		if writeErr != nil {
			s.failAsync(writeErr)
		}
		s.mu.Lock()
		s.startPendingLocked()
		s.mu.Unlock()
	}()
	return nil
}

func (s *Server) handleInputResponse(request command) error {
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.RequestID == "" {
		return s.failure(request.ID, request.Type, errors.New("requestId is required"))
	}
	s.mu.Lock()
	pending := s.inputs[request.RequestID]
	if pending == nil {
		s.mu.Unlock()
		return s.failure(request.ID, request.Type, fmt.Errorf("input request %q is not pending", request.RequestID))
	}
	if err := pending.ctx.Err(); err != nil {
		delete(s.inputs, request.RequestID)
		s.mu.Unlock()
		pending.reply <- inputResponse{err: err}
		return s.failure(request.ID, request.Type, fmt.Errorf("input request %q is not pending", request.RequestID))
	}
	if pending.kind == "select" && !request.Cancelled && !containsString(pending.options, request.Value) {
		s.mu.Unlock()
		return s.failure(request.ID, request.Type, fmt.Errorf("input response value is not one of the pending options"))
	}
	delete(s.inputs, request.RequestID)
	s.mu.Unlock()
	response := inputResponse{value: request.Value}
	if request.Cancelled {
		response.err = context.Canceled
	}
	pending.reply <- response
	return s.success(request, nil)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *Server) handlePrompt(ctx context.Context, request command) error {
	if len(request.Images) != 0 {
		return s.failure(request.ID, "prompt", errors.New("image prompts are not supported in Notch RPC mode"))
	}
	message, err := s.expand(request.Message)
	if err != nil {
		return s.failure(request.ID, "prompt", err)
	}
	if strings.TrimSpace(message) == "" {
		return s.failure(request.ID, "prompt", errors.New("prompt message is required"))
	}

	s.mu.Lock()
	if s.commandExclusive {
		s.mu.Unlock()
		return s.failure(request.ID, "prompt", errors.New("an exclusive extension command is running"))
	}
	// Work accepted during an exclusive command must not be overtaken in the
	// brief gap between command completion and its asynchronous response.
	s.startPendingLocked()
	if s.active {
		s.mu.Unlock()
		if request.StreamingBehavior == "" {
			return s.failure(request.ID, "prompt", errors.New("agent is streaming; set streamingBehavior to steer or followUp"))
		}
		return s.queue(request, request.StreamingBehavior)
	}
	if s.closed {
		s.mu.Unlock()
		return s.failure(request.ID, "prompt", errors.New("RPC server has closed"))
	}
	started, release := s.startPromptLocked(ctx, message)
	s.mu.Unlock()
	select {
	case <-started:
		err := s.success(request, nil)
		close(release)
		return err
	case <-ctx.Done():
		close(release)
		return ctx.Err()
	}
}

// startPromptLocked reserves the idle runner before unlocking. Idle extension
// wakeups use the same run lifecycle as an RPC prompt, without a fake response.
func (s *Server) startPromptLocked(ctx context.Context, message string) (<-chan struct{}, chan struct{}) {
	promptCtx, cancel := context.WithCancel(ctx)
	s.active, s.cancel = true, cancel
	runner, state := s.runner, s.state
	started, release := make(chan struct{}), make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		adapter := newEventAdapter(s, state)
		prompt := message
		var first sync.Once
		for {
			err := runner.PromptWithStart(promptCtx, prompt, adapter.Handle, func() {
				first.Do(func() { close(started); <-release; adapter.Start() })
			})
			adapter.Finish(err)
			s.mu.Lock()
			if err != nil {
				s.pending = nil
			}
			if len(s.pending) == 0 || err != nil || s.closed {
				s.settling = true
				s.mu.Unlock()
				adapter.Settled()
				s.mu.Lock()
				s.settling = false
				s.active, s.compacting, s.cancel = false, false, nil
				s.startPendingLocked()
				s.mu.Unlock()
				break
			}
			prompt, s.pending = s.pending[0], s.pending[1:]
			s.mu.Unlock()
			adapter = newEventAdapter(s, state)
			adapter.Start()
		}
	}()
	return started, release
}

// Start accepted wakeups only after exclusive user work or terminal events
// finish. A fresh context also keeps a late wake from inheriting an abort.
func (s *Server) startPendingLocked() {
	if s.closed || s.active || s.commandExclusive || s.runCtx == nil || s.runCtx.Err() != nil || len(s.pending) == 0 {
		return
	}
	prompt := s.pending[0]
	s.pending = s.pending[1:]
	_, release := s.startPromptLocked(s.runCtx, prompt)
	close(release)
}

func (s *Server) queue(request command, behavior string) error {
	if len(request.Images) != 0 {
		return s.failure(request.ID, request.Type, errors.New("queued images are not supported in Notch RPC mode"))
	}
	message, err := s.expand(request.Message)
	if err != nil {
		return s.failure(request.ID, request.Type, err)
	}
	s.mu.Lock()
	runner, active := s.runner, s.active
	s.mu.Unlock()
	if !active {
		return s.failure(request.ID, request.Type, errors.New("agent is not streaming"))
	}
	switch behavior {
	case "steer":
		_, err = runner.Steer(message)
	case "followUp", "follow_up":
		_, err = runner.FollowUp(message)
	default:
		err = fmt.Errorf("invalid streamingBehavior %q", behavior)
	}
	if err != nil {
		return s.failure(request.ID, request.Type, err)
	}
	return s.success(request, nil)
}

func (s *Server) expand(message string) (string, error) {
	s.mu.Lock()
	catalog := s.catalog
	s.mu.Unlock()
	if catalog == nil {
		return message, nil
	}
	expanded, err := catalog.ExpandInput(message)
	if err != nil && strings.HasPrefix(strings.TrimSpace(message), "/") {
		return "", err
	}
	if err != nil {
		return message, nil
	}
	return expanded, nil
}

func (s *Server) getState() map[string]any {
	s.mu.Lock()
	runner, registry, state := s.runner, s.registry, s.state
	active, compacting := s.active && !s.settling, s.compacting
	s.mu.Unlock()
	_, steering, followUps := runner.QueueStatus()
	tools := make([]string, 0)
	if registry != nil {
		for _, tool := range registry.Tools() {
			tools = append(tools, tool.Definition.Name)
		}
	}
	model := map[string]any{
		"id": state.Model, "name": state.Model, "api": state.API, "provider": state.Provider,
		"baseUrl": state.BaseURL, "reasoning": true, "input": []string{"text"},
		"contextWindow": state.ContextWindow, "maxTokens": state.MaxTokens,
		"cost": map[string]float64{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
	}
	data := map[string]any{
		"model": model, "thinkingLevel": runner.ThinkingLevel(), "isStreaming": active,
		"isCompacting": compacting, "steeringMode": "one-at-a-time", "followUpMode": "one-at-a-time",
		"autoCompactionEnabled": state.AutoCompactionEnabled, "messageCount": runner.MessageCount(),
		"pendingMessageCount": len(steering) + len(followUps), "tools": tools,
	}
	if state.Profile != "" {
		data["profile"] = state.Profile
	}
	if state.SessionFile != "" {
		data["sessionFile"] = state.SessionFile
	}
	if state.SessionID != "" {
		data["sessionId"] = state.SessionID
	}
	return data
}

func (s *Server) getTools() map[string]any {
	s.mu.Lock()
	registry := s.registry
	s.mu.Unlock()
	items := make([]map[string]string, 0)
	if registry != nil {
		for _, tool := range registry.Tools() {
			items = append(items, map[string]string{"name": tool.Definition.Name, "description": tool.Definition.Description, "source": tool.Source})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["name"] < items[j]["name"] })
	return map[string]any{"tools": items}
}

func (s *Server) success(request command, data any) error {
	return s.write(response{ID: request.ID, Type: "response", Command: request.Type, Success: true, Data: data})
}

func (s *Server) failure(id json.RawMessage, commandName string, err error) error {
	return s.write(response{ID: id, Type: "response", Command: commandName, Success: false, Error: err.Error()})
}

func (s *Server) write(value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	encoder := json.NewEncoder(s.out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func (s *Server) failAsync(err error) {
	s.mu.Lock()
	if s.asyncErr == nil {
		s.asyncErr = err
	}
	if s.runCancel != nil {
		s.runCancel()
	}
	s.mu.Unlock()
}

func (s *Server) setCompacting(value bool) {
	s.mu.Lock()
	s.compacting = value
	s.mu.Unlock()
}

func (s *Server) cancelInputs(err error) {
	s.mu.Lock()
	pending := make([]*pendingInput, 0, len(s.inputs))
	for requestID, input := range s.inputs {
		pending = append(pending, input)
		delete(s.inputs, requestID)
	}
	s.mu.Unlock()
	for _, input := range pending {
		input.reply <- inputResponse{err: err}
	}
}

func (s *Server) shutdown() {
	s.mu.Lock()
	s.closed = true
	if s.runCancel != nil {
		s.runCancel()
	}
	cancel, commandCancel := s.cancel, s.commandCancel
	s.mu.Unlock()
	if commandCancel != nil {
		commandCancel()
	}
	if cancel != nil {
		cancel()
	}
	s.cancelInputs(context.Canceled)
}

func readRecord(reader *bufio.Reader) ([]byte, error) {
	var record []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(record)+len(fragment) > maxCommandSize {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = reader.ReadSlice('\n')
			}
			return nil, errCommandTooLarge
		}
		record = append(record, fragment...)
		if err == nil {
			record = record[:len(record)-1]
			if len(record) != 0 && record[len(record)-1] == '\r' {
				record = record[:len(record)-1]
			}
			return record, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(record) == 0 {
				return nil, io.EOF
			}
			return nil, errUnterminatedLine
		}
		return nil, err
	}
}
