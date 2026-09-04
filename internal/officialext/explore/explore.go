// Package explore implements Notch's official explore_codebase extension.
package explore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trobrock/notch/internal/delegation"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/officialext/subagent"
)

const (
	Source         = "official:explore"
	ToolName       = "explore_codebase"
	maxTasks       = 8
	maxConcurrency = 4
)

type Task struct {
	Task  string `json:"task"`
	Model string `json:"model,omitempty"`
	CWD   string `json:"cwd,omitempty"`
}

type Input struct {
	Task  string `json:"task,omitempty"`
	Tasks []Task `json:"tasks,omitempty"`
	Model string `json:"model,omitempty"`
	CWD   string `json:"cwd,omitempty"`
}

type taskResult struct {
	Index    int            `json:"index"`
	Task     string         `json:"task"`
	Output   string         `json:"output"`
	ExitCode int            `json:"exitCode"`
	Usage    subagent.Usage `json:"usage"`
}

type runResult struct {
	Results        []taskResult
	DelegatedUsage delegation.Usage
}

// Register registers explore_codebase with the shared subagent runner.
func Register(registry *extension.Registry, host extension.Host) error {
	return RegisterWithSettingSources(registry, host, "user,project")
}

func RegisterWithSettingSources(registry *extension.Registry, host extension.Host, settingSources string) error {
	runner, err := subagent.NewRunnerWithSettingSources(host, settingSources)
	if err != nil {
		return err
	}
	return RegisterWithRunner(registry, runner)
}

func RegisterWithRunner(registry *extension.Registry, runner subagent.Runner) error {
	if registry == nil || runner == nil {
		return errors.New("register explore: registry and runner are required")
	}
	tool := extension.Tool{
		Source:     Source,
		UpdateMode: "replace",
		Definition: model.ToolDefinition{
			Name:        ToolName,
			Description: "Delegate broad or multi-file codebase discovery to isolated read-only Notch subagents when doing so is likely to save parent context or parallelize independent work. Prefer direct read/grep/find/ls calls for focused lookups, and avoid delegation when startup and duplicated context would likely cost more than a few direct tool calls. Always provide a tasks array: use one item for one focused question or multiple items for independent parallel questions. Normally omit model (or leave it empty) so Notch uses the configured explore model or current parent model. Never guess model IDs. If the selected model is unavailable, call list_models for that provider and retry once with the closest listed model in the same family and capability tier.",
			InputSchema: schema(),
		},
		Execute: func(ctx context.Context, raw json.RawMessage, update func(string)) (extension.ToolResult, error) {
			input, err := decode(raw)
			if err != nil {
				return extension.ToolResult{}, err
			}
			batch, err := run(ctx, runner, input, update)
			if err != nil {
				return extension.ToolResult{}, err
			}
			failed := false
			for _, result := range batch.Results {
				failed = failed || result.ExitCode != 0
			}
			content := render(batch.Results)
			if guidance := modelRecoveryGuidance(batch.Results); guidance != "" {
				content += "\n\n" + guidance
			}
			return extension.ToolResult{Content: content, IsError: failed, Details: map[string]any{"results": batch.Results, "count": len(batch.Results), "delegated_usage": batch.DelegatedUsage}}, nil
		},
	}
	if err := registry.RegisterTool(tool); err != nil {
		return fmt.Errorf("register %s: %w", ToolName, err)
	}
	return nil
}

func schema() map[string]any {
	task := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task":  map[string]any{"type": "string", "minLength": 1, "description": "Focused exploration question."},
			"model": map[string]any{"type": "string", "description": "Optional per-task override. Normally omit or leave empty; only use an ID returned by list_models."},
			"cwd":   map[string]any{"type": "string", "description": "Optional working directory override."},
		},
		"required": []string{"task"}, "additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{"type": "array", "minItems": 1, "maxItems": maxTasks, "items": task, "description": "Exploration questions. Use one item for a single focused question or multiple independent items to run in parallel."},
			"model": map[string]any{"type": "string", "description": "Optional batch override. Normally omit or leave empty to use configured explore_model or the parent model; never guess an ID."},
			"cwd":   map[string]any{"type": "string", "description": "Default working directory override."},
		},
		"required": []string{"tasks"}, "additionalProperties": false,
	}
}

