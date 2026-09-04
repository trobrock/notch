package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/delegation"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/session"
)

type fakeProvider struct{ calls int }

func (f *fakeProvider) Stream(_ context.Context, req model.Request, emit func(model.StreamEvent)) (model.Response, error) {
	f.calls++
	if f.calls == 1 {
		return model.Response{Content: []model.Block{{Type: "tool_use", ID: "1", Name: "echo", Arguments: json.RawMessage(`{"value":"hello"}`)}}, StopReason: "tool_use"}, nil
	}
	emit(model.StreamEvent{Type: "thinking_delta", Text: "checked"})
	emit(model.StreamEvent{Type: "text_delta", Text: "done"})
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
}

func TestPromptDoesNotMutateMessagesWhenSessionAppendFails(t *testing.T) {
	store, err := session.New(t.TempDir(), "/work", "fake", "fake")
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	a, err := New(Config{Provider: provider, Registry: extension.NewRegistry(), Session: store, Model: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	err = a.Prompt(context.Background(), "must be durable", nil)
	if err == nil || !strings.Contains(err.Error(), "session is closed") {
		t.Fatalf("Prompt() error = %v", err)
	}
	if messages := a.Messages(); len(messages) != 0 {
		t.Fatalf("messages mutated after failed append: %#v", messages)
	}
	if count := a.MessageCount(); count != 0 {
		t.Fatalf("message count = %d, want 0", count)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.calls)
	}
}

func TestPromptFillsEmptyFailedToolResult(t *testing.T) {
	reg := extension.NewRegistry()
	err := reg.RegisterTool(extension.Tool{Definition: model.ToolDefinition{Name: "echo", InputSchema: map[string]any{"type": "object"}}, Source: "test", Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
		return extension.ToolResult{IsError: true}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Provider: &fakeProvider{}, Registry: reg, Model: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), "go", nil); err != nil {
		t.Fatal(err)
	}
	messages := a.Messages()
	if len(messages) < 3 || len(messages[2].Content) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	result := messages[2].Content[0]
	if !result.IsError || strings.TrimSpace(result.Text) == "" {
		t.Fatalf("empty failed tool result was persisted: %#v", result)
	}
}

func TestPromptExecutesToolAndContinues(t *testing.T) {
	reg := extension.NewRegistry()
	err := reg.RegisterTool(extension.Tool{Definition: model.ToolDefinition{Name: "echo", InputSchema: map[string]any{"type": "object"}}, Source: "test", Execute: func(_ context.Context, args json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		return extension.ToolResult{Content: string(args)}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	a, err := New(Config{Provider: provider, Registry: reg, Model: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	var text, thinking string
	var toolArgs json.RawMessage
	if err := a.Prompt(context.Background(), "go", func(e Event) {
		if e.Type == "text_delta" {
			text += e.Text
		}
		if e.Type == "thinking_delta" {
			thinking += e.Text
		}
		if e.Type == "tool_start" {
			toolArgs = append(json.RawMessage(nil), e.Arguments...)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d", provider.calls)
	}
	if text != "done" || thinking != "checked" {
		t.Fatalf("text = %q, thinking = %q", text, thinking)
	}
	if string(toolArgs) != `{"value":"hello"}` {
		t.Fatalf("tool arguments = %s", toolArgs)
	}
	messages := a.Messages()
	if len(messages) != 4 || messages[2].Content[0].Type != "tool_result" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
}

type usageProvider struct{}

func (usageProvider) Stream(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
	cost := 0.0123
	return model.Response{
		Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn",
		InputTokens: 123, OutputTokens: 45, CacheReadTokens: 67, CacheWriteTokens: 8,
		ReasoningTokens: 12, CostUSD: &cost,
	}, nil
}

func TestPromptPersistsProviderUsage(t *testing.T) {
	store, err := session.New(t.TempDir(), "/work", "anthropic", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	a, err := New(Config{
		Provider: usageProvider{}, ProviderName: "anthropic", Registry: extension.NewRegistry(),
		Session: store, Model: "model-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), "go", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := session.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if len(loaded.UsageEntries) != 1 {
		t.Fatalf("usage entries = %#v", loaded.UsageEntries)
	}
	entry := loaded.UsageEntries[0]
	if entry.Provider != "anthropic" || entry.Model != "model-a" || entry.Usage.InputTokens != 123 || entry.Usage.OutputTokens != 45 || entry.Usage.CacheReadTokens != 67 || entry.Usage.CacheWriteTokens != 8 || entry.Usage.ReasoningTokens != 12 || entry.Usage.CostUSD == nil || *entry.Usage.CostUSD != 0.0123 || entry.Usage.ProviderCostUSD == nil || *entry.Usage.ProviderCostUSD != 0.0123 || entry.Usage.EstimatedCostUSD != nil || entry.Usage.CostSource != "provider" || entry.StopReason != "end_turn" {
		t.Fatalf("usage entry = %#v", entry)
	}
	if entry.Delegated != nil {
		t.Fatalf("delegated usage unexpectedly present: %#v", entry.Delegated)
	}
}

type delegatedToolProvider struct{ calls int }

func (p *delegatedToolProvider) Stream(_ context.Context, req model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	p.calls++
	if p.calls == 1 {
		return model.Response{Content: []model.Block{{Type: "tool_use", ID: "d1", Name: "delegate", Arguments: json.RawMessage(`{}`)}}, StopReason: "tool_use", InputTokens: 11, OutputTokens: 2}, nil
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn", InputTokens: 7, OutputTokens: 3}, nil
}

func TestPromptAggregatesDelegatedUsageAndPersistsAfterTools(t *testing.T) {
	store, err := session.New(t.TempDir(), "/work", "anthropic", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	reg := extension.NewRegistry()
	if err := reg.RegisterTool(extension.Tool{
		Definition: model.ToolDefinition{Name: "delegate", InputSchema: map[string]any{"type": "object"}},
		Source:     "test",
		Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
			return extension.ToolResult{Content: "ok", Details: map[string]any{"delegated_usage": map[string]any{"turns": 2, "input_tokens": 9, "output_tokens": 4, "wall_ms": 33, "calls": 1}}}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	provider := &delegatedToolProvider{}
	a, err := New(Config{Provider: provider, ProviderName: "anthropic", Registry: reg, Session: store, Model: "model-a"})
	if err != nil {
		t.Fatal(err)
	}
	var turnEnds []Usage
	var delegationEvents []delegation.Usage
	if err := a.Prompt(context.Background(), "go", func(event Event) {
		if event.Type == "turn_end" && event.Usage != nil {
			turnEnds = append(turnEnds, *event.Usage)
		}
		if event.Type == "delegation_usage" && event.DelegationUsage != nil {
			delegationEvents = append(delegationEvents, *event.DelegationUsage)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(turnEnds) != 2 {
		t.Fatalf("turn ends = %#v", turnEnds)
	}
	if len(delegationEvents) != 1 || delegationEvents[0] != (delegation.Usage{Turns: 2, InputTokens: 9, OutputTokens: 4, WallMS: 33, Calls: 1}) {
		t.Fatalf("delegation events = %#v", delegationEvents)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := session.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if len(loaded.UsageEntries) != 2 || loaded.UsageEntries[0].Delegated == nil || loaded.UsageEntries[1].Delegated != nil {
		t.Fatalf("usage entries = %#v", loaded.UsageEntries)
	}
	if loaded.UsageEntries[0].Delegated.WallMS != 33 || loaded.UsageEntries[0].Delegated.InputTokens != 9 {
		t.Fatalf("delegated persisted = %#v", loaded.UsageEntries[0].Delegated)
	}
}

type recordingProvider struct {
	mu       sync.Mutex
	requests []model.Request
	block    <-chan struct{}
}

func (p *recordingProvider) Stream(ctx context.Context, req model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
	}
	text := "done"
	if len(req.Tools) == 0 && len(req.Messages) == 1 && req.SystemPrompt != "" {
		text = "old work summarized"
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: text}}, InputTokens: 20, APIPricingEligible: true}, nil
}

func TestSteeringAndFollowUpQueues(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	provider := &queueTestProvider{started: started, release: release}
	a, err := New(Config{Provider: provider, Registry: extension.NewRegistry(), Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Steer("too early"); err == nil {
		t.Fatal("idle steering succeeded")
	}
	var mu sync.Mutex
	var events []Event
	done := make(chan error, 1)
	go func() {
		done <- a.Prompt(context.Background(), "initial", func(event Event) { mu.Lock(); events = append(events, event); mu.Unlock() })
	}()
	<-started
	steer, err := a.Steer("change direction")
	if err != nil {
		t.Fatal(err)
	}
	follow, err := a.FollowUp("then summarize")
	if err != nil {
		t.Fatal(err)
	}
	if steer.Mode != "steer" || follow.Mode != "follow_up" || steer.ID == follow.ID {
		t.Fatalf("queued = %#v / %#v", steer, follow)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	messages := a.Messages()
	if len(messages) != 6 || messages[2].Content[0].Text != "change direction" || messages[4].Content[0].Text != "then summarize" {
		t.Fatalf("messages = %#v", messages)
	}
	provider.mu.Lock()
	requests := append([]model.Request(nil), provider.requests...)
	provider.mu.Unlock()
	if len(requests) != 3 || requests[1].Messages[len(requests[1].Messages)-1].Content[0].Text != "change direction" || requests[2].Messages[len(requests[2].Messages)-1].Content[0].Text != "then summarize" {
		t.Fatalf("requests = %#v", requests)
	}
	mu.Lock()
	defer mu.Unlock()
	var delivered []string
	for _, event := range events {
		if event.Type == "queue_delivered" && event.Queued != nil {
			delivered = append(delivered, event.Queued.Mode)
		}
	}
	if strings.Join(delivered, ",") != "steer,follow_up" {
		t.Fatalf("delivered = %#v events=%#v", delivered, events)
	}
}

type parallelToolProvider struct {
	calls    int
	requests []model.Request
}

func (p *parallelToolProvider) Stream(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	p.calls++
	p.requests = append(p.requests, request)
	if p.calls == 1 {
		return model.Response{Content: []model.Block{
			{Type: "tool_use", ID: "call-1", Name: "parallel_test", Arguments: json.RawMessage(`{"value":"one"}`)},
			{Type: "tool_use", ID: "call-2", Name: "parallel_test", Arguments: json.RawMessage(`{"value":"two"}`)},
			{Type: "tool_use", ID: "call-3", Name: "parallel_test", Arguments: json.RawMessage(`{"value":"three"}`)},
		}, StopReason: "tool_use"}, nil
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
}

func TestPromptExecutesParallelToolCallsConcurrentlyAndPreservesOrder(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	registry := extension.NewRegistry()
	if err := registry.RegisterTool(extension.Tool{
		Definition: model.ToolDefinition{Name: "parallel_test", InputSchema: map[string]any{"type": "object"}},
		Execute: func(_ context.Context, args json.RawMessage, _ func(string)) (extension.ToolResult, error) {
			started <- struct{}{}
			<-release
			var input struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(args, &input); err != nil {
				return extension.ToolResult{}, err
			}
			return extension.ToolResult{Content: input.Value}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	provider := &parallelToolProvider{}
	runner, err := New(Config{Provider: provider, Registry: registry, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runner.Prompt(context.Background(), "run", nil) }()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			<-done
			t.Fatal("tool calls did not execute concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	messages := provider.requests[1].Messages
	results := messages[len(messages)-1].Content
	if len(results) != 3 || results[0].ToolUseID != "call-1" || results[0].Text != "one" || results[1].ToolUseID != "call-2" || results[1].Text != "two" || results[2].ToolUseID != "call-3" || results[2].Text != "three" {
		t.Fatalf("tool results = %#v", results)
	}
}

type longToolLoopProvider struct{ calls int }

func (p *longToolLoopProvider) Stream(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
	p.calls++
	if p.calls <= 50 {
		return model.Response{
			Content: []model.Block{{
				Type: "tool_use", ID: fmt.Sprintf("call-%d", p.calls), Name: "continue_test", Arguments: json.RawMessage(`{}`),
			}},
			StopReason: "tool_use",
		}, nil
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
}

func TestPromptHasNoFixedTurnLimit(t *testing.T) {
	registry := extension.NewRegistry()
	if err := registry.RegisterTool(extension.Tool{
		Definition: model.ToolDefinition{Name: "continue_test", InputSchema: map[string]any{"type": "object"}},
		Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
			return extension.ToolResult{Content: "continue"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	provider := &longToolLoopProvider{}
	a, err := New(Config{Provider: provider, Registry: registry, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), "run", nil); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 51 {
		t.Fatalf("provider calls = %d, want 51", provider.calls)
	}
}

type queueTestProvider struct {
	mu       sync.Mutex
	requests []model.Request
	started  chan struct{}
	release  chan struct{}
	calls    int
}

func (p *queueTestProvider) Stream(ctx context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	if call == 1 {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: fmt.Sprintf("answer-%d", call)}}, StopReason: "end_turn"}, nil
}

func TestThinkingLevelForwardedAndSetDoesNotWaitForPrompt(t *testing.T) {
	blocked := make(chan struct{})
	provider := &recordingProvider{block: blocked}
	a, err := New(Config{Provider: provider, Registry: extension.NewRegistry(), Model: "fake", ThinkingLevel: "low"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Prompt(context.Background(), "go", nil) }()
	for deadline := time.Now().Add(time.Second); ; {
		provider.mu.Lock()
		started := len(provider.requests) != 0
		provider.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prompt did not reach provider")
		}
		time.Sleep(time.Millisecond)
	}
	setDone := make(chan error, 1)
	go func() { setDone <- a.SetThinkingLevel("xhigh") }()
	select {
	case err := <-setDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("SetThinkingLevel waited for active Prompt")
	}
	close(blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	got := provider.requests[0].ReasoningLevel
	provider.mu.Unlock()
	if got != "low" {
		t.Fatalf("reasoning level = %q", got)
	}
}

func TestEstimatedTokensUsesUTF8Bytes(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want int
	}{
		{name: "empty", data: nil, want: 0},
		{name: "ascii", data: []byte("abcdefghijkl"), want: 4},
		{name: "round up", data: []byte("abcd"), want: 2},
		{name: "multibyte", data: []byte("你好"), want: 2},
		{name: "malformed", data: []byte{0xff, 0xfe, 0xfd, 0xfc}, want: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := estimatedTokens(test.data); got != test.want {
				t.Fatalf("estimatedTokens(%q) = %d, want %d", test.data, got, test.want)
			}
		})
	}
}

func TestPromptUsesStableSessionCacheKeyAndEstimatesCost(t *testing.T) {
	first, err := session.New(t.TempDir(), "/work", "anthropic", "claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	provider := &recordingProvider{}
	a, err := New(Config{
		Provider: provider, ProviderName: "anthropic", Registry: extension.NewRegistry(),
		Session: first, Model: "claude-sonnet-4-5", CacheRetention: "long",
	})
	if err != nil {
		t.Fatal(err)
	}
	var turnUsage *Usage
	if err := a.Prompt(context.Background(), "first", func(event Event) {
		if event.Type == "turn_end" {
			turnUsage = event.Usage
		}
	}); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	request := provider.requests[0]
	provider.mu.Unlock()
	if request.CacheRetention != "long" || request.CacheKey != "notch-"+first.Header.ID {
		t.Fatalf("cache request = %#v", request)
	}
	if turnUsage == nil || turnUsage.CostUSD == nil || turnUsage.EstimatedCostUSD == nil || turnUsage.ProviderCostUSD != nil || turnUsage.CostSource != "api_list_price_estimate" || turnUsage.PricingVersion == "" {
		t.Fatalf("turn usage = %#v", turnUsage)
	}
	if got, want := *turnUsage.CostUSD, 0.00006; math.Abs(got-want) > 1e-12 {
		t.Fatalf("estimated cost = %v, want %v", got, want)
	}

	second, err := session.New(t.TempDir(), "/work", "anthropic", "claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	old, err := a.ResumeSession(second)
	if err != nil || old != first {
		t.Fatalf("ResumeSession() = %#v, %v", old, err)
	}
	if err := a.Prompt(context.Background(), "second", nil); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	secondRequest := provider.requests[1]
	provider.mu.Unlock()
	if secondRequest.CacheKey != "notch-"+second.Header.ID || secondRequest.CacheKey == request.CacheKey {
		t.Fatalf("resumed cache key = %q", secondRequest.CacheKey)
	}
}

func TestManualCompactionPersists(t *testing.T) {
	store, err := session.New(t.TempDir(), t.TempDir(), "fake", "fake")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, message := range []model.Message{
		model.TextMessage("user", "old task"), model.TextMessage("assistant", "old answer"),
		model.TextMessage("user", "recent task"), model.TextMessage("assistant", "recent answer"),
	} {
		if err := store.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	provider := &recordingProvider{}
	a, err := New(Config{
		Provider: provider, Registry: extension.NewRegistry(), Session: store, Model: "fake",
		Compaction: CompactionConfig{Enabled: false, ContextWindow: 128000, ReserveTokens: 16384, KeepRecentTokens: 20000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Compact(context.Background(), "focus on files", false, nil); err != nil {
		t.Fatal(err)
	}
	var entry session.CompactionEntry
	if err := json.Unmarshal(store.Entries[len(store.Entries)-1], &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Type != "compaction" || entry.Auto || entry.Summary != "old work summarized" {
		t.Fatalf("unexpected compaction entry: %#v", entry)
	}
	if len(store.UsageEntries) != 1 || store.UsageEntries[0].Provider != "fake" || store.UsageEntries[0].Usage.InputTokens != 20 {
		t.Fatalf("compaction usage = %#v", store.UsageEntries)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 1 || provider.requests[0].CacheRetention != "none" {
		t.Fatalf("compaction cache config = %#v", provider.requests)
	}
}

func TestAutomaticCompactionAndReset(t *testing.T) {
	store, err := session.New(t.TempDir(), t.TempDir(), "fake", "fake")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, message := range []model.Message{
		model.TextMessage("user", "first task with substantial old context"),
		model.TextMessage("assistant", "first answer with substantial old context"),
		model.TextMessage("user", "recent task"),
	} {
		if err := store.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	provider := &recordingProvider{}
	a, err := New(Config{
		Provider: provider, Registry: extension.NewRegistry(), Session: store, Model: "fake",
		Compaction: CompactionConfig{Enabled: true, ContextWindow: 50, ReserveTokens: 10, KeepRecentTokens: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	var starts, ends int
	if err := a.Prompt(context.Background(), "newest task", func(event Event) {
		if event.Type == "compaction_start" && event.Auto {
			starts++
		}
		if event.Type == "compaction_end" && event.Auto {
			ends++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("compaction events = %d/%d", starts, ends)
	}
	messages := a.Messages()
	if len(messages) < 2 || messages[0].Content[0].Text[:22] != "[Conversation summary]" {
		t.Fatalf("messages were not compacted: %#v", messages)
	}
	if messages[1].Content[0].Type == "tool_result" {
		t.Fatal("recent context starts with a tool result")
	}
	if _, err := a.ResetConversation(store); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages()) != 0 || len(store.Messages) != 0 {
		t.Fatal("reset did not clear conversation")
	}
}

func TestSwitchProviderPreservesContextAndRecordsSession(t *testing.T) {
	store, err := session.New(t.TempDir(), "/work", "old", "old-model")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AppendMessage(model.TextMessage("user", "keep me")); err != nil {
		t.Fatal(err)
	}
	oldProvider, nextProvider := &recordingProvider{}, &recordingProvider{}
	a, err := New(Config{Provider: oldProvider, Registry: extension.NewRegistry(), Model: "old-model", Session: store, Compaction: CompactionConfig{ContextWindow: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SwitchProvider("new", nextProvider, "new-model", 999); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages()) != 1 || a.Messages()[0].Content[0].Text != "keep me" || a.model != "new-model" || a.provider != nextProvider || a.compaction.ContextWindow != 999 {
		t.Fatalf("switched agent = %#v", a)
	}
	if len(store.Entries) != 2 || !strings.Contains(string(store.Entries[1]), `"type":"model_change"`) {
		t.Fatalf("session entries = %s", store.Entries)
	}
}

func TestResumeSessionRestoresMessages(t *testing.T) {
	oldSession, err := session.New(t.TempDir(), "/old", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	defer oldSession.Close()
	nextSession, err := session.New(t.TempDir(), "/next", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	defer nextSession.Close()
	if err := nextSession.AppendMessage(model.TextMessage("user", "restored")); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Provider: &recordingProvider{}, Registry: extension.NewRegistry(), Model: "m", Session: oldSession})
	if err != nil {
		t.Fatal(err)
	}
	old, err := a.ResumeSession(nextSession)
	if err != nil {
		t.Fatal(err)
	}
	if old != oldSession || len(a.Messages()) != 1 || a.Messages()[0].Content[0].Text != "restored" {
		t.Fatalf("resume = old %p messages %#v", old, a.Messages())
	}
	if _, err := a.ResumeSession(nil); err == nil {
		t.Fatal("nil session resumed")
	}
}

func TestToolHookCanDeny(t *testing.T) {
	reg := extension.NewRegistry()
	called := false
	_ = reg.RegisterTool(extension.Tool{Definition: model.ToolDefinition{Name: "echo"}, Source: "test", Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
		called = true
		return extension.ToolResult{}, nil
	}})
	reg.On("tool_call", "test", func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"denied": true, "reason": "blocked"}, nil
	})
	provider := &fakeProvider{}
	a, _ := New(Config{Provider: provider, Registry: reg, Model: "fake"})
	if err := a.Prompt(context.Background(), "go", nil); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("denied tool executed")
	}
	if got := a.Messages()[2].Content[0].Text; got != "blocked" {
		t.Fatalf("result = %q", got)
	}
}

func TestContextAndStatusLifecycleHooks(t *testing.T) {
	provider := &recordingProvider{}
	registry := extension.NewRegistry()
	var lifecycle []string
	registry.On("agent_start", "test", func(context.Context, map[string]any) (map[string]any, error) {
		lifecycle = append(lifecycle, "start")
		return nil, nil
	})
	registry.On("context", "test", func(_ context.Context, event map[string]any) (map[string]any, error) {
		messages, _ := decodeHookMessages(event["messages"])
		messages = append(messages, model.TextMessage("user", "injected"))
		return map[string]any{"messages": messages}, nil
	})
	a, err := New(Config{Provider: provider, Registry: registry, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), "original", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lifecycle, []string{"start"}) {
		t.Fatalf("lifecycle=%#v", lifecycle)
	}
	if got := provider.requests[0].Messages[len(provider.requests[0].Messages)-1].Content[0].Text; got != "injected" {
		t.Fatalf("request message=%q", got)
	}
}

func TestAgentErrorHook(t *testing.T) {
	provider := &errorProvider{err: errors.New("provider failed")}
	registry := extension.NewRegistry()
	var message string
	registry.On("agent_error", "test", func(_ context.Context, event map[string]any) (map[string]any, error) {
		message, _ = event["message"].(string)
		return nil, nil
	})
	a, err := New(Config{Provider: provider, Registry: registry, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), "go", nil); err == nil {
		t.Fatal("prompt succeeded")
	}
	if message != "provider failed" {
		t.Fatalf("message=%q", message)
	}
}

type errorProvider struct{ err error }

func (p *errorProvider) Stream(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
	return model.Response{}, p.err
}

func TestQueuedProviderSwitchAppliesAfterToolTurn(t *testing.T) {
	oldProvider := &queueTestProvider{started: make(chan struct{}), release: make(chan struct{})}
	close(oldProvider.release)
	registry := extension.NewRegistry()
	next := &recordingProvider{}
	var agentRef *Agent
	if err := registry.RegisterTool(extension.Tool{Definition: model.ToolDefinition{Name: "switch"}, Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
		return extension.ToolResult{}, agentRef.QueueProviderSwitch("next", next, "next-model", 999)
	}}); err != nil {
		t.Fatal(err)
	}
	provider := &switchToolProvider{}
	store, err := session.New(t.TempDir(), "/work", "old", "old-model")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a, err := New(Config{Provider: provider, ProviderName: "old", Registry: registry, Session: store, Model: "old-model"})
	if err != nil {
		t.Fatal(err)
	}
	agentRef = a
	if err := a.Prompt(context.Background(), "go", nil); err != nil {
		t.Fatal(err)
	}
	if len(next.requests) != 1 || next.requests[0].Model != "next-model" {
		t.Fatalf("next requests=%#v", next.requests)
	}
	if len(store.UsageEntries) != 2 || store.UsageEntries[0].Provider != "old" || store.UsageEntries[0].Model != "old-model" || store.UsageEntries[1].Provider != "next" || store.UsageEntries[1].Model != "next-model" {
		t.Fatalf("usage attribution=%#v", store.UsageEntries)
	}
}

type switchToolProvider struct{ calls int }

func (p *switchToolProvider) Stream(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	p.calls++
	if p.calls == 1 {
		return model.Response{Content: []model.Block{{Type: "tool_use", ID: "1", Name: "switch", Arguments: json.RawMessage(`{}`)}}}, nil
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}}, nil
}

type retryProvider struct {
	calls       int
	failures    int
	emitOnError bool
}

func (p *retryProvider) Stream(_ context.Context, _ model.Request, emit func(model.StreamEvent)) (model.Response, error) {
	p.calls++
	if p.calls <= p.failures {
		if p.emitOnError {
			emit(model.StreamEvent{Type: "text_delta", Text: "partial"})
		}
		return model.Response{}, &model.ProviderError{Message: "overloaded", StatusCode: 529, Code: "overloaded_error"}
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "ok"}}, StopReason: "end_turn"}, nil
}

func TestPromptRetriesTransientProviderErrors(t *testing.T) {
	provider := &retryProvider{failures: 2}
	a, err := New(Config{Provider: provider, Registry: extension.NewRegistry(), Model: "m", Retry: RetryConfig{MaxAttempts: 3, BaseDelay: time.Nanosecond, MaxDelay: time.Nanosecond}})
	if err != nil {
		t.Fatal(err)
	}
	var retries []Event
	if err := a.Prompt(context.Background(), "go", func(event Event) {
		if event.Type == "provider_retry" {
			retries = append(retries, event)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 3 || len(retries) != 2 {
		t.Fatalf("calls=%d retries=%#v", provider.calls, retries)
	}
	if retries[0].Attempt != 2 || retries[1].Attempt != 3 || retries[0].MaxAttempts != 3 {
		t.Fatalf("retries=%#v", retries)
	}
}

func TestPromptDoesNotRetryAfterStreamOutput(t *testing.T) {
	provider := &retryProvider{failures: 1, emitOnError: true}
	a, err := New(Config{Provider: provider, Registry: extension.NewRegistry(), Model: "m", Retry: RetryConfig{MaxAttempts: 3, BaseDelay: time.Nanosecond, MaxDelay: time.Nanosecond}})
	if err != nil {
		t.Fatal(err)
	}
	err = a.Prompt(context.Background(), "go", func(Event) {})
	if err == nil || provider.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, provider.calls)
	}
}

func TestPromptRetryBackoffHonorsCancellation(t *testing.T) {
	provider := &retryProvider{failures: 10}
	a, err := New(Config{Provider: provider, Registry: extension.NewRegistry(), Model: "m", Retry: RetryConfig{MaxAttempts: 3, BaseDelay: time.Hour, MaxDelay: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = a.Prompt(ctx, "go", func(event Event) {
		if event.Type == "provider_retry" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || provider.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, provider.calls)
	}
}

func TestDelegationToolsUseConfiguredDefaults(t *testing.T) {
	a, err := New(Config{
		Provider: &fakeProvider{}, ProviderName: "openrouter", Registry: extension.NewRegistry(),
		Model: "anthropic/claude-test", ExploreModel: " openai-codex/gpt-explore ",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"run_subagent":     "openrouter/anthropic/claude-test",
		"explore_codebase": "openai-codex/gpt-explore",
	}
	for name, expected := range want {
		call := model.Block{Name: name, Arguments: json.RawMessage(`{"prompt":"inspect"}`)}
		a.applyDelegationModelDefault(&call)
		var arguments map[string]any
		if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
			t.Fatal(err)
		}
		if arguments["model"] != expected {
			t.Fatalf("%s model = %#v, want %q", name, arguments["model"], expected)
		}
	}
	explicit := model.Block{Name: "explore_codebase", Arguments: json.RawMessage(`{"tasks":[{"task":"inspect"}],"model":"anthropic/custom"}`)}
	a.applyDelegationModelDefault(&explicit)
	if !strings.Contains(string(explicit.Arguments), `"anthropic/custom"`) {
		t.Fatalf("explicit model changed: %s", explicit.Arguments)
	}
	ordinary := model.Block{Name: "read", Arguments: json.RawMessage(`{"path":"x"}`)}
	a.applyDelegationModelDefault(&ordinary)
	if string(ordinary.Arguments) != `{"path":"x"}` {
		t.Fatalf("ordinary tool changed: %s", ordinary.Arguments)
	}
}

type progressUpdateProvider struct{ calls int }

func (p *progressUpdateProvider) Stream(_ context.Context, _ model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	p.calls++
	if p.calls == 1 {
		return model.Response{Content: []model.Block{{Type: "tool_use", ID: "progress-1", Name: "progress", Arguments: json.RawMessage(`{}`)}}}, nil
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
}

func TestToolUpdateModeIsEmittedToFrontends(t *testing.T) {
	registry := extension.NewRegistry()
	if err := registry.RegisterTool(extension.Tool{
		Definition: model.ToolDefinition{Name: "progress"},
		UpdateMode: "replace",
		Execute: func(_ context.Context, _ json.RawMessage, update func(string)) (extension.ToolResult, error) {
			update("first")
			update("second")
			return extension.ToolResult{Content: "done"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Provider: &progressUpdateProvider{}, Registry: registry, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var updates []Event
	if err := a.Prompt(context.Background(), "go", func(event Event) {
		if event.Type == "tool_update" {
			updates = append(updates, event)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 || updates[0].ToolUpdateMode != "replace" || updates[1].ToolUpdateMode != "replace" || updates[1].Text != "second" {
		t.Fatalf("updates = %#v", updates)
	}
}

type benchmarkProvider struct {
	calls   int
	cost    *float64
	block   bool
	started chan struct{}
}

func (p *benchmarkProvider) Stream(ctx context.Context, _ model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	p.calls++
	if p.started != nil {
		close(p.started)
	}
	if p.block {
		<-ctx.Done()
		return model.Response{}, ctx.Err()
	}
	content := []model.Block{{Type: "text", Text: "done"}}
	stop := "end_turn"
	if p.calls == 1 {
		content, stop = []model.Block{{Type: "tool_use", ID: "1", Name: "echo", Arguments: json.RawMessage(`{}`)}}, "tool_use"
	}
	return model.Response{Content: content, StopReason: stop, InputTokens: 10, OutputTokens: 2, CostUSD: p.cost}, nil
}

func benchmarkRegistry(t *testing.T) *extension.Registry {
	t.Helper()
	registry := extension.NewRegistry()
	if err := registry.RegisterTool(extension.Tool{Definition: model.ToolDefinition{Name: "echo", InputSchema: map[string]any{"type": "object"}}, Source: "test", Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
		return extension.ToolResult{Content: "ok"}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestPromptMaxTurnsEmitsCumulativeRunEnd(t *testing.T) {
	cost := 0.25
	provider := &benchmarkProvider{cost: &cost}
	runner, err := New(Config{Provider: provider, ProviderName: "anthropic", Registry: benchmarkRegistry(t), Model: "test", MaxTurns: 1,
		SessionInfo: SessionInfo{NotchVersion: "test", WorkspaceTrusted: true}})
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	err = runner.Prompt(context.Background(), "go", func(event Event) { events = append(events, event) })
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("error = %v, want ErrMaxTurns", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	if len(events) < 2 || events[0].Type != "session_start" {
		t.Fatalf("events = %#v", events)
	}
	end := events[len(events)-1]
	if end.Type != "run_end" || end.StopReason != "max_turns" || end.NumTurns != 1 || end.Usage == nil || end.Usage.InputTokens != 10 || end.TotalCostUSD == nil || *end.TotalCostUSD != cost || end.FinalAssistant != "" {
		t.Fatalf("run_end = %#v", end)
	}
}

func TestPromptMaxCostAndUnknownPricing(t *testing.T) {
	cost := 1.25
	for _, tc := range []struct {
		name   string
		cost   *float64
		want   error
		reason string
	}{
		{"limit", &cost, ErrMaxCost, "max_cost"}, {"unknown", nil, ErrPricingUnknown, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &benchmarkProvider{cost: tc.cost}
			runner, err := New(Config{Provider: provider, Registry: benchmarkRegistry(t), Model: "unknown", MaxCostUSD: 1})
			if err != nil {
				t.Fatal(err)
			}
			var end Event
			err = runner.Prompt(context.Background(), "go", func(event Event) {
				if event.Type == "run_end" {
					end = event
				}
			})
			if !errors.Is(err, tc.want) || end.StopReason != tc.reason {
				t.Fatalf("error = %v, run_end = %#v", err, end)
			}
			if tc.cost == nil && (end.CostKnown == nil || *end.CostKnown || end.PricingError != "price_unknown") {
				t.Fatalf("unknown-price run_end = %#v", end)
			}
		})
	}
}

func TestPromptCancellationStillEmitsRunEnd(t *testing.T) {
	provider := &benchmarkProvider{block: true, started: make(chan struct{})}
	runner, err := New(Config{Provider: provider, Registry: extension.NewRegistry(), Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var events []Event
	done := make(chan error, 1)
	go func() { done <- runner.Prompt(ctx, "go", func(event Event) { events = append(events, event) }) }()
	<-provider.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if end := events[len(events)-1]; end.Type != "run_end" || end.StopReason != "canceled" {
		t.Fatalf("events = %#v", events)
	}
}

func TestPromptIdleTimeoutEmitsErrorRunEnd(t *testing.T) {
	provider := &benchmarkProvider{block: true, started: make(chan struct{})}
	runner, err := New(Config{Provider: provider, Registry: extension.NewRegistry(), Model: "test", IdleTimeout: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var end Event
	err = runner.Prompt(context.Background(), "go", func(event Event) {
		if event.Type == "run_end" {
			end = event
		}
	})
	if !errors.Is(err, ErrIdleTimeout) || end.StopReason != "error" {
		t.Fatalf("error = %v, run_end = %#v", err, end)
	}
}
