package extension

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/model"
)

func TestRunHooksBestEffortContinuesAfterErrors(t *testing.T) {
	registry := NewRegistry()
	var called []string
	registry.On("session_shutdown", "broken", func(context.Context, map[string]any) (map[string]any, error) {
		called = append(called, "broken")
		return nil, errors.New("failed")
	})
	registry.On("session_shutdown", "cleanup", func(_ context.Context, event map[string]any) (map[string]any, error) {
		called = append(called, "cleanup")
		if event["value"] != "initial" {
			t.Fatalf("event = %#v", event)
		}
		return map[string]any{"cleaned": true}, nil
	})
	result, err := registry.RunHooksBestEffort(context.Background(), "session_shutdown", map[string]any{"value": "initial"})
	if err == nil || !reflect.DeepEqual(called, []string{"broken", "cleanup"}) {
		t.Fatalf("called = %#v, err = %v", called, err)
	}
	if result["cleaned"] != true {
		t.Fatalf("result = %#v", result)
	}
}

func TestRegistryToolRestrictions(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"read", "write", "bash"} {
		tool := Tool{
			Definition: model.ToolDefinition{Name: name},
			Execute: func(context.Context, json.RawMessage, func(string)) (ToolResult, error) {
				return ToolResult{}, nil
			},
		}
		if err := registry.RegisterTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	if missing := registry.RemoveTools([]string{"write", "missing"}); !reflect.DeepEqual(missing, []string{"missing"}) {
		t.Fatalf("remove missing = %#v", missing)
	}
	if missing := registry.RestrictTools([]string{"read", "other"}); !reflect.DeepEqual(missing, []string{"other"}) {
		t.Fatalf("restrict missing = %#v", missing)
	}
	registered := registry.Tools()
	if len(registered) != 1 || registered[0].Definition.Name != "read" {
		t.Fatalf("tools = %#v", registered)
	}
	if missing := registry.RestrictTools(nil); len(missing) != 0 || len(registry.Tools()) != 0 {
		t.Fatalf("clear = %#v / %#v", missing, registry.Tools())
	}
}

func TestRegistryActiveToolsCanBeRestored(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"read", "write"} {
		name := name
		if err := registry.RegisterTool(Tool{Definition: model.ToolDefinition{Name: name}, Execute: func(context.Context, json.RawMessage, func(string)) (ToolResult, error) { return ToolResult{}, nil }}); err != nil {
			t.Fatal(err)
		}
	}
	if missing := registry.SetActiveTools([]string{"read", "missing"}); !reflect.DeepEqual(missing, []string{"missing"}) {
		t.Fatalf("missing=%#v", missing)
	}
	if _, ok := registry.Tool("write"); ok {
		t.Fatal("inactive write tool remained visible")
	}
	if got := registry.ActiveToolNames(); !reflect.DeepEqual(got, []string{"read"}) {
		t.Fatalf("active=%#v", got)
	}
	registry.SetActiveTools(nil)
	if _, ok := registry.Tool("write"); !ok {
		t.Fatal("restore did not expose write")
	}
}

func TestLimitToolResultKeepsHeadAndTail(t *testing.T) {
	content := "HEAD" + strings.Repeat("x", MaxToolResultBytes) + "TAIL"
	result := LimitToolResult(ToolResult{Content: content})
	if !strings.HasPrefix(result.Content, "HEAD") || !strings.HasSuffix(result.Content, "TAIL") || !strings.Contains(result.Content, "tool output truncated") {
		t.Fatalf("result markers missing")
	}
	if len(result.Content) > MaxToolResultBytes+100 {
		t.Fatalf("result length=%d", len(result.Content))
	}
}
