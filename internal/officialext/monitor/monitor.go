// Package monitor implements official background process monitor extensions.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

const (
	Source       = "official:monitor"
	maxOutput    = 16000
	maxCompleted = 25
)

type state struct {
	host     extension.Host
	cwd      string
	mu       sync.Mutex
	next     int
	monitors map[string]*Monitor
}

type Monitor struct {
	ID, Name, Command, Trigger, Status, Pattern, Prompt string
	StartedAt                                           time.Time
	CompletedAt                                         time.Time
	ExitCode                                            int
	Output                                              string
	DeliveryError                                       string
	cmd                                                 *exec.Cmd
	cancel                                              context.CancelFunc
	triggered                                           bool
	stopOnTrigger                                       bool
}

type commandInput struct {
	Command        string `json:"command"`
	Name           string `json:"name,omitempty"`
	Trigger        string `json:"trigger,omitempty"`
	Pattern        string `json:"pattern,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	StopOnTrigger  bool   `json:"stopOnTrigger,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
}

func Register(registry *extension.Registry, host extension.Host) error {
	if registry == nil || host == nil {
		return errors.New("register monitor: registry and host are required")
	}
	s := &state{host: host, cwd: host.CWD(), monitors: map[string]*Monitor{}}
	for _, tool := range []extension.Tool{s.commandTool(), s.githubTool(), s.listTool(), s.stopTool()} {
		if err := registry.RegisterTool(tool); err != nil {
			return fmt.Errorf("register monitor: %w", err)
		}
	}
	registry.On("session_shutdown", Source, func(context.Context, map[string]any) (map[string]any, error) { s.close(); return nil, nil })
	return nil
}

func (s *state) commandTool() extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{
		Name: "monitor_command", Description: "Run a long-running shell command in the background and notify the agent when it exits, times out, or output matches a regex.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"command": map[string]any{"type": "string", "minLength": 1}, "name": map[string]any{"type": "string"},
			"trigger": map[string]any{"type": "string", "enum": []string{"exit", "output_match", "timeout"}},
			"pattern": map[string]any{"type": "string"}, "timeoutSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400},
			"stopOnTrigger": map[string]any{"type": "boolean"}, "prompt": map[string]any{"type": "string"},
		}, "required": []string{"command"}, "additionalProperties": false},
	}, Execute: func(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		input, err := decodeCommand(raw)
		if err != nil {
			return extension.ToolResult{}, err
		}
		monitor, err := s.start(input)
		if err != nil {
			return extension.ToolResult{}, err
		}
		s.mu.Lock()
		id, name, status, command, trigger := monitor.ID, monitor.Name, monitor.Status, monitor.Command, monitor.Trigger
		s.mu.Unlock()
		return extension.ToolResult{Content: fmt.Sprintf("Started monitor %s: %s. The agent will be notified when %s triggers.", id, name, trigger), Details: map[string]any{"id": id, "status": status, "command": command, "trigger": trigger}}, nil
	}}
}

func (s *state) githubTool() extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{
		Name: "monitor_github_pr_checks", Description: "Watch GitHub PR checks with gh and notify the agent when checks pass or fail.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"pr":   map[string]any{"type": "string", "description": "PR number, branch, or URL. Defaults to current branch."},
			"repo": map[string]any{"type": "string", "description": "Repository in OWNER/REPO form."},
			"name": map[string]any{"type": "string"}, "requiredOnly": map[string]any{"type": "boolean"},
			"failFast": map[string]any{"type": "boolean"}, "intervalSeconds": map[string]any{"type": "integer", "minimum": 10, "maximum": 3600},
			"prompt": map[string]any{"type": "string"},
		}, "additionalProperties": false},
	}, Execute: func(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		var input struct {
			PR              string `json:"pr"`
			Repo            string `json:"repo"`
			Name            string `json:"name"`
			RequiredOnly    bool   `json:"requiredOnly"`
			FailFast        bool   `json:"failFast"`
			IntervalSeconds int    `json:"intervalSeconds"`
			Prompt          string `json:"prompt"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return extension.ToolResult{}, err
		}
		if input.IntervalSeconds == 0 {
			input.IntervalSeconds = 30
		}
		if input.IntervalSeconds < 10 || input.IntervalSeconds > 3600 {
			return extension.ToolResult{}, errors.New("intervalSeconds must be between 10 and 3600")
		}
		args := []string{"pr", "checks"}
		if input.PR != "" {
			args = append(args, input.PR)
		}
		args = append(args, "--watch", "--interval", fmt.Sprint(input.IntervalSeconds))
		if input.RequiredOnly {
			args = append(args, "--required")
		}
		if input.FailFast {
			args = append(args, "--fail-fast")
		}
		if input.Repo != "" {
			args = append(args, "--repo", input.Repo)
		}
		name := input.Name
		if name == "" {
			name = "PR checks"
			if input.PR != "" {
				name += " " + input.PR
			}
		}
		prompt := input.Prompt
		if prompt == "" {
			prompt = "If checks failed, inspect the failure and fix it. If checks passed, summarize the status."
		}
		monitor, err := s.startArgv(name, "gh", args, prompt)
		if err != nil {
			return extension.ToolResult{}, err
		}
		s.mu.Lock()
		id, monitorName, status, command := monitor.ID, monitor.Name, monitor.Status, monitor.Command
		s.mu.Unlock()
		return extension.ToolResult{Content: fmt.Sprintf("Started GitHub PR checks monitor %s: %s.", id, monitorName), Details: map[string]any{"id": id, "status": status, "command": command}}, nil
	}}
}

func (s *state) listTool() extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "list_monitors", Description: "List active and recently completed monitors. Use when the user asks for monitor status; do not poll repeatedly.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"includeOutput": map[string]any{"type": "boolean"}}, "additionalProperties": false}}, Execute: func(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		var input struct {
			IncludeOutput bool `json:"includeOutput"`
		}
		if len(raw) > 0 {
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&input); err != nil {
				return extension.ToolResult{}, err
			}
		}
		return extension.ToolResult{Content: s.list(input.IncludeOutput)}, nil
	}}
}

func (s *state) stopTool() extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "stop_monitor", Description: "Stop a running monitor by ID.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"id"}, "additionalProperties": false}}, Execute: func(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		var input struct {
			ID string `json:"id"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return extension.ToolResult{}, err
		}
		if err := s.stop(input.ID); err != nil {
			return extension.ToolResult{}, err
		}
		return extension.ToolResult{Content: "Stopped monitor " + input.ID, Details: map[string]any{"id": input.ID}}, nil
	}}
}

