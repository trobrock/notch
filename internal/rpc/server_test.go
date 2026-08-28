package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/delegation"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/session"
)

type rpcTestProvider struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (p *rpcTestProvider) Stream(ctx context.Context, _ model.Request, emit func(model.StreamEvent)) (model.Response, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
	}
	text := "answer-one"
	if call > 1 {
		text = "answer-two"
	}
	emit(model.StreamEvent{Type: "text_delta", Text: text})
	return model.Response{
		Content: []model.Block{{Type: "text", Text: text}}, StopReason: "end_turn",
		InputTokens: 10 * call, OutputTokens: 2,
	}, nil
}

func TestRPCUsageKeepsProviderAndDelegationMetricsSeparate(t *testing.T) {
	cost, delegatedCost := 0.012, 0.004
	estimated := 0.013
	usage := rpcUsage(&agent.Usage{
		InputTokens: 12, OutputTokens: 3, CacheReadTokens: 5, CacheWriteTokens: 2,
		ReasoningTokens: 1, CostUSD: &cost, ProviderCostUSD: &cost, EstimatedCostUSD: &estimated,
		CostSource: "provider", PricingVersion: "test-v1",
	})
	if usage["providerTokens"] != 22 || usage["totalTokens"] != 22 || usage["cacheRead"] != 5 || usage["cacheWrite"] != 2 || usage["reasoning"] != 1 || usage["cost"].(map[string]float64)["total"] != cost || usage["costKnown"] != true || usage["costSource"] != "provider" || usage["providerCostUSD"] != cost || usage["estimatedCostUSD"] != estimated || usage["pricingVersion"] != "test-v1" {
		t.Fatalf("usage = %#v", usage)
	}
	delegatedUsage := rpcDelegationUsage(&delegation.Usage{
		Turns: 2, InputTokens: 7, OutputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 1,
		ReasoningTokens: 2, CostUSD: &delegatedCost, WallMS: 40, Calls: 1,
	})
	if delegatedUsage["wallMs"] != int64(40) || delegatedUsage["totalTokens"] != 16 || delegatedUsage["costUSD"] != delegatedCost {
		t.Fatalf("delegated = %#v", delegatedUsage)
	}
}

