// Package explore implements Notch's official explore_codebase extension.
package explore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	runner, err := subagent.NewRunner(host)
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
		Source: Source,
		Definition: model.ToolDefinition{
			Name:        ToolName,
			Description: "Delegate broad or multi-file codebase discovery to isolated read-only Notch subagents when doing so is likely to save parent context or parallelize independent work. Prefer direct read/grep/find/ls calls for focused lookups, and avoid delegation when startup and duplicated context would likely cost more than a few direct tool calls. Provide exactly one of task (one focused question) or tasks (independent questions run in parallel).",
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
			return extension.ToolResult{Content: render(batch.Results), IsError: failed, Details: map[string]any{"results": batch.Results, "count": len(batch.Results), "delegated_usage": batch.DelegatedUsage}}, nil
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
			"model": map[string]any{"type": "string", "description": "Optional model override for this task."},
			"cwd":   map[string]any{"type": "string", "description": "Optional working directory override."},
		},
		"required": []string{"task"}, "additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task":  map[string]any{"type": "string", "minLength": 1, "description": "One focused exploration question. Do not provide tasks with this field."},
			"tasks": map[string]any{"type": "array", "minItems": 1, "maxItems": maxTasks, "items": task, "description": "Independent exploration questions to run in parallel. Do not provide task with this field."},
			"model": map[string]any{"type": "string", "description": "Default model override."},
			"cwd":   map[string]any{"type": "string", "description": "Default working directory override."},
		},
		// Anthropic rejects oneOf/anyOf/allOf at an input schema's top level.
		// decode enforces that exactly one of task or tasks is provided.
		"additionalProperties": false,
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
	if input.Task != "" && len(input.Tasks) != 0 {
		return input, errors.New("provide either task or tasks, not both")
	}
	if input.Task == "" && len(input.Tasks) == 0 {
		return input, errors.New("task or tasks is required")
	}
	if len(input.Tasks) > maxTasks {
		return input, fmt.Errorf("tasks must contain at most %d items", maxTasks)
	}
	if input.Task != "" {
		input.Tasks = []Task{{Task: input.Task}}
	}
	for i := range input.Tasks {
		input.Tasks[i].Task = strings.TrimSpace(input.Tasks[i].Task)
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