func decodeCommand(raw json.RawMessage) (commandInput, error) {
	var input commandInput
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		return input, err
	}
	input.Command = strings.TrimSpace(input.Command)
	if input.Command == "" {
		return input, errors.New("command must not be empty")
	}
	if input.Trigger == "" {
		input.Trigger = "exit"
	}
	if input.Trigger != "exit" && input.Trigger != "output_match" && input.Trigger != "timeout" {
		return input, fmt.Errorf("invalid trigger %q", input.Trigger)
	}
	if input.Trigger == "output_match" && input.Pattern == "" {
		return input, errors.New("pattern is required for output_match")
	}
	if input.Pattern != "" {
		if _, err := regexp.Compile(input.Pattern); err != nil {
			return input, fmt.Errorf("invalid pattern: %w", err)
		}
	}
	if input.Trigger == "timeout" && input.TimeoutSeconds < 1 {
		return input, errors.New("timeoutSeconds is required for timeout")
	}
	if input.TimeoutSeconds < 0 || input.TimeoutSeconds > 86400 {
		return input, errors.New("timeoutSeconds must be between 1 and 86400")
	}
	return input, nil
}

func (s *state) startArgv(name, command string, args []string, prompt string) (*Monitor, error) {
	s.mu.Lock()
	s.next++
	id := fmt.Sprintf("mon-%d", s.next)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = s.cwd
	var output lockedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start monitor: %w", err)
	}
	m := &Monitor{ID: id, Name: name, Command: strings.Join(append([]string{command}, args...), " "), Trigger: "exit", Prompt: prompt, Status: "running", StartedAt: time.Now(), cmd: cmd, cancel: cancel}
	s.mu.Lock()
	s.monitors[id] = m
	s.mu.Unlock()
	s.render()
	go s.watch(m, &output, 0)
	return m, nil
}

func (s *state) start(input commandInput) (*Monitor, error) {
	s.mu.Lock()
	s.next++
	id := fmt.Sprintf("mon-%d", s.next)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := shellCommand(ctx, input.Command)
	cmd.Dir = s.cwd
	var output lockedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start monitor: %w", err)
	}
	m := &Monitor{ID: id, Name: input.Name, Command: input.Command, Trigger: input.Trigger, Pattern: input.Pattern, Prompt: input.Prompt, Status: "running", StartedAt: time.Now(), cmd: cmd, cancel: cancel, stopOnTrigger: input.StopOnTrigger}
	if m.Name == "" {
		m.Name = input.Command
	}
	s.mu.Lock()
	s.monitors[id] = m
	s.mu.Unlock()
	s.render()
	go s.watch(m, &output, input.TimeoutSeconds)
	return m, nil
}

func (s *state) watch(m *Monitor, output *lockedBuffer, timeoutSeconds int) {
	done := make(chan error, 1)
	go func() { done <- m.cmd.Wait() }()
	var timer <-chan time.Time
	if timeoutSeconds > 0 {
		t := time.NewTimer(time.Duration(timeoutSeconds) * time.Second)
		defer t.Stop()
		timer = t.C
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			s.finish(m, err, output.String(), "process exited")
			return
		case <-ticker.C:
			if m.Trigger == "output_match" && !m.triggered {
				matched, _ := regexp.MatchString(m.Pattern, output.String())
				if matched {
					s.trigger(m, "output matched /"+m.Pattern+"/", output.String())
					if m.stopOnTrigger {
						m.cancel()
					}
				}
			}
		case <-timer:
			s.trigger(m, fmt.Sprintf("timeout after %d seconds", timeoutSeconds), output.String())
			if m.stopOnTrigger {
				m.cancel()
			}
			timer = nil
		}
	}
}