func TestServerStatePromptStreamingAndSteering(t *testing.T) {
	provider := &rpcTestProvider{started: make(chan struct{}), release: make(chan struct{})}
	registry := extension.NewRegistry()
	if err := registry.RegisterTool(extension.Tool{
		Definition: model.ToolDefinition{Name: "read", Description: "Read a file"}, Source: "builtin",
		Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
			return extension.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := agent.New(agent.Config{Provider: provider, Registry: registry, Model: "test-model", ThinkingLevel: "medium"})
	if err != nil {
		t.Fatal(err)
	}

	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	server := New(inputReader, outputWriter, "/work")
	server.Configure(runner, registry, nil, StateConfig{
		Provider: "test", Model: "test-model", API: "test-api", ContextWindow: 1000,
		MaxTokens: 100, AutoCompactionEnabled: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx); _ = outputWriter.Close() }()

	records := make(chan map[string]any, 128)
	go decodeRecords(outputReader, records)
	writeCommand := func(value map[string]any) {
		t.Helper()
		data, _ := json.Marshal(value)
		if _, err := inputWriter.Write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	var seen []map[string]any
	await := func(match func(map[string]any) bool) map[string]any {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case record := <-records:
				seen = append(seen, record)
				if match(record) {
					return record
				}
			case <-timer.C:
				t.Fatalf("timed out; records = %#v", seen)
			}
		}
	}
	responseID := func(id string) func(map[string]any) bool {
		return func(record map[string]any) bool { return record["type"] == "response" && record["id"] == id }
	}

	writeCommand(map[string]any{"id": "state-1", "type": "get_state"})
	initial := await(responseID("state-1"))
	data := initial["data"].(map[string]any)
	if data["isStreaming"] != false || data["messageCount"] != float64(0) || data["thinkingLevel"] != "medium" {
		t.Fatalf("initial state = %#v", data)
	}
	if tools := data["tools"].([]any); len(tools) != 1 || tools[0] != "read" {
		t.Fatalf("state tools = %#v", tools)
	}

	writeCommand(map[string]any{"id": "prompt-1", "type": "prompt", "message": "hello"})
	accepted := await(responseID("prompt-1"))
	if accepted["success"] != true {
		t.Fatalf("prompt response = %#v", accepted)
	}
	<-provider.started
	writeCommand(map[string]any{"id": "state-2", "type": "status"})
	streaming := await(responseID("state-2"))["data"].(map[string]any)
	if streaming["isStreaming"] != true || streaming["messageCount"] != float64(1) {
		t.Fatalf("streaming state = %#v", streaming)
	}

	writeCommand(map[string]any{"id": "prompt-2", "type": "prompt", "message": "rejected"})
	if rejected := await(responseID("prompt-2")); rejected["success"] != false || !strings.Contains(rejected["error"].(string), "streamingBehavior") {
		t.Fatalf("active prompt response = %#v", rejected)
	}
	writeCommand(map[string]any{"id": "prompt-3", "type": "prompt", "message": "change direction", "streamingBehavior": "steer"})
	if queued := await(responseID("prompt-3")); queued["success"] != true {
		t.Fatalf("queued response = %#v", queued)
	}
	close(provider.release)
	await(func(record map[string]any) bool { return record["type"] == "agent_settled" })

	writeCommand(map[string]any{"id": "state-3", "type": "get_status"})
	settled := await(responseID("state-3"))["data"].(map[string]any)
	if settled["isStreaming"] != false || settled["messageCount"] != float64(4) || settled["pendingMessageCount"] != float64(0) {
		t.Fatalf("settled state = %#v", settled)
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	responseIndex, startIndex, updates := -1, -1, 0
	for index, record := range seen {
		if record["type"] == "response" && record["id"] == "prompt-1" {
			responseIndex = index
		}
		if record["type"] == "agent_start" && startIndex < 0 {
			startIndex = index
		}
		if record["type"] == "message_update" {
			if _, ok := record["assistantMessageEvent"].(map[string]any); !ok {
				t.Fatalf("message update = %#v", record)
			}
			updates++
		}
	}
	if responseIndex < 0 || startIndex <= responseIndex || updates == 0 {
		t.Fatalf("event ordering response=%d start=%d updates=%d records=%#v", responseIndex, startIndex, updates, seen)
	}
}

func TestServerSessionEntriesUseConfiguredSession(t *testing.T) {
	current, err := session.New(t.TempDir(), "/work", "test", "model")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	server := New(strings.NewReader(""), io.Discard, "/work")
	server.SetSession(current)
	if err := server.AppendSessionEntry("example", map[string]any{"value": "saved"}); err != nil {
		t.Fatal(err)
	}
	entries, err := server.SessionEntries("example")
	if err != nil || len(entries) != 1 || !strings.Contains(string(entries[0]), `"saved"`) {
		t.Fatalf("entries = %q, %v", entries, err)
	}
}

func TestReadRecordStrictJSONL(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("one\r\ntwo\npartial"))
	first, err := readRecord(reader)
	if err != nil || string(first) != "one" {
		t.Fatalf("first = %q, %v", first, err)
	}
	second, err := readRecord(reader)
	if err != nil || string(second) != "two" {
		t.Fatalf("second = %q, %v", second, err)
	}
	if _, err := readRecord(reader); !errors.Is(err, errUnterminatedLine) {
		t.Fatalf("unterminated error = %v", err)
	}
}

func decodeRecords(reader io.Reader, output chan<- map[string]any) {
	defer close(output)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) == nil {
			output <- record
		}
	}
}