func decode(raw json.RawMessage) (Input, error) {
	var input Input
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) == 0 {
		return input, errors.New("arguments are required")
	}
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("decode arguments: %w", err)
	}
	input.Task = strings.TrimSpace(input.Task)
	for i := range input.Tasks {
		input.Tasks[i].Task = strings.TrimSpace(input.Tasks[i].Task)
	}
	if input.Task != "" {
		// Backward compatibility for callers using the old singular field. If a
		// provider also synthesizes a tasks placeholder, the explicit singular
		// request wins instead of forcing the model through a retry loop.
		input.Tasks = []Task{{Task: input.Task}}
	}
	if len(input.Tasks) == 0 {
		return input, errors.New("tasks is required")
	}
	if len(input.Tasks) > maxTasks {
		return input, fmt.Errorf("tasks must contain at most %d items", maxTasks)
	}
	for i := range input.Tasks {
		if input.Tasks[i].Task == "" {
			return input, fmt.Errorf("task %d must not be empty", i+1)
		}
	}
	return input, nil
}

func run(ctx context.Context, runner subagent.Runner, input Input, update func(string)) (runResult, error) {
	started := time.Now()
	results := make([]taskResult, len(input.Tasks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	var progressMu sync.Mutex
	completed := 0
	workers := min(maxConcurrency, len(input.Tasks))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				task := input.Tasks[index]
				modelName, cwd := task.Model, task.CWD
				if modelName == "" {
					modelName = input.Model
				}
				if cwd == "" {
					cwd = input.CWD
				}
				result, err := runner.Run(ctx, subagent.Input{
					Prompt: "Explore task: " + task.Task, Model: modelName, CWD: cwd,
					Tools: "read,grep,find,ls", Thinking: subagent.DefaultThinking, TimeoutSeconds: 300,
					MaxOutputChars: 8000, SystemPrompt: systemPrompt(),
				}, nil)
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					continue
				}
				results[index] = taskResult{Index: index + 1, Task: task.Task, Output: result.Output, ExitCode: result.ExitCode, Usage: result.Usage}
				progressMu.Lock()
				completed++
				if update != nil {
					update(fmt.Sprintf("explore: %d/%d complete", completed, len(input.Tasks)))
				}
				progressMu.Unlock()
			}
		}()
	}
	for i := range input.Tasks {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return runResult{}, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return runResult{}, firstErr
	}
	aggregate := delegation.Usage{WallMS: time.Since(started).Milliseconds()}
	for _, result := range results {
		aggregate.Turns += result.Usage.Turns
		aggregate.InputTokens += result.Usage.Input
		aggregate.OutputTokens += result.Usage.Output
		aggregate.Calls++
	}
	return runResult{Results: results, DelegatedUsage: aggregate}, nil
}

func modelRecoveryGuidance(results []taskResult) string {
	models := make(map[string]bool)
	providers := make(map[string]bool)
	for _, result := range results {
		if result.ExitCode == 0 || !isUnavailableModelError(result.Output) {
			continue
		}
		name := strings.TrimSpace(result.Usage.Model)
		if name == "" {
			continue
		}
		models[name] = true
		if provider, _, ok := strings.Cut(name, "/"); ok && provider != "" {
			providers[provider] = true
		}
	}
	if len(models) == 0 {
		return ""
	}
	modelNames := sortedKeys(models)
	providerNames := sortedKeys(providers)
	providerHint := "the failed model's provider"
	if len(providerNames) != 0 {
		quoted := make([]string, len(providerNames))
		for i, provider := range providerNames {
			quoted[i] = "`" + provider + "`"
		}
		providerHint = strings.Join(quoted, ", ")
	}
	quotedModels := make([]string, len(modelNames))
	for i, name := range modelNames {
		quotedModels[i] = "`" + name + "`"
	}
	return "Explore model " + strings.Join(quotedModels, ", ") + " appears unavailable. Do not invent another model ID. Call list_models for " + providerHint + " (refresh if needed), then retry once with a returned model closest in family and capability tier. For bounded read-only exploration, prefer a current mini/small coding variant; for nuanced architecture work, prefer the current full coding model."
}

func isUnavailableModelError(output string) bool {
	value := strings.ToLower(output)
	if !strings.Contains(value, "model") {
		return false
	}
	for _, marker := range []string{"not supported", "unsupported", "not found", "unknown model", "does not exist", "unavailable", "not available"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func systemPrompt() string {
	return `You are explore, a fast read-only codebase exploration subagent.
Use only read/search/list tools. Prefer grep/find first and read the smallest relevant sections.
Stop once you can answer confidently. Do not paste large excerpts or modify files.
Return a concise conclusion, key files/symbols, relevant flow, recommended next steps, and open questions.`
}

func render(results []taskResult) string {
	if len(results) == 1 {
		if results[0].ExitCode != 0 {
			return fmt.Sprintf("Exploration failed with exit %d.\n\n%s", results[0].ExitCode, results[0].Output)
		}
		return results[0].Output
	}
	parts := make([]string, len(results))
	for i, result := range results {
		parts[i] = fmt.Sprintf("## Exploration %d: %s\n\n%s", i+1, result.Task, result.Output)
	}
	return strings.Join(parts, "\n\n")
}
