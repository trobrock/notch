// Package subagent implements Notch's official run_subagent extension.
package subagent

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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/trobrock/notch/internal/delegation"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

const (
	Source                = "official:subagent"
	ToolName              = "run_subagent"
	DefaultThinking       = "low"
	defaultTools          = "read,grep,find,ls"
	defaultTimeoutSeconds = 300
	defaultMaxOutputChars = 12000
	maxOutputChars        = 50000
	maxTimeoutSeconds     = 3600
	maxEventLine          = 16 << 20
	defaultHeartbeat      = 10 * time.Second
)

var readOnlyTools = map[string]bool{"read": true, "grep": true, "find": true, "ls": true}

type Input struct {
	Prompt          string `json:"prompt"`
	Model           string `json:"model,omitempty"`
	CWD             string `json:"cwd,omitempty"`
	Tools           string `json:"tools,omitempty"`
	AllowWriteTools bool   `json:"allowWriteTools,omitempty"`
	TimeoutSeconds  int    `json:"timeoutSeconds,omitempty"`
	MaxOutputChars  int    `json:"maxOutputChars,omitempty"`
	SystemPrompt    string `json:"systemPrompt,omitempty"`
	Thinking        string `json:"thinking,omitempty"`
}

type Usage struct {
	Turns      int      `json:"turns"`
	ToolCalls  int      `json:"tool_calls,omitempty"`
	Input      int      `json:"input"`
	Output     int      `json:"output"`
	CacheRead  int      `json:"cache_read,omitempty"`
	CacheWrite int      `json:"cache_write,omitempty"`
	Reasoning  int      `json:"reasoning,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	WallMS     int64    `json:"wall_ms"`
	Model      string   `json:"model"`
}

type Result struct {
	Output   string `json:"output"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut"`
	Usage    Usage  `json:"usage"`
}

type Runner interface {
	Run(context.Context, Input, func(string)) (Result, error)
}

type processRunner struct {
	executable      string
	defaultCWD      string
	heartbeatPeriod time.Duration
	settingSources  string
}

// NewRunner creates a subprocess runner using the current Notch executable.
func NewRunner(host extension.Host) (Runner, error) {
	return NewRunnerWithSettingSources(host, "user,project")
}

func NewRunnerWithSettingSources(host extension.Host, settingSources string) (Runner, error) {
	if host == nil {
		return nil, errors.New("create subagent runner: host is required")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Notch executable: %w", err)
	}
	return &processRunner{executable: executable, defaultCWD: host.CWD(), settingSources: settingSources}, nil
}

// Register registers run_subagent using the current Notch executable.
func Register(registry *extension.Registry, host extension.Host) error {
	return RegisterWithSettingSources(registry, host, "user,project")
}

func RegisterWithSettingSources(registry *extension.Registry, host extension.Host, settingSources string) error {
	if registry == nil || host == nil {
		return errors.New("register subagent: registry and host are required")
	}
	runner, err := NewRunnerWithSettingSources(host, settingSources)
	if err != nil {
		return err
	}
	return RegisterWithRunner(registry, runner)
}

// RegisterWithRunner exposes runner injection for focused tests.
func RegisterWithRunner(registry *extension.Registry, runner Runner) error {
	if registry == nil || runner == nil {
		return errors.New("register subagent: registry and runner are required")
	}
	tool := extension.Tool{
		Source:     Source,
		UpdateMode: "replace",
		Definition: model.ToolDefinition{
			Name:        ToolName,
			Description: "Spawn a focused Notch subagent in an isolated process. Defaults to read-only tools; enable write-capable tools only when the user explicitly wants delegated implementation.",
			InputSchema: schema(),
		},
		Execute: func(ctx context.Context, raw json.RawMessage, update func(string)) (extension.ToolResult, error) {
			input, err := decodeInput(raw)
			if err != nil {
				return extension.ToolResult{}, err
			}
			result, err := runner.Run(ctx, input, update)
			if err != nil {
				return extension.ToolResult{}, err
			}
			content := result.Output
			if result.ExitCode != 0 {
				content = fmt.Sprintf("Subagent failed with exit %d.\n\n%s", result.ExitCode, result.Output)
			}
			delegated := delegatedUsage(result.Usage)
			details := map[string]any{
				"output": result.Output, "stderr": result.Stderr, "exitCode": result.ExitCode,
				"timedOut": result.TimedOut, "usage": result.Usage,
				"usageLine":       fmt.Sprintf("subagent usage: %s, %d turns, %d tool calls, in %d, out %d, %d ms", result.Usage.Model, result.Usage.Turns, result.Usage.ToolCalls, result.Usage.Input, result.Usage.Output, result.Usage.WallMS),
				"delegated_usage": delegated,
			}
			return extension.ToolResult{Content: content, IsError: result.ExitCode != 0, Details: details}, nil
		},
	}
	if err := registry.RegisterTool(tool); err != nil {
		return fmt.Errorf("register %s: %w", ToolName, err)
	}
	return nil
}

func schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt":          map[string]any{"type": "string", "minLength": 1, "description": "Self-contained task or question for the subagent."},
			"model":           map[string]any{"type": "string", "description": "Model ID, optionally provider/model. The parent agent supplies its current provider/model when omitted."},
			"cwd":             map[string]any{"type": "string", "description": "Working directory. Defaults to the parent working directory."},
			"tools":           map[string]any{"type": "string", "description": "Comma-separated tool allowlist. Defaults to read,grep,find,ls. Include write-capable tools only with allowWriteTools=true."},
			"allowWriteTools": map[string]any{"type": "boolean", "description": "Permit tools outside the read-only default set."},
			"timeoutSeconds":  map[string]any{"type": "integer", "minimum": 1, "maximum": maxTimeoutSeconds},
			"maxOutputChars":  map[string]any{"type": "integer", "minimum": 1000, "maximum": maxOutputChars},
			"systemPrompt":    map[string]any{"type": "string", "description": "Additional system instructions for the subagent."},
			"thinking":        map[string]any{"type": "string", "enum": []string{"off", "minimal", "low", "medium", "high", "xhigh"}},
		},
		"required": []string{"prompt"}, "additionalProperties": false,
	}
}

func decodeInput(raw json.RawMessage) (Input, error) {
	var input Input
	if len(raw) == 0 {
		return input, errors.New("arguments are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("decode arguments: %w", err)
	}
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" {
		return input, errors.New("prompt must not be empty")
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = defaultTimeoutSeconds
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > maxTimeoutSeconds {
		return input, fmt.Errorf("timeoutSeconds must be between 1 and %d", maxTimeoutSeconds)
	}
	if input.MaxOutputChars == 0 {
		input.MaxOutputChars = defaultMaxOutputChars
	}
	if input.MaxOutputChars < 1000 || input.MaxOutputChars > maxOutputChars {
		return input, fmt.Errorf("maxOutputChars must be between 1000 and %d", maxOutputChars)
	}
	if input.Tools == "" {
		input.Tools = defaultTools
	}
	tools, err := parseTools(input.Tools)
	if err != nil {
		return input, err
	}
	var unsafe []string
	for _, tool := range tools {
		if !readOnlyTools[tool] {
			unsafe = append(unsafe, tool)
		}
	}
	if len(unsafe) > 0 && !input.AllowWriteTools {
		return input, fmt.Errorf("tools include non-read-only tools (%s); set allowWriteTools only when delegated write access is intended", strings.Join(unsafe, ", "))
	}
	input.Tools = strings.Join(tools, ",")
	if input.Thinking == "" {
		input.Thinking = DefaultThinking
	}
	switch input.Thinking {
	case "off", "minimal", "low", "medium", "high", "xhigh":
	default:
		return input, fmt.Errorf("invalid thinking level %q", input.Thinking)
	}
	return input, nil
}

func parseTools(value string) ([]string, error) {
	seen := map[string]bool{}
	var tools []string
	for _, part := range strings.Split(value, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, errors.New("tool names must not be empty")
		}
		if !seen[name] {
			seen[name] = true
			tools = append(tools, name)
		}
	}
	sort.Strings(tools)
	return tools, nil
}

