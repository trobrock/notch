// Package agent implements Notch's small provider-independent agent loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/session"
)

type Event struct {
	Type       string                `json:"type"`
	Text       string                `json:"text,omitempty"`
	ToolName   string                `json:"tool_name,omitempty"`
	ToolCallID string                `json:"tool_call_id,omitempty"`
	Result     *extension.ToolResult `json:"result,omitempty"`
	Usage      *Usage                `json:"usage,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Config struct {
	Provider     model.Provider
	Registry     *extension.Registry
	Session      *session.Session
	Model        string
	SystemPrompt string
	MaxTokens    int
	MaxTurns     int
}

type Agent struct {
	provider  model.Provider
	registry  *extension.Registry
	session   *session.Session
	model     string
	system    string
	maxTokens int
	maxTurns  int

	mu       sync.Mutex
	messages []model.Message
}

func New(cfg Config) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, errors.New("agent requires a provider")
	}
	if cfg.Registry == nil {
		return nil, errors.New("agent requires an extension registry")
	}
	if cfg.Model == "" {
		return nil, errors.New("agent requires a model")
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 8192
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 50
	}
	a := &Agent{provider: cfg.Provider, registry: cfg.Registry, session: cfg.Session, model: cfg.Model, system: cfg.SystemPrompt, maxTokens: cfg.MaxTokens, maxTurns: cfg.MaxTurns}
	if cfg.Session != nil {
		a.messages = append(a.messages, cfg.Session.Messages...)
	}
	return a, nil
}

func (a *Agent) Messages() []model.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]model.Message(nil), a.messages...)
}

func (a *Agent) appendMessage(message model.Message) error {
	a.messages = append(a.messages, message)
	if a.session != nil {
		return a.session.AppendMessage(message)
	}
	return nil
}

// Prompt runs until the model produces a final response without tool calls.
// Calls are serialized so an Agent's history and session remain ordered.
func (a *Agent) Prompt(ctx context.Context, text string, emit func(Event)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if emit == nil {
		emit = func(Event) {}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if text != "" {
		if err := a.appendMessage(model.TextMessage("user", text)); err != nil {
			return err
		}
	}

	for turn := 0; turn < a.maxTurns; turn++ {
		system := a.system
		before, err := a.registry.RunHooks(ctx, "before_agent_start", map[string]any{
			"system_prompt": system, "model": a.model, "turn": turn,
		})
		if err != nil {
			return err
		}
		if value, ok := before["system_prompt"].(string); ok {
			system = value
		}

		emit(Event{Type: "turn_start"})
		response, err := a.provider.Stream(ctx, model.Request{
			Model: a.model, SystemPrompt: system, Messages: append([]model.Message(nil), a.messages...),
			Tools: a.registry.Definitions(), MaxTokens: a.maxTokens,
		}, func(event model.StreamEvent) {
			if event.Type == "text_delta" {
				emit(Event{Type: "text_delta", Text: event.Text})
			}
		})
		if err != nil {
			emit(Event{Type: "error", Text: err.Error()})
			return err
		}
		assistant := model.Message{Role: "assistant", Content: response.Content}
		if err := a.appendMessage(assistant); err != nil {
			return err
		}

		usage := &Usage{InputTokens: response.InputTokens, OutputTokens: response.OutputTokens}
		emit(Event{Type: "turn_end", Usage: usage})
		calls := toolCalls(response.Content)
		if len(calls) == 0 {
			end, hookErr := a.registry.RunHooks(ctx, "agent_end", map[string]any{
				"stop_reason": response.StopReason, "turn": turn,
			})
			if hookErr != nil {
				return hookErr
			}
			if followUp, ok := end["follow_up"].(string); ok && followUp != "" {
				if err := a.appendMessage(model.TextMessage("user", followUp)); err != nil {
					return err
				}
				continue
			}
			return nil
		}

		results := make([]model.Block, 0, len(calls))
		for _, call := range calls {
			result := a.executeTool(ctx, call, emit)
			results = append(results, model.Block{Type: "tool_result", ToolUseID: call.ID, Text: result.Content, IsError: result.IsError})
		}
		if err := a.appendMessage(model.Message{Role: "user", Content: results}); err != nil {
			return err
		}
	}
	return fmt.Errorf("agent stopped after %d turns", a.maxTurns)
}

func toolCalls(blocks []model.Block) []model.Block {
	var calls []model.Block
	for _, block := range blocks {
		if block.Type == "tool_use" || block.Type == "function_call" {
			calls = append(calls, block)
		}
	}
	return calls
}

func (a *Agent) executeTool(ctx context.Context, call model.Block, emit func(Event)) extension.ToolResult {
	eventArgs := any(map[string]any{})
	if len(call.Arguments) != 0 {
		_ = json.Unmarshal(call.Arguments, &eventArgs)
	}
	intercepted, err := a.registry.RunHooks(ctx, "tool_call", map[string]any{
		"name": call.Name, "id": call.ID, "arguments": eventArgs,
	})
	if err != nil {
		return extension.ToolResult{Content: err.Error(), IsError: true}
	}
	if denied, _ := intercepted["denied"].(bool); denied {
		reason, _ := intercepted["reason"].(string)
		if reason == "" {
			reason = "tool call denied by extension"
		}
		result := extension.ToolResult{Content: reason, IsError: true}
		emit(Event{Type: "tool_end", ToolName: call.Name, ToolCallID: call.ID, Result: &result})
		return result
	}
	if replacement, ok := intercepted["arguments"]; ok {
		if raw, marshalErr := json.Marshal(replacement); marshalErr == nil {
			call.Arguments = raw
		}
	}

	tool, ok := a.registry.Tool(call.Name)
	if !ok {
		result := extension.ToolResult{Content: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}
		emit(Event{Type: "tool_end", ToolName: call.Name, ToolCallID: call.ID, Result: &result})
		return result
	}
	emit(Event{Type: "tool_start", ToolName: call.Name, ToolCallID: call.ID})
	_, _ = a.registry.RunHooks(ctx, "tool_execution_start", map[string]any{"name": call.Name, "id": call.ID})
	result, execErr := tool.Execute(ctx, call.Arguments, func(update string) {
		emit(Event{Type: "tool_update", ToolName: call.Name, ToolCallID: call.ID, Text: update})
	})
	if execErr != nil {
		result = extension.ToolResult{Content: execErr.Error(), IsError: true}
	}
	_, hookErr := a.registry.RunHooks(ctx, "tool_execution_end", map[string]any{
		"name": call.Name, "id": call.ID, "content": result.Content, "is_error": result.IsError,
	})
	if hookErr != nil {
		result = extension.ToolResult{Content: hookErr.Error(), IsError: true}
	}
	emit(Event{Type: "tool_end", ToolName: call.Name, ToolCallID: call.ID, Result: &result})
	return result
}
