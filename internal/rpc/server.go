// Package rpc implements Notch's Pi-compatible JSONL control mode.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/resources"
)

const maxCommandSize = 16 << 20

var (
	errCommandTooLarge  = errors.New("RPC command exceeds 16 MiB")
	errUnterminatedLine = errors.New("RPC command is not terminated by LF")
)

// StateConfig describes static process/session state returned by get_state.
type StateConfig struct {
	Provider              string
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

	writeMu    sync.Mutex
	mu         sync.Mutex
	runner     *agent.Agent
	registry   *extension.Registry
	catalog    *resources.Catalog
	state      StateConfig
	active     bool
	compacting bool
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

func New(in io.Reader, out io.Writer, cwd string) *Server {
	return &Server{in: in, out: out, cwd: cwd}
}

func (s *Server) Configure(runner *agent.Agent, registry *extension.Registry, catalog *resources.Catalog, state StateConfig) {
	s.mu.Lock()
	s.runner, s.registry, s.catalog, s.state = runner, registry, catalog, state
	s.mu.Unlock()
}

func (s *Server) CWD() string { return s.cwd }

func (s *Server) Exec(ctx context.Context, command string, args []string) (string, string, int, error) {
	if strings.TrimSpace(command) == "" {
		return "", "", -1, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = s.cwd
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}

func (s *Server) Input(context.Context, string, string) (string, error) {
	return "", errors.New("interactive extension input is not supported in Notch RPC mode")
}

func (s *Server) Select(context.Context, string, []string) (string, error) {
	return "", errors.New("interactive extension selection is not supported in Notch RPC mode")
}

func (s *Server) Notify(message, level string) {
	if level == "" {
		level = "info"
	}
	_ = s.write(map[string]any{"type": "notification", "message": message, "level": level})
}

type command struct {
	ID                json.RawMessage   `json:"id,omitempty"`
	Type              string            `json:"type"`
	Message           string            `json:"message,omitempty"`
	StreamingBehavior string            `json:"streamingBehavior,omitempty"`
	Images            []json.RawMessage `json:"images,omitempty"`
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
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	configured := s.runner != nil && s.registry != nil
	s.mu.Unlock()
	if !configured {
		return errors.New("RPC server is not configured")
	}
	reader := bufio.NewReaderSize(s.in, 64*1024)
	for {
		line, err := readRecord(reader)
		if errors.Is(err, io.EOF) {
			s.stopActive()
			s.wg.Wait()
			return nil
		}
		if err != nil {
			_ = s.failure(nil, "parse", err)
			if errors.Is(err, errUnterminatedLine) {
				s.stopActive()
				s.wg.Wait()
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
	case "prompt":
		return s.handlePrompt(ctx, request)
	case "steer":
		return s.queue(request, "steer")
	case "follow_up":
		return s.queue(request, "followUp")
	case "abort":
		s.mu.Lock()
		cancel := s.cancel
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return s.success(request, nil)
	case "":
		return s.failure(request.ID, "parse", errors.New("command type is required"))
	default:
		return s.failure(request.ID, request.Type, fmt.Errorf("unknown command %q", request.Type))
	}
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
	if s.active {
		s.mu.Unlock()
		if request.StreamingBehavior == "" {
			return s.failure(request.ID, "prompt", errors.New("agent is streaming; set streamingBehavior to steer or followUp"))
		}
		return s.queue(request, request.StreamingBehavior)
	}
	promptCtx, cancel := context.WithCancel(ctx)
	s.active, s.cancel = true, cancel
	runner, state := s.runner, s.state
	s.mu.Unlock()

	started, release := make(chan struct{}), make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		adapter := newEventAdapter(s, state)
		err := runner.PromptWithStart(promptCtx, message, adapter.Handle, func() {
			close(started)
			<-release
			adapter.Start()
		})
		adapter.Finish(err)
		s.mu.Lock()
		s.active, s.compacting, s.cancel = false, false, nil
		s.mu.Unlock()
		adapter.Settled()
	}()
	select {
	case <-started:
		if err := s.success(request, nil); err != nil {
			close(release)
			return err
		}
		close(release)
		return nil
	case <-ctx.Done():
		close(release)
		return ctx.Err()
	}
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
	active, compacting := s.active, s.compacting
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

func (s *Server) setCompacting(value bool) {
	s.mu.Lock()
	s.compacting = value
	s.mu.Unlock()
}

func (s *Server) stopActive() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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