func (r *processRunner) Run(ctx context.Context, input Input, update func(string)) (Result, error) {
	cwd := input.CWD
	if cwd == "" {
		cwd = r.defaultCWD
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return Result{}, fmt.Errorf("subagent cwd %q: %w", cwd, err)
	}

	tempDir, err := os.MkdirTemp("", "notch-run-subagent-")
	if err != nil {
		return Result{}, fmt.Errorf("create subagent prompt directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	systemPath := filepath.Join(tempDir, "SYSTEM.md")
	if err := os.WriteFile(systemPath, []byte(baseSystemPrompt(input.SystemPrompt)), 0o600); err != nil {
		return Result{}, fmt.Errorf("write subagent system prompt: %w", err)
	}

	settingSources := r.settingSources
	if settingSources == "" {
		settingSources = "user,project"
	}
	args := []string{"--json", "--setting-sources", settingSources, "--no-session", "--no-tui", "--no-extensions", "--no-resources", "--tools", input.Tools,
		"--thinking", input.Thinking, "--system-prompt-file", systemPath}
	args = append(args, modelArgs(input.Model)...)
	args = append(args, "--print")
	if update != nil {
		modelName := input.Model
		if modelName == "" {
			modelName = "current model"
		}
		update("starting subagent with " + modelName)
	}

	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.Command(r.executable, args...)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(input.Prompt)
	configureProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start subagent: %w", err)
	}

	parsedCh := make(chan parsedEvents, 1)
	progressCh := make(chan childProgress, 1)
	go func() {
		parsedCh <- parseEvents(stdout, input.MaxOutputChars, func(progress childProgress) {
			publishLatestProgress(progressCh, progress)
		})
	}()
	heartbeatPeriod := r.heartbeatPeriod
	if heartbeatPeriod <= 0 {
		heartbeatPeriod = defaultHeartbeat
	}
	heartbeat := time.NewTicker(heartbeatPeriod)
	var parsed parsedEvents
	var progress childProgress
	streamDone := false
	terminated := false
	for !streamDone {
		select {
		case parsed = <-parsedCh:
			progress = parsed.progress()
			streamDone = true
		case progress = <-progressCh:
			if update != nil {
				update(formatChildProgress(progress, time.Since(start)))
			}
		case <-runCtx.Done():
			terminateProcessGroup(cmd)
			terminated = true
			parsed = <-parsedCh
			streamDone = true
		case <-heartbeat.C:
			if update != nil {
				update(formatChildProgress(progress, time.Since(start)))
			}
		}
	}
	heartbeat.Stop()

	// StdoutPipe requires all reads to finish before Wait closes the pipe. Waiting
	// first races the parser and can turn an otherwise successful child into an
	// "file already closed" event-stream failure under load or the race detector.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-runCtx.Done():
		if !terminated {
			terminateProcessGroup(cmd)
		}
		waitErr = <-waitCh
	}
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	wallMS := time.Since(start).Milliseconds()
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if errors.Is(runCtx.Err(), context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
		return Result{}, ctx.Err()
	}
	output := parsed.output
	stderrText := trimOutput(stderr.String(), input.MaxOutputChars)
	if output == "" {
		output = stderrText
	}
	if parsed.err != nil {
		if output == "" {
			output = "subagent event stream failed: " + parsed.err.Error()
		} else {
			output += "\n\nsubagent event stream failed: " + parsed.err.Error()
		}
		if exitCode == 0 {
			exitCode = 1
		}
	}
	if parsed.turns == 0 && exitCode == 0 && !timedOut {
		exitCode = 1
		if output == "" {
			output = "subagent exited without a completed model turn"
		} else {
			output = "subagent exited without a completed model turn\n\n" + output
		}
	}
	if output == "" {
		output = "(run_subagent returned no text)"
	}
	if timedOut {
		exitCode = 124
		output = fmt.Sprintf("Subagent timed out after %d seconds (%s).\n\n%s", input.TimeoutSeconds, formatChildActivity(parsed.progress()), output)
	}
	return Result{Output: output, Stderr: stderrText, ExitCode: exitCode, TimedOut: timedOut,
		Usage: Usage{
			Turns: parsed.turns, ToolCalls: parsed.toolCalls, Input: parsed.input, Output: parsed.outputTokens,
			CacheRead: parsed.cacheRead, CacheWrite: parsed.cacheWrite, Reasoning: parsed.reasoning,
			CostUSD: parsed.costUSD(), WallMS: wallMS, Model: input.Model,
		}}, nil
}

func delegatedUsage(usage Usage) delegation.Usage {
	return delegation.Usage{
		Turns: usage.Turns, InputTokens: usage.Input, OutputTokens: usage.Output,
		CacheReadTokens: usage.CacheRead, CacheWriteTokens: usage.CacheWrite,
		ReasoningTokens: usage.Reasoning, CostUSD: usage.CostUSD,
		WallMS: usage.WallMS, Calls: 1,
	}
}

func baseSystemPrompt(extra string) string {
	parts := []string{
		"You are a focused Notch subagent spawned by another coding agent.",
		"Complete only the delegated task and return a concise report to the parent agent.",
		"Include exact file paths, commands, and errors needed by the parent to continue.",
		"Do not ask follow-up questions unless the task is impossible without clarification.",
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, "\n")
}