func (s *state) trigger(m *Monitor, reason, output string) {
	s.mu.Lock()
	if m.triggered {
		s.mu.Unlock()
		return
	}
	m.triggered = true
	if m.Trigger != "exit" {
		m.Status = "triggered"
	}
	s.mu.Unlock()
	s.wakeup(m, reason, output)
	s.render()
}
func (s *state) finish(m *Monitor, err error, output, reason string) {
	s.mu.Lock()
	m.CompletedAt = time.Now()
	m.Output = tail(output, maxOutput)
	m.ExitCode = 0
	if err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) {
			m.ExitCode = e.ExitCode()
		} else {
			m.ExitCode = 1
		}
	}
	if m.Status != "cancelled" {
		if m.ExitCode == 0 {
			m.Status = "completed"
		} else {
			m.Status = "failed"
		}
	}
	notify := !m.triggered && (m.Trigger == "exit" || m.ExitCode != 0)
	m.triggered = m.triggered || notify
	s.pruneLocked()
	s.mu.Unlock()
	if notify {
		s.wakeup(m, reason, m.Output)
	}
	s.render()
}
func (s *state) wakeup(m *Monitor, reason, output string) {
	message := fmt.Sprintf("Monitor triggered: %s\n\nReason: %s\nCommand: %s\nStatus: %s\nExit code: %d\n\nRecent output:\n%s", m.Name, reason, m.Command, m.Status, m.ExitCode, tail(output, 12000))
	if m.Prompt != "" {
		message += "\n\nRequested follow-up:\n" + m.Prompt
	}
	err := s.host.FollowUp(message)
	s.mu.Lock()
	if err != nil {
		m.DeliveryError = err.Error()
	} else {
		m.DeliveryError = ""
	}
	s.mu.Unlock()
	if err != nil {
		s.host.Notify(fmt.Sprintf("monitor %s completed but could not wake the agent: %v", m.ID, err), "warning")
	}
}
func (s *state) stop(id string) error {
	s.mu.Lock()
	m := s.monitors[id]
	if m == nil {
		s.mu.Unlock()
		return fmt.Errorf("monitor %s not found", id)
	}
	if m.Status != "running" {
		s.mu.Unlock()
		return fmt.Errorf("monitor %s is not running", id)
	}
	m.Status = "cancelled"
	m.CompletedAt = time.Now()
	s.mu.Unlock()
	m.cancel()
	s.render()
	return nil
}
func (s *state) list(include bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.monitors) == 0 {
		return "No monitors."
	}
	items := make([]*Monitor, 0, len(s.monitors))
	for _, m := range s.monitors {
		items = append(items, m)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StartedAt.After(items[j].StartedAt) })
	var parts []string
	for _, m := range items {
		line := fmt.Sprintf("%s [%s] %s", m.ID, m.Status, m.Name)
		if m.DeliveryError != "" {
			line += "\nWake-up delivery failed: " + m.DeliveryError
		}
		if include {
			line += "\nCommand: " + m.Command + "\nRecent output:\n" + m.Output
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n\n")
}
func (s *state) render() {
	s.mu.Lock()
	var lines []string
	for _, m := range s.monitors {
		if m.Status == "running" {
			lines = append(lines, "● "+m.ID+" "+m.Name)
		}
	}
	sort.Strings(lines)
	s.mu.Unlock()
	if len(lines) == 0 {
		s.host.SetStatus("monitor", "")
		s.host.SetPanel("monitor", "", nil)
	} else {
		s.host.SetStatus("monitor", fmt.Sprintf("monitors: %d", len(lines)))
		s.host.SetPanel("monitor", "Monitors", lines)
	}
}
func (s *state) close() {
	s.mu.Lock()
	var active []*Monitor
	for _, m := range s.monitors {
		if m.Status == "running" {
			m.Status = "cancelled"
			active = append(active, m)
		}
	}
	s.mu.Unlock()
	for _, m := range active {
		m.cancel()
	}
	s.host.SetStatus("monitor", "")
	s.host.SetPanel("monitor", "", nil)
}
func (s *state) pruneLocked() {
	var done []*Monitor
	for _, m := range s.monitors {
		if m.Status != "running" {
			done = append(done, m)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].CompletedAt.After(done[j].CompletedAt) })
	if len(done) <= maxCompleted {
		return
	}
	for _, m := range done[maxCompleted:] {
		delete(s.monitors, m.ID)
	}
}
func tail(value string, limit int) string {
	r := []rune(value)
	if len(r) > limit {
		r = r[len(r)-limit:]
	}
	return strings.TrimSpace(string(r))
}
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd.exe", "/C", command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }
