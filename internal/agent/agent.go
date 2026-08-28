// Package agent implements Notch's small provider-independent agent loop.
package agent

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trobrock/notch/internal/delegation"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/pricing"
	"github.com/trobrock/notch/internal/session"
)

var ErrNotProcessing = errors.New("agent is not processing")

type QueuedMessage struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
	Text string `json:"text"`
}

type Event struct {
	Type            string                `json:"type"`
	Text            string                `json:"text,omitempty"`
	ToolName        string                `json:"tool_name,omitempty"`
	ToolCallID      string                `json:"tool_call_id,omitempty"`
	Arguments       json.RawMessage       `json:"arguments,omitempty"`
	Result          *extension.ToolResult `json:"result,omitempty"`
	Usage           *Usage                `json:"usage,omitempty"`
	ContextUsage    *ContextUsage         `json:"context_usage,omitempty"`
	Auto            bool                  `json:"auto,omitempty"`
	Queue           []QueuedMessage       `json:"queue,omitempty"`
	Queued          *QueuedMessage        `json:"queued,omitempty"`
	Message         *model.Message        `json:"message,omitempty"`
	StopReason      string                `json:"stop_reason,omitempty"`
	DelegationUsage *delegation.Usage     `json:"delegation_usage,omitempty"`
	Attempt         int                   `json:"attempt,omitempty"`
	MaxAttempts     int                   `json:"max_attempts,omitempty"`
	DelayMS         int64                 `json:"delay_ms,omitempty"`
}

