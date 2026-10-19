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
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/trobrock/notch/internal/config"
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
	Prompt            string  `json:"prompt"`
	Role              string  `json:"role,omitempty"`
	Model             string  `json:"model,omitempty"`
	CWD               string  `json:"cwd,omitempty"`
	Tools             string  `json:"tools,omitempty"`
	AllowWriteTools   bool    `json:"allowWriteTools,omitempty"`
	TimeoutSeconds    int     `json:"timeoutSeconds,omitempty"`
	MaxOutputChars    int     `json:"maxOutputChars,omitempty"`
	MaxTurns          int     `json:"maxTurns,omitempty"`
	MaxCostUSD        float64 `json:"maxCostUSD,omitempty"`
	SystemPrompt      string  `json:"systemPrompt,omitempty"`
	Thinking          string  `json:"thinking,omitempty"`
	ModelExplicit     bool    `json:"-"`
	ThinkingExplicit  bool    `json:"-"`
	TimeoutExplicit   bool    `json:"-"`
	MaxOutputExplicit bool    `json:"-"`
	MaxTurnsExplicit  bool    `json:"-"`
	MaxCostExplicit   bool    `json:"-"`
	TimeoutLimit      int     `json:"-"`
	MaxOutputLimit    int     `json:"-"`
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
	Output   string                 `json:"output"`
	Stderr   string                 `json:"stderr,omitempty"`
	ExitCode int                    `json:"exitCode"`
	TimedOut bool                   `json:"timedOut"`
	Usage    Usage                  `json:"usage"`
	Role     *config.RoleResolution `json:"role,omitempty"`
}

type Runner interface {
	Run(context.Context, Input, func(string)) (Result, error)
}

type roleRunner struct {
	base            Runner
	host            extension.Host
	cfg             config.Config
	routing         *config.RoutingSnapshot
	defaultProvider string
}

// NewRoleRunner adds reusable role policy without changing the authority of
// the wrapped runner.
func NewRoleRunner(base Runner, host extension.Host, cfg config.Config, routing *config.RoutingSnapshot, provider string) Runner {
	if base == nil || host == nil {
		return base
	}
	return &roleRunner{base: base, host: host, cfg: cfg, routing: routing, defaultProvider: provider}
}

func (r *roleRunner) Run(ctx context.Context, input Input, update func(string)) (Result, error) {
	if strings.TrimSpace(input.Role) == "" {
		return r.base.Run(ctx, input, update)
	}
	resolved, err := config.ResolveRole(r.cfg, r.routing, input.Role)
	if err != nil {
		return Result{}, err
	}
	if !input.ModelExplicit && strings.TrimSpace(input.Model) == "" {
		input.Model = resolved.Model
	}
	if !input.ThinkingExplicit && resolved.Thinking != "" {
		input.Thinking = resolved.Thinking
	}
	if !input.TimeoutExplicit && resolved.TimeoutSeconds > 0 {
		input.TimeoutSeconds = resolved.TimeoutSeconds
	}
	if !input.MaxOutputExplicit && resolved.MaxOutputChars > 0 {
		input.MaxOutputChars = resolved.MaxOutputChars
	}
	if !input.MaxTurnsExplicit && resolved.MaxTurns > 0 {
		input.MaxTurns = resolved.MaxTurns
	}
	if !input.MaxCostExplicit && resolved.MaxCostUSD > 0 {
		input.MaxCostUSD = resolved.MaxCostUSD
	}
	if input.TimeoutLimit > 0 && input.TimeoutSeconds > input.TimeoutLimit {
		return Result{}, fmt.Errorf("role %q timeout_seconds %d exceeds this delegation tool's limit %d", resolved.Name, input.TimeoutSeconds, input.TimeoutLimit)
	}
	if input.MaxOutputLimit > 0 && input.MaxOutputChars > input.MaxOutputLimit {
		return Result{}, fmt.Errorf("role %q max_output_chars %d exceeds this delegation tool's limit %d", resolved.Name, input.MaxOutputChars, input.MaxOutputLimit)
	}
	if input.ModelExplicit {
		resolved.Model, resolved.ModelProvenance = input.Model, "explicit task override"
	}
	resolved.Thinking = input.Thinking
	resolved.TimeoutSeconds = input.TimeoutSeconds
	resolved.MaxOutputChars = input.MaxOutputChars
	resolved.MaxTurns = input.MaxTurns
	resolved.MaxCostUSD = input.MaxCostUSD
	input.SystemPrompt = strings.TrimSpace(resolved.Instructions + "\n" + input.SystemPrompt)
	if err := r.validateModel(ctx, input.Model); err != nil {
		return Result{}, err
	}
	config.RefreshRoleHash(&resolved)
	if sessionID := r.host.SessionID(); sessionID != "" {
		if err := r.host.AppendSessionEntry(sessionID, "delegation_role", resolved); err != nil {
			return Result{}, fmt.Errorf("record resolved role before dispatch: %w", err)
		}
	}
	if update != nil {
		update("role " + resolved.Name + " · model " + effectiveModelLabel(input.Model))
	}
	result, err := r.base.Run(ctx, input, update)
	if resolved.Model == "" && result.Usage.Model != "" {
		resolved.Model = result.Usage.Model
	}
	config.RefreshRoleHash(&resolved)
	if sessionID := r.host.SessionID(); sessionID != "" {
		if appendErr := r.host.AppendSessionEntry(sessionID, "delegation_role_result", resolved); appendErr != nil && err == nil {
			err = fmt.Errorf("record resolved role result: %w", appendErr)
		}
	}
	result.Role = &resolved
	return result, err
}

func effectiveModelLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "base default"
	}
	return strings.TrimSpace(value)
}

func (r *roleRunner) validateModel(ctx context.Context, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	provider, modelName := strings.ToLower(strings.TrimSpace(r.defaultProvider)), value
	if current, ok := r.host.(interface{ CurrentModel() (string, string) }); ok {
		if currentProvider, _ := current.CurrentModel(); strings.TrimSpace(currentProvider) != "" {
			provider = strings.ToLower(strings.TrimSpace(currentProvider))
		}
	}
	if prefix, rest, found := strings.Cut(value, "/"); found && isProviderName(prefix) {
		provider, modelName = strings.ToLower(prefix), rest
	}
	if r.routing != nil {
		if err := r.routing.ValidateProvider(provider); err != nil {
			return err
		}
	}
	models, err := r.host.ListModels(ctx, provider, false)
	if err != nil {
		return fmt.Errorf("validate role model %q with provider %q before dispatch: %w", modelName, provider, err)
	}
	for _, available := range models {
		if available.ID == modelName {
			return nil
		}
	}
	return fmt.Errorf("role model %q is unavailable from provider %q; call list_models for that provider and update the role mapping (no cross-provider fallback was attempted)", modelName, provider)
}

func isProviderName(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "openai-codex", "anthropic-claude-code", "openrouter", "anthropic", "openai":
		return true
	default:
		return false
	}
}

type processRunner struct {
	executable       string
	defaultCWD       string
	heartbeatPeriod  time.Duration
	settingSources   string
	defaultProvider  string
	defaultModel     string
	currentModel     func() (string, string)
	routingSnapshot  string
	allowedProviders map[string]bool
}

// NewRunner creates a subprocess runner using the current Notch executable.
func NewRunner(host extension.Host) (Runner, error) {
	return NewRunnerWithSettingSources(host, "user,project")
}

func NewRunnerWithSettingSources(host extension.Host, settingSources string) (Runner, error) {
	return NewRunnerWithRuntime(host, settingSources, "", "")
}

// NewRunnerWithRuntime also supplies the parent provider/model used when a
// helper omits a model or gives an unqualified model ID.
func NewRunnerWithRuntime(host extension.Host, settingSources, provider, modelName string) (Runner, error) {
	return NewRunnerWithRouting(host, settingSources, provider, modelName, "", nil)
}

// NewRunnerWithRouting pins child routing to the parent's resolved snapshot.
func NewRunnerWithRouting(host extension.Host, settingSources, provider, modelName, routingSnapshot string, allowedProviders []string) (Runner, error) {
	if host == nil {
		return nil, errors.New("create subagent runner: host is required")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Notch executable: %w", err)
	}
	allowed := make(map[string]bool, len(allowedProviders))
	for _, name := range allowedProviders {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			allowed[name] = true
		}
	}
	runner := &processRunner{executable: executable, defaultCWD: host.CWD(), settingSources: settingSources, defaultProvider: provider, defaultModel: modelName, routingSnapshot: routingSnapshot, allowedProviders: allowed}
	if current, ok := host.(interface{ CurrentModel() (string, string) }); ok {
		runner.currentModel = current.CurrentModel
	}
	return runner, nil
}

// Register registers run_subagent using the current Notch executable.
func Register(registry *extension.Registry, host extension.Host) error {
	return RegisterWithSettingSources(registry, host, "user,project")
}

func RegisterWithSettingSources(registry *extension.Registry, host extension.Host, settingSources string) error {
	return RegisterWithRuntime(registry, host, settingSources, "", "")
}

func RegisterWithRuntime(registry *extension.Registry, host extension.Host, settingSources, provider, modelName string) error {
	return RegisterWithRouting(registry, host, settingSources, provider, modelName, "", nil)
}

