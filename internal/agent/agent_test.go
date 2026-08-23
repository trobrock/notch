package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

type fakeProvider struct{ calls int }

func (f *fakeProvider) Stream(_ context.Context, req model.Request, emit func(model.StreamEvent)) (model.Response, error) {
	f.calls++
	if f.calls == 1 {
		return model.Response{Content: []model.Block{{Type: "tool_use", ID: "1", Name: "echo", Arguments: json.RawMessage(`{"value":"hello"}`)}}, StopReason: "tool_use"}, nil
	}
	emit(model.StreamEvent{Type: "text_delta", Text: "done"})
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
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
	var text string
	if err := a.Prompt(context.Background(), "go", func(e Event) {
		if e.Type == "text_delta" {
			text += e.Text
		}
	}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d", provider.calls)
	}
	if text != "done" {
		t.Fatalf("text = %q", text)
	}
	messages := a.Messages()
	if len(messages) != 4 || messages[2].Content[0].Type != "tool_result" {
		t.Fatalf("unexpected messages: %#v", messages)
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