type Usage struct {
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	CacheReadTokens  int      `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int      `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int      `json:"reasoning_tokens,omitempty"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
	ProviderCostUSD  *float64 `json:"provider_cost_usd,omitempty"`
	EstimatedCostUSD *float64 `json:"estimated_cost_usd,omitempty"`
	CostSource       string   `json:"cost_source,omitempty"`
	PricingVersion   string   `json:"pricing_version,omitempty"`
}

type CompactionConfig struct {
	Enabled          bool
	ContextWindow    int
	ReserveTokens    int
	KeepRecentTokens int
}

type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

type Config struct {
	Provider       model.Provider
	ProviderName   string
	Registry       *extension.Registry
	Session        *session.Session
	Model          string
	SystemPrompt   string
	MaxTokens      int
	ThinkingLevel  string
	CacheRetention string
	Compaction     CompactionConfig
	Retry          RetryConfig
}

type providerSwitch struct {
	providerName  string
	provider      model.Provider
	model         string
	contextWindow int
}

type Agent struct {
	provider       model.Provider
	providerName   string
	registry       *extension.Registry
	session        *session.Session
	model          string
	system         string
	maxTokens      int
	cacheRetention string
	cacheKey       string
	compaction     CompactionConfig
	retry          RetryConfig

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
	pendingSwitch *providerSwitch
}

func validCacheRetention(value string) bool {
	switch value {
	case "none", "short", "long":
		return true
	default:
		return false
	}
}

func newCacheKey(store *session.Session) (string, error) {
	if store != nil && store.Header.ID != "" {
		return "notch-" + store.Header.ID, nil
	}
	var value [16]byte
	if _, err := crand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate prompt cache key: %w", err)
	}
	return "notch-" + hex.EncodeToString(value[:]), nil
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
	if cfg.ThinkingLevel == "" {
		cfg.ThinkingLevel = "off"
	}
	if !validThinkingLevel(cfg.ThinkingLevel) {
		return nil, fmt.Errorf("invalid thinking level %q", cfg.ThinkingLevel)
	}
	if cfg.CacheRetention == "" {
		cfg.CacheRetention = "short"
	}
	cfg.CacheRetention = strings.ToLower(strings.TrimSpace(cfg.CacheRetention))
	if !validCacheRetention(cfg.CacheRetention) {
		return nil, fmt.Errorf("invalid cache retention %q", cfg.CacheRetention)
	}
	cacheKey, err := newCacheKey(cfg.Session)
	if err != nil {
		return nil, err
	}
	cfg.Compaction = defaultCompactionConfig(cfg.Compaction)
	if cfg.Retry.MaxAttempts <= 0 {
		cfg.Retry.MaxAttempts = 3
	}
	if cfg.Retry.BaseDelay <= 0 {
		cfg.Retry.BaseDelay = time.Second
	}
	if cfg.Retry.MaxDelay <= 0 {
		cfg.Retry.MaxDelay = 30 * time.Second
	}
	if cfg.Retry.MaxDelay < cfg.Retry.BaseDelay {
		cfg.Retry.MaxDelay = cfg.Retry.BaseDelay
	}
	providerName := strings.TrimSpace(cfg.ProviderName)
	if providerName == "" && cfg.Session != nil {
		providerName = cfg.Session.Header.Provider
	}
	a := &Agent{
		provider: cfg.Provider, providerName: providerName, registry: cfg.Registry, session: cfg.Session,
		model: cfg.Model, system: cfg.SystemPrompt, maxTokens: cfg.MaxTokens,
		thinkingLevel: cfg.ThinkingLevel, cacheRetention: cfg.CacheRetention, cacheKey: cacheKey,
		compaction: cfg.Compaction, retry: cfg.Retry,
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
	if a.session != nil {
		if err := a.session.AppendMessage(message); err != nil {
			return err
		}
	}
	a.messages = append(a.messages, message)
	a.messageCount.Store(int64(len(a.messages)))
	return nil
}

func (a *Agent) responseUsage(response model.Response, cacheRetention, providerName, modelName string) Usage {
	usage := Usage{
		InputTokens: response.InputTokens, OutputTokens: response.OutputTokens,
		CacheReadTokens: response.CacheReadTokens, CacheWriteTokens: response.CacheWriteTokens,
		ReasoningTokens: response.ReasoningTokens,
	}
	if response.CostUSD != nil {
		providerCost := *response.CostUSD
		usage.ProviderCostUSD = &providerCost
		usage.CostUSD = &providerCost
		usage.CostSource = "provider"
	}
	if estimated, ok := pricing.Estimate(providerName, modelName, cacheRetention, response); response.APIPricingEligible && ok {
		usage.EstimatedCostUSD = &estimated
		usage.PricingVersion = pricing.Version
		if usage.CostUSD == nil {
			usage.CostUSD = &estimated
			usage.CostSource = "api_list_price_estimate"
		}
	}
	return usage
}

func (a *Agent) appendUsage(response model.Response, delegated delegation.Usage, cacheRetention, providerName, modelName string) error {
	if a.session == nil {
		return nil
	}
	usage := a.responseUsage(response, cacheRetention, providerName, modelName)
	entry := session.TokenUsage{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens,
		ReasoningTokens: usage.ReasoningTokens, CostUSD: usage.CostUSD,
		ProviderCostUSD: usage.ProviderCostUSD, EstimatedCostUSD: usage.EstimatedCostUSD,
		CostSource: usage.CostSource, PricingVersion: usage.PricingVersion,
	}
	if delegated.Empty() {
		return a.session.AppendUsage(providerName, modelName, entry, response.StopReason)
	}
	return a.session.AppendUsage(providerName, modelName, entry, response.StopReason, session.DelegatedUsage{
		Turns: delegated.Turns, InputTokens: delegated.InputTokens, OutputTokens: delegated.OutputTokens,
		CacheReadTokens: delegated.CacheReadTokens, CacheWriteTokens: delegated.CacheWriteTokens,
		ReasoningTokens: delegated.ReasoningTokens, CostUSD: delegated.CostUSD,
		WallMS: delegated.WallMS, Calls: delegated.Calls,
	})
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
		return QueuedMessage{}, ErrNotProcessing
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

	for turn := 0; ; turn++ {
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
		requestMessages := contextMessages(a.messages)
		contextEvent, contextErr := a.registry.RunHooks(ctx, "context", map[string]any{"messages": requestMessages, "turn": turn})
		if contextErr != nil {
			return contextErr
		}
		if replacement, ok := decodeHookMessages(contextEvent["messages"]); ok {
			requestMessages = replacement
		}
		emit(Event{Type: "turn_start"})
		_, _ = a.registry.RunHooks(ctx, "agent_start", map[string]any{"model": a.model, "turn": turn})
		requestEstimate := a.estimatedContextTokensLocked()
		requestProvider, requestModel := a.providerName, a.model
		request := model.Request{
			Model: requestModel, SystemPrompt: system, Messages: requestMessages,
			Tools: a.registry.Definitions(), MaxTokens: a.maxTokens,
			ReasoningLevel: a.ThinkingLevel(), CacheRetention: a.cacheRetention, CacheKey: a.cacheKey,
		}
		response, err := a.streamWithRetry(ctx, request, emit)
		if err != nil {
			_, _ = a.registry.RunHooks(ctx, "agent_error", map[string]any{"message": err.Error(), "model": a.model, "turn": turn})
			emit(Event{Type: "error", Text: err.Error()})
			return err
		}
		if response.TotalInputTokens() > 0 {
			a.reportedInputTokens = response.TotalInputTokens()
			a.reportedEstimate = requestEstimate
		} else {
			a.reportedInputTokens = 0
			a.reportedEstimate = 0
		}
		assistant := model.Message{Role: "assistant", Content: response.Content}
		if err := a.appendMessage(assistant); err != nil {
			return err
		}

		turnUsage := a.responseUsage(response, a.cacheRetention, requestProvider, requestModel)
		usage := &turnUsage
		contextUsage := a.contextUsageLocked()
		emit(Event{Type: "turn_end", Usage: usage, ContextUsage: &contextUsage, Message: &assistant, StopReason: response.StopReason})
		var delegatedTotals delegation.Usage
		calls := toolCalls(response.Content)
		if len(calls) != 0 {
			results := make([]model.Block, 0, len(calls))
			for _, call := range calls {
				result := a.executeTool(ctx, call, emit)
				if delegated, ok := delegation.FromDetails(result.Details); ok {
					delegatedTotals = delegatedTotals.Add(delegated)
					running := delegatedTotals
					emit(Event{Type: "delegation_usage", DelegationUsage: &running})
				}
				results = append(results, model.Block{Type: "tool_result", ToolUseID: call.ID, Text: result.Content, IsError: result.IsError})
			}
			if err := a.appendMessage(model.Message{Role: "user", Content: results}); err != nil {
				if appendErr := a.appendUsage(response, delegatedTotals, a.cacheRetention, requestProvider, requestModel); appendErr != nil {
					return appendErr
				}
				return err
			}
			if err := a.applyPendingSwitchLocked(); err != nil {
				if appendErr := a.appendUsage(response, delegatedTotals, a.cacheRetention, requestProvider, requestModel); appendErr != nil {
					return appendErr
				}
				return err
			}
		}
		if err := a.appendUsage(response, delegatedTotals, a.cacheRetention, requestProvider, requestModel); err != nil {
			return err
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
}

func (a *Agent) streamWithRetry(ctx context.Context, request model.Request, emit func(Event)) (model.Response, error) {
	for attempt := 1; ; attempt++ {
		emitted := false
		response, err := a.provider.Stream(ctx, request, func(event model.StreamEvent) {
			switch event.Type {
			case "text_delta", "thinking_delta":
				emitted = true
				emit(Event{Type: event.Type, Text: event.Text})
			}
		})
		if err == nil {
			return response, nil
		}
		retryable, retryAfter := model.RetryInfo(err)
		// Retrying after visible output would duplicate content because provider
		// streams cannot be rolled back safely.
		if !retryable || emitted || attempt >= a.retry.MaxAttempts {
			return model.Response{}, err
		}
		delay := a.retryDelay(attempt, retryAfter)
		emit(Event{Type: "provider_retry", Text: err.Error(), Attempt: attempt + 1, MaxAttempts: a.retry.MaxAttempts, DelayMS: delay.Milliseconds()})
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return model.Response{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (a *Agent) retryDelay(failedAttempt int, retryAfter time.Duration) time.Duration {
	delay := a.retry.BaseDelay
	for i := 1; i < failedAttempt && delay < a.retry.MaxDelay; i++ {
		if delay > a.retry.MaxDelay/2 {
			delay = a.retry.MaxDelay
			break
		}
		delay *= 2
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > a.retry.MaxDelay {
		delay = a.retry.MaxDelay
	}
	// Full jitter avoids synchronized retries while retaining the configured
	// exponential ceiling. Server Retry-After remains a lower bound.
	floor := retryAfter
	if floor > delay {
		floor = delay
	}
	if delay > floor {
		delay = floor + time.Duration(rand.Int64N(int64(delay-floor)+1))
	}
	return delay
}

func decodeHookMessages(value any) ([]model.Message, bool) {
	if value == nil {
		return nil, false
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var messages []model.Message
	if json.Unmarshal(data, &messages) != nil {
		return nil, false
	}
	return messages, true
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
	result = extension.LimitToolResult(result)
	_, hookErr := a.registry.RunHooks(ctx, "tool_execution_end", map[string]any{
		"name": call.Name, "id": call.ID, "content": result.Content, "is_error": result.IsError,
	})
	if hookErr != nil {
		result = extension.ToolResult{Content: hookErr.Error(), IsError: true}
	}
	// Anthropic rejects error tool results with empty content. Preserve a useful
	// failure marker in provider-neutral history so new sessions are portable
	// across providers and can always be resumed.
	if result.IsError && strings.TrimSpace(result.Content) == "" {
		result.Content = "tool execution failed without an error message"
	}
	emit(Event{Type: "tool_end", ToolName: call.Name, ToolCallID: call.ID, Result: &result})
	return result
}
