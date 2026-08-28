package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

func BenchmarkPromptParallelToolCalls(b *testing.B) {
	registry := extension.NewRegistry()
	if err := registry.RegisterTool(extension.Tool{
		Definition: model.ToolDefinition{Name: "delayed_benchmark", InputSchema: map[string]any{"type": "object"}},
		Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
			time.Sleep(10 * time.Millisecond)
			return extension.ToolResult{Content: "done"}, nil
		},
	}); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		runner, err := New(Config{Provider: &benchmarkParallelProvider{}, Registry: registry, Model: "benchmark"})
		if err != nil {
			b.Fatal(err)
		}
		if err := runner.Prompt(context.Background(), "run independent operations", nil); err != nil {
			b.Fatal(err)
		}
	}
}

type benchmarkParallelProvider struct{ calls int }

func (p *benchmarkParallelProvider) Stream(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
	p.calls++
	if p.calls == 1 {
		return model.Response{Content: []model.Block{
			{Type: "tool_use", ID: "call-1", Name: "delayed_benchmark", Arguments: json.RawMessage(`{}`)},
			{Type: "tool_use", ID: "call-2", Name: "delayed_benchmark", Arguments: json.RawMessage(`{}`)},
			{Type: "tool_use", ID: "call-3", Name: "delayed_benchmark", Arguments: json.RawMessage(`{}`)},
		}, StopReason: "tool_use"}, nil
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
}

// BenchmarkPromptLongToolLoop exercises the local overhead of a complex task
// with many model/tool turns while keeping provider and tool latency
// deterministic.
func BenchmarkPromptLongToolLoop(b *testing.B) {
	registry := extension.NewRegistry()
	if err := registry.RegisterTool(extension.Tool{
		Definition: model.ToolDefinition{
			Name:        "continue_benchmark",
			Description: "Return a moderately sized deterministic result.",
			InputSchema: map[string]any{"type": "object"},
		},
		Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
			return extension.ToolResult{Content: "continue with the next step: " + benchmarkToolResult}, nil
		},
	}); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		provider := &benchmarkLoopProvider{}
		runner, err := New(Config{Provider: provider, Registry: registry, Model: "benchmark"})
		if err != nil {
			b.Fatal(err)
		}
		if err := runner.Prompt(context.Background(), "complete the benchmark task", nil); err != nil {
			b.Fatal(err)
		}
	}
}

const benchmarkToolResult = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

type benchmarkLoopProvider struct{ calls int }

func (p *benchmarkLoopProvider) Stream(context.Context, model.Request, func(model.StreamEvent)) (model.Response, error) {
	p.calls++
	if p.calls <= 50 {
		return model.Response{
			Content: []model.Block{{
				Type:      "tool_use",
				ID:        fmt.Sprintf("call-%d", p.calls),
				Name:      "continue_benchmark",
				Arguments: json.RawMessage(`{}`),
			}},
			StopReason: "tool_use",
		}, nil
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
}