func RegisterWithRouting(registry *extension.Registry, host extension.Host, settingSources, provider, modelName, routingSnapshot string, allowedProviders []string) error {
	if registry == nil || host == nil {
		return errors.New("register subagent: registry and host are required")
	}
	runner, err := NewRunnerWithRouting(host, settingSources, provider, modelName, routingSnapshot, allowedProviders)
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
			Description: "Spawn a focused Notch subagent in an isolated process. Defaults to read-only tools; enable write-capable tools only when the user explicitly wants delegated implementation. Omit model to use the current parent provider/model. An unqualified model stays on the current provider. Use a provider-qualified model only when the user explicitly requested that provider (or explicitly configured it for delegation); if a requested model is unavailable on the current provider and no other provider was named, ask before using another provider.",
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
				"timedOut": result.TimedOut, "usage": result.Usage, "role": result.Role,
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
			"role":            map[string]any{"type": "string", "description": "Reusable specialist role. Built-ins: lead, researcher, designer, developer (implementer alias), qa, reviewer. Global custom roles are also accepted."},
			"model":           map[string]any{"type": "string", "description": "Model ID, optionally provider/model. Omit to use the current parent provider/model. An unqualified ID stays on the current provider; qualify it only for a user-requested or explicitly configured provider change."},
			"cwd":             map[string]any{"type": "string", "description": "Working directory. Defaults to the parent working directory."},
			"tools":           map[string]any{"type": "string", "description": "Comma-separated tool allowlist. Defaults to read,grep,find,ls. bash is NOT read-only, even for inspection commands. Any other tool requires explicit delegated write authorization and allowWriteTools=true."},
			"allowWriteTools": map[string]any{"type": "boolean", "description": "Permit tools outside read,grep,find,ls (including bash). Enable only when the user explicitly authorizes delegated write access; never enable just to run read-only shell commands."},
			"timeoutSeconds":  map[string]any{"type": "integer", "minimum": 1, "maximum": maxTimeoutSeconds},
			"maxOutputChars":  map[string]any{"type": "integer", "minimum": 1000, "maximum": maxOutputChars},
			"maxTurns":        map[string]any{"type": "integer", "minimum": 1, "description": "Maximum model turns for this subagent."},
			"maxCostUSD":      map[string]any{"type": "number", "exclusiveMinimum": 0, "description": "Maximum cumulative estimated cost in USD for this subagent."},
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
	var present map[string]json.RawMessage
	_ = json.Unmarshal(raw, &present)
	_, input.ModelExplicit = present["model"]
	_, input.ThinkingExplicit = present["thinking"]
	_, input.TimeoutExplicit = present["timeoutSeconds"]
	_, input.MaxOutputExplicit = present["maxOutputChars"]
	_, input.MaxTurnsExplicit = present["maxTurns"]
	_, input.MaxCostExplicit = present["maxCostUSD"]
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
	if input.MaxTurns < 0 {
		return input, errors.New("maxTurns must be non-negative")
	}
	if input.MaxCostUSD < 0 || math.IsNaN(input.MaxCostUSD) || math.IsInf(input.MaxCostUSD, 0) {
		return input, errors.New("maxCostUSD must be a finite non-negative number")
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
		return input, fmt.Errorf("tools include non-read-only tools (%s). bash is write-capable regardless of the intended command. For read-only discovery omit tools or use read,grep,find,ls; run shell analysis in the parent. Set allowWriteTools=true only with explicit user authorization for delegated write access", strings.Join(unsafe, ", "))
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
	if err := os.WriteFile(systemPath, []byte(baseSystemPrompt("")), 0o600); err != nil {
		return Result{}, fmt.Errorf("write subagent system prompt: %w", err)
	}
	delegationPath := ""
	if extra := strings.TrimSpace(input.SystemPrompt); extra != "" {
		delegationPath = filepath.Join(tempDir, "DELEGATION.md")
		if err := os.WriteFile(delegationPath, []byte(extra), 0o600); err != nil {
			return Result{}, fmt.Errorf("write subagent delegation instructions: %w", err)
		}
	}

	settingSources := r.settingSources
	if settingSources == "" {
		settingSources = "user,project"
	}
	args := []string{"--json", "--setting-sources", settingSources, "--no-session", "--no-tui", "--no-extensions", "--no-resources", "--tools", input.Tools,
		"--thinking", input.Thinking, "--system-prompt-file", systemPath}
	if delegationPath != "" {
		args = append(args, "--delegation-instructions-file", delegationPath)
	}
	if input.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(input.MaxTurns))
	}
	if input.MaxCostUSD > 0 {
		args = append(args, "--max-cost-usd", strconv.FormatFloat(input.MaxCostUSD, 'f', -1, 64))
	}
	provider, parentModel := r.defaultProvider, r.defaultModel
	if r.currentModel != nil {
		if currentProvider, currentName := r.currentModel(); currentProvider != "" || currentName != "" {
			provider, parentModel = currentProvider, currentName
		}
	}
	selectedProvider := strings.ToLower(strings.TrimSpace(provider))
	if requestedProvider, _, found := strings.Cut(strings.TrimSpace(input.Model), "/"); found && requestedProvider != "" {
		selectedProvider = strings.ToLower(strings.TrimSpace(requestedProvider))
	}
	if len(r.allowedProviders) != 0 && !r.allowedProviders[selectedProvider] {
		return Result{}, fmt.Errorf("provider %q is not allowed by the active profile", selectedProvider)
	}
	if r.routingSnapshot != "" {
		args = append(args, "--routing-snapshot", r.routingSnapshot)
	}
	args = append(args, modelArgs(input.Model, provider, parentModel)...)
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
	cmd.Env = childEnvironment(os.Environ())
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
	canceled := errors.Is(runCtx.Err(), context.Canceled) && errors.Is(ctx.Err(), context.Canceled)
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
	result := Result{Output: output, Stderr: stderrText, ExitCode: exitCode, TimedOut: timedOut,
		Usage: Usage{
			Turns: parsed.turns, ToolCalls: parsed.toolCalls, Input: parsed.input, Output: parsed.outputTokens,
			CacheRead: parsed.cacheRead, CacheWrite: parsed.cacheWrite, Reasoning: parsed.reasoning,
			CostUSD: parsed.costUSD(), WallMS: wallMS, Model: input.Model,
		}}
	if canceled {
		return result, ctx.Err()
	}
	return result, nil
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

func childEnvironment(values []string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "HERDR_") {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func modelArgs(value, parentProvider, parentModel string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = strings.TrimSpace(parentModel)
		if value == "" {
			return nil
		}
		if parentProvider = strings.TrimSpace(parentProvider); parentProvider != "" {
			return []string{"--provider", parentProvider, "--model", value}
		}
		return []string{"--model", value}
	}
	provider, name, found := strings.Cut(value, "/")
	if found && provider != "" && name != "" && !strings.Contains(provider, "/") {
		return []string{"--provider", provider, "--model", name}
	}
	if parentProvider = strings.TrimSpace(parentProvider); parentProvider != "" {
		return []string{"--provider", parentProvider, "--model", value}
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
