// Package agent implements Notch's small provider-independent agent loop.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/session"
)

type QueuedMessage struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
	Text string `json:"text"`
}

type Event struct {
	Type         string                `json:"type"`
	Text         string                `json:"text,omitempty"`
	ToolName     string                `json:"tool_name,omitempty"`
	ToolCallID   string                `json:"tool_call_id,omitempty"`
	Arguments    json.RawMessage       `json:"arguments,omitempty"`
	Result       *extension.ToolResult `json:"result,omitempty"`
	Usage        *Usage                `json:"usage,omitempty"`
	ContextUsage *ContextUsage         `json:"context_usage,omitempty"`
	Auto         bool                  `json:"auto,omitempty"`
	Queue        []QueuedMessage       `json:"queue,omitempty"`
	Queued       *QueuedMessage        `json:"queued,omitempty"`
	Message      *model.Message        `json:"message,omitempty"`
	StopReason   string                `json:"stop_reason,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type CompactionConfig struct {
	Enabled          bool
	ContextWindow    int
	ReserveTokens    int
	KeepRecentTokens int
}

type Config struct {
	Provider      model.Provider
	ProviderName  string
	Registry      *extension.Registry
	Session       *session.Session
	Model         string
	SystemPrompt  string
	MaxTokens     int
	MaxTurns      int
	ThinkingLevel string
	Compaction    CompactionConfig
}

type Agent struct {
	provider     model.Provider
	providerName string
	registry     *extension.Registry
	session      *session.Session
	model        string
	system       string
	maxTokens    int
	maxTurns     int
	compaction   CompactionConfig

	// mu serializes operations which mutate conversation or session state.
	mu                  sync.Mutex
	messages            []model.Message
	reportedInputTokens int
	reportedEstimate    int
	messageCount        atomic.Int64

	// settingsMu is deliberately independent of mu: UI settings such as the
	// thinking level remain responsive while Prompt is waiting on a provider.
	settingsMu    sync.RWMutex
	thinkingLevel string

	queueMu       sync.Mutex
	processing    bool
	queueSequence uint64
	steering      []QueuedMessage
	followUps     []QueuedMessage
	queueEmit     func(Event)
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
	if cfg.ThinkingLevel == "" {
		cfg.ThinkingLevel = "off"
	}
	if !validThinkingLevel(cfg.ThinkingLevel) {
		return nil, fmt.Errorf("invalid thinking level %q", cfg.ThinkingLevel)
	}
	cfg.Compaction = defaultCompactionConfig(cfg.Compaction)
	providerName := strings.TrimSpace(cfg.ProviderName)
	if providerName == "" && cfg.Session != nil {
		providerName = cfg.Session.Header.Provider
	}
	a := &Agent{
		provider: cfg.Provider, providerName: providerName, registry: cfg.Registry, session: cfg.Session,
		model: cfg.Model, system: cfg.SystemPrompt, maxTokens: cfg.MaxTokens,
		maxTurns: cfg.MaxTurns, thinkingLevel: cfg.ThinkingLevel,
		compaction: cfg.Compaction,
	}
	if cfg.Session != nil {
		a.messages = cloneMessages(cfg.Session.Messages)
	}
	a.messageCount.Store(int64(len(a.messages)))
	return a, nil
}

func (a *Agent) Messages() []model.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneMessages(a.messages)
}

// MessageCount returns the current effective context size without waiting for
// an active provider request or tool execution.
func (a *Agent) MessageCount() int { return int(a.messageCount.Load()) }

// QueueStatus returns whether a prompt is active and snapshots pending queues.
func (a *Agent) QueueStatus() (processing bool, steering, followUps []QueuedMessage) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	return a.processing, append([]QueuedMessage(nil), a.steering...), append([]QueuedMessage(nil), a.followUps...)
}

func (a *Agent) appendMessage(message model.Message) error {
	a.messages = append(a.messages, message)
	a.messageCount.Store(int64(len(a.messages)))
	if a.session != nil {
		return a.session.AppendMessage(message)
	}
	return nil
}

func (a *Agent) appendUsage(response model.Response) error {
	if a.session == nil {
		return nil
	}
	return a.session.AppendUsage(a.providerName, a.model, session.TokenUsage{
		InputTokens: response.InputTokens, OutputTokens: response.OutputTokens,
	}, response.StopReason)
}

// Prompt runs until the model produces a final response without tool calls.
// Steer queues a user message for the next model turn after the current turn
// and any tool results complete.
func (a *Agent) Steer(text string) (QueuedMessage, error) { return a.enqueueMessage("steer", text) }

// FollowUp queues a user message for after the current agent run would
// otherwise settle.
func (a *Agent) FollowUp(text string) (QueuedMessage, error) {
	return a.enqueueMessage("follow_up", text)
}

func (a *Agent) enqueueMessage(mode, text string) (QueuedMessage, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return QueuedMessage{}, errors.New("queued message is empty")
	}
	a.queueMu.Lock()
	if !a.processing {
		a.queueMu.Unlock()
		return QueuedMessage{}, errors.New("agent is not processing")
	}
	a.queueSequence++
	message := QueuedMessage{ID: fmt.Sprintf("q-%d", a.queueSequence), Mode: mode, Text: text}
	if mode == "follow_up" {
		a.followUps = append(a.followUps, message)
	} else {
		a.steering = append(a.steering, message)
	}
	emit, queue := a.queueEmit, a.queueSnapshotLocked()
	a.queueMu.Unlock()
	if emit != nil {
		emit(Event{Type: "queue_update", Queue: queue})
	}
	return message, nil
}

func (a *Agent) beginProcessing(emit func(Event)) {
	a.queueMu.Lock()
	a.processing, a.queueEmit = true, emit
	queue := a.queueSnapshotLocked()
	a.queueMu.Unlock()
	if len(queue) != 0 {
		emit(Event{Type: "queue_update", Queue: queue})
	}
}

func (a *Agent) endProcessing() {
	a.queueMu.Lock()
	a.processing, a.queueEmit = false, nil
	a.queueMu.Unlock()
}

func (a *Agent) queueSnapshotLocked() []QueuedMessage {
	queue := make([]QueuedMessage, 0, len(a.steering)+len(a.followUps))
	queue = append(queue, a.steering...)
	queue = append(queue, a.followUps...)
	return queue
}

func (a *Agent) takeQueued(mode string) (QueuedMessage, bool) {
	a.queueMu.Lock()
	var message QueuedMessage
	var ok bool
	if mode == "steer" && len(a.steering) != 0 {
		message, a.steering, ok = a.steering[0], a.steering[1:], true
	} else if mode == "follow_up" && len(a.followUps) != 0 {
		message, a.followUps, ok = a.followUps[0], a.followUps[1:], true
	}
	emit, queue := a.queueEmit, a.queueSnapshotLocked()
	a.queueMu.Unlock()
	if ok && emit != nil {
		copy := message
		emit(Event{Type: "queue_delivered", Queued: &copy})
		emit(Event{Type: "queue_update", Queue: queue})
	}
	return message, ok
}

func (a *Agent) settleOrTakeQueued() (QueuedMessage, bool) {
	a.queueMu.Lock()
	var message QueuedMessage
	var ok bool
	if len(a.steering) != 0 {
		message, a.steering, ok = a.steering[0], a.steering[1:], true
	} else if len(a.followUps) != 0 {
		message, a.followUps, ok = a.followUps[0], a.followUps[1:], true
	} else {
		a.processing, a.queueEmit = false, nil
	}
	emit, queue := a.queueEmit, a.queueSnapshotLocked()
	a.queueMu.Unlock()
	if ok && emit != nil {
		copy := message
		emit(Event{Type: "queue_delivered", Queued: &copy})
		emit(Event{Type: "queue_update", Queue: queue})
	}
	return message, ok
}

// Calls are serialized so an Agent's history and session remain ordered.
func (a *Agent) Prompt(ctx context.Context, text string, emit func(Event)) error {
	return a.PromptWithStart(ctx, text, emit, nil)
}

// PromptWithStart is Prompt with a callback invoked after queueing becomes
// available but before model work starts. RPC uses it to acknowledge a prompt
// before any streamed events can be emitted.
func (a *Agent) PromptWithStart(ctx context.Context, text string, emit func(Event), started func()) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if emit == nil {
		emit = func(Event) {}
	}
	a.beginProcessing(emit)
	defer a.endProcessing()
	if started != nil {
		started()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if text != "" {
		if err := a.appendMessage(model.TextMessage("user", text)); err != nil {
			return err
		}
	}
	if queued, ok := a.takeQueued("steer"); ok {
		if err := a.appendMessage(model.TextMessage("user", queued.Text)); err != nil {
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

		if a.shouldAutoCompactLocked() {
			if compactErr := a.compactLocked(ctx, "", true, emit); compactErr != nil && !errors.Is(compactErr, ErrNothingToCompact) {
				return compactErr
			}
		}
		emit(Event{Type: "turn_start"})
		requestEstimate := a.estimatedContextTokensLocked()
		response, err := a.provider.Stream(ctx, model.Request{
			Model: a.model, SystemPrompt: system, Messages: cloneMessages(a.messages),
			Tools: a.registry.Definitions(), MaxTokens: a.maxTokens,
			ReasoningLevel: a.ThinkingLevel(),
		}, func(event model.StreamEvent) {
			switch event.Type {
			case "text_delta", "thinking_delta":
				emit(Event{Type: event.Type, Text: event.Text})
			}
		})
		if err != nil {
			emit(Event{Type: "error", Text: err.Error()})
			return err
		}
		if response.InputTokens > 0 {
			a.reportedInputTokens = response.InputTokens
			a.reportedEstimate = requestEstimate
		} else {
			a.reportedInputTokens = 0
			a.reportedEstimate = 0
		}
		assistant := model.Message{Role: "assistant", Content: response.Content}
		if err := a.appendMessage(assistant); err != nil {
			return err
		}

		usage := &Usage{InputTokens: response.InputTokens, OutputTokens: response.OutputTokens}
		if err := a.appendUsage(response); err != nil {
			return err
		}
		contextUsage := a.contextUsageLocked()
		emit(Event{Type: "turn_end", Usage: usage, ContextUsage: &contextUsage, Message: &assistant, StopReason: response.StopReason})
		calls := toolCalls(response.Content)
		if len(calls) != 0 {
			results := make([]model.Block, 0, len(calls))
			for _, call := range calls {
				result := a.executeTool(ctx, call, emit)
				results = append(results, model.Block{Type: "tool_result", ToolUseID: call.ID, Text: result.Content, IsError: result.IsError})
			}
			if err := a.appendMessage(model.Message{Role: "user", Content: results}); err != nil {
				return err
			}
		}

		// Steering interrupts the normal tool-call chain at the next safe turn
		// boundary, after tool results have been recorded.
		if queued, ok := a.takeQueued("steer"); ok {
			if err := a.appendMessage(model.TextMessage("user", queued.Text)); err != nil {
				return err
			}
			continue
		}
		if len(calls) != 0 {
			continue
		}

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

		// The queue check and transition to idle are atomic, so a message cannot
		// be accepted as queued after the run has already decided to settle.
		if queued, ok := a.settleOrTakeQueued(); ok {
			if err := a.appendMessage(model.TextMessage("user", queued.Text)); err != nil {
				return err
			}
			continue
		}
		return nil
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
	emit(Event{Type: "tool_start", ToolName: call.Name, ToolCallID: call.ID, Arguments: append(json.RawMessage(nil), call.Arguments...)})
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