func modelArgs(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	provider, name, found := strings.Cut(value, "/")
	if found && provider != "" && name != "" && !strings.Contains(provider, "/") {
		return []string{"--provider", provider, "--model", name}
	}
	return []string{"--model", value}
}

type childProgress struct {
	Turns     int
	ToolCalls int
	LastTool  string
}

type parsedEvents struct {
	output       string
	turns        int
	toolCalls    int
	lastTool     string
	input        int
	outputTokens int
	cacheRead    int
	cacheWrite   int
	reasoning    int
	cost         float64
	costTurns    int
	err          error
}

func (p parsedEvents) progress() childProgress {
	return childProgress{Turns: p.turns, ToolCalls: p.toolCalls, LastTool: p.lastTool}
}

func publishLatestProgress(ch chan childProgress, progress childProgress) {
	select {
	case ch <- progress:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- progress:
	default:
	}
}

func formatChildProgress(progress childProgress, elapsed time.Duration) string {
	parts := []string{"running"}
	if progress.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", progress.Turns, plural(progress.Turns, "turn", "turns")))
	}
	if progress.ToolCalls > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", progress.ToolCalls, plural(progress.ToolCalls, "tool call", "tool calls")))
	}
	if progress.LastTool != "" {
		parts = append(parts, "last: "+progress.LastTool)
	}
	parts = append(parts, elapsed.Round(time.Second).String()+" elapsed")
	return strings.Join(parts, " · ")
}

func formatChildActivity(progress childProgress) string {
	if progress.Turns == 0 && progress.ToolCalls == 0 {
		return "no completed turns or tool calls observed"
	}
	parts := []string{
		fmt.Sprintf("%d completed %s", progress.Turns, plural(progress.Turns, "turn", "turns")),
		fmt.Sprintf("%d %s", progress.ToolCalls, plural(progress.ToolCalls, "tool call", "tool calls")),
	}
	if progress.LastTool != "" {
		parts = append(parts, "last tool: "+progress.LastTool)
	}
	return strings.Join(parts, ", ")
}

func plural(value int, singular, plural string) string {
	if value == 1 {
		return singular
	}
	return plural
}

func (p parsedEvents) costUSD() *float64 {
	if p.turns == 0 || p.costTurns != p.turns {
		return nil
	}
	cost := p.cost
	return &cost
}

func parseEvents(reader io.Reader, limit int, onProgress func(childProgress)) parsedEvents {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxEventLine)
	var parsed parsedEvents
	for scanner.Scan() {
		var event struct {
			Type     string         `json:"type"`
			ToolName string         `json:"tool_name"`
			Message  *model.Message `json:"message"`
			Usage    *struct {
				InputTokens      int      `json:"input_tokens"`
				OutputTokens     int      `json:"output_tokens"`
				CacheReadTokens  int      `json:"cache_read_tokens"`
				CacheWriteTokens int      `json:"cache_write_tokens"`
				ReasoningTokens  int      `json:"reasoning_tokens"`
				CostUSD          *float64 `json:"cost_usd"`
			} `json:"usage"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Type == "tool_start" {
			parsed.toolCalls++
			parsed.lastTool = strings.TrimSpace(event.ToolName)
			if onProgress != nil {
				onProgress(parsed.progress())
			}
		}
		if event.Type == "turn_end" {
			parsed.turns++
			if event.Usage != nil {
				parsed.input += event.Usage.InputTokens
				parsed.outputTokens += event.Usage.OutputTokens
				parsed.cacheRead += event.Usage.CacheReadTokens
				parsed.cacheWrite += event.Usage.CacheWriteTokens
				parsed.reasoning += event.Usage.ReasoningTokens
				if event.Usage.CostUSD != nil {
					parsed.cost += *event.Usage.CostUSD
					parsed.costTurns++
				}
			}
			if event.Message != nil {
				if text := messageText(*event.Message); text != "" {
					parsed.output = trimOutput(text, limit)
				}
			}
			if onProgress != nil {
				onProgress(parsed.progress())
			}
		}
	}
	parsed.err = scanner.Err()
	return parsed
}

func messageText(message model.Message) string {
	var parts []string
	for _, block := range message.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func trimOutput(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	head := limit * 65 / 100
	tail := limit - head
	return strings.TrimSpace(string(runes[:head])) + "\n\n[run_subagent output truncated: " + strconv.Itoa(len(runes)) + " chars total]\n\n" + strings.TrimSpace(string(runes[len(runes)-tail:]))
}

func configureProcessGroup(cmd *exec.Cmd) { setProcessGroup(cmd) }

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	killProcessGroup(cmd)
}
