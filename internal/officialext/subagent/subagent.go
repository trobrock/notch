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

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

const (
	Source                = "official:subagent"
	ToolName              = "run_subagent"
	defaultTools          = "read,grep,find,ls"
	defaultTimeoutSeconds = 300
	defaultMaxOutputChars = 12000
	maxOutputChars        = 50000
	maxTimeoutSeconds     = 3600
	maxEventLine          = 16 << 20
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
	Turns  int    `json:"turns"`
	Input  int    `json:"input"`
	Output int    `json:"output"`
	Model  string `json:"model"`
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
	executable string
	defaultCWD string
}

// Register registers run_subagent using the current Notch executable.
func Register(registry *extension.Registry, host extension.Host) error {
	if registry == nil || host == nil {
		return errors.New("register subagent: registry and host are required")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Notch executable: %w", err)
	}
	return RegisterWithRunner(registry, &processRunner{executable: executable, defaultCWD: host.CWD()})
}

// RegisterWithRunner exposes runner injection for focused tests.
func RegisterWithRunner(registry *extension.Registry, runner Runner) error {
	if registry == nil || runner == nil {
		return errors.New("register subagent: registry and runner are required")
	}
	tool := extension.Tool{
		Source: Source,
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
			details := map[string]any{
				"output": result.Output, "stderr": result.Stderr, "exitCode": result.ExitCode,
				"timedOut": result.TimedOut, "usage": result.Usage,
				"usageLine": fmt.Sprintf("subagent usage: %s, in %d, out %d", result.Usage.Model, result.Usage.Input, result.Usage.Output),
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
			"model":           map[string]any{"type": "string", "description": "Model ID, optionally provider/model. Defaults to current configuration."},
			"cwd":             map[string]any{"type": "string", "description": "Working directory. Defaults to the parent working directory."},
			"tools":           map[string]any{"type": "string", "description": "Comma-separated tool allowlist. Defaults to read,grep,find,ls."},
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
		input.Thinking = "minimal"
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

	args := []string{"--json", "--no-session", "--no-tui", "--no-extensions", "--no-resources", "--tools", input.Tools,
		"--thinking", input.Thinking, "--system-prompt-file", systemPath}
	args = append(args, modelArgs(input.Model)...)
	args = append(args, "--print", input.Prompt)
	if update != nil {
		modelName := input.Model
		if modelName == "" {
			modelName = "current model"
		}
		update("starting subagent with " + modelName)
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.Command(r.executable, args...)
	cmd.Dir = cwd
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
	go func() { parsedCh <- parseEvents(stdout, input.MaxOutputChars) }()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-runCtx.Done():
		terminateProcessGroup(cmd)
		waitErr = <-waitCh
	}
	parsed := <-parsedCh
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if errors.Is(runCtx.Err(), context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
		return Result{}, ctx.Err()
	}
	output := parsed.output
	stderrText := trimOutput(stderr.String(), input.MaxOutputChars)
	if output == "" {
		output = stderrText
	}
	if output == "" {
		output = "(run_subagent returned no text)"
	}
	if timedOut {
		exitCode = 124
		output = fmt.Sprintf("Subagent timed out after %d seconds.\n\n%s", input.TimeoutSeconds, output)
	}
	return Result{Output: output, Stderr: stderrText, ExitCode: exitCode, TimedOut: timedOut,
		Usage: Usage{Turns: parsed.turns, Input: parsed.input, Output: parsed.outputTokens, Model: input.Model}}, nil
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

type parsedEvents struct {
	output       string
	turns        int
	input        int
	outputTokens int
}

func parseEvents(reader io.Reader, limit int) parsedEvents {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxEventLine)
	var parsed parsedEvents
	for scanner.Scan() {
		var event struct {
			Type    string         `json:"type"`
			Message *model.Message `json:"message"`
			Usage   *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Type == "turn_end" {
			parsed.turns++
			if event.Usage != nil {
				parsed.input += event.Usage.InputTokens
				parsed.outputTokens += event.Usage.OutputTokens
			}
			if event.Message != nil {
				if text := messageText(*event.Message); text != "" {
					parsed.output = trimOutput(text, limit)
				}
			}
		}
	}
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
