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

func TestRegisterBatchIsAtomicAndLeaseOwnsEntries(t *testing.T) {
	registry := NewRegistry()
	executeTool := func(context.Context, json.RawMessage, func(string)) (ToolResult, error) {
		return ToolResult{}, nil
	}
	executeCommand := func(context.Context, string) (string, error) { return "", nil }
	if err := registry.RegisterTool(Tool{Definition: model.ToolDefinition{Name: "taken"}, Source: "existing", Execute: executeTool}); err != nil {
		t.Fatal(err)
	}

	lease, err := registry.RegisterBatch(Batch{
		Tools:    []Tool{{Definition: model.ToolDefinition{Name: "taken"}, Source: "batch", Execute: executeTool}},
		Commands: []Command{{Name: "new", Source: "batch", Execute: executeCommand}},
		Hooks: []HookRegistration{{Event: "event", Source: "batch", Handler: func(context.Context, map[string]any) (map[string]any, error) {
			return map[string]any{"called": true}, nil
		}}},
	})
	if err == nil || lease != nil {
		t.Fatalf("RegisterBatch lease=%v err=%v", lease, err)
	}
	if _, ok := registry.Command("new"); ok {
		t.Fatal("failed batch left a command registered")
	}
	result, err := registry.RunHooks(context.Background(), "event", nil)
	if err != nil || result["called"] != nil {
		t.Fatalf("failed batch left a hook registered: result=%v err=%v", result, err)
	}

	lease, err = registry.RegisterBatch(Batch{
		Tools:    []Tool{{Definition: model.ToolDefinition{Name: "owned"}, Source: "batch", Execute: executeTool}},
		Commands: []Command{{Name: "owned", Source: "batch", Execute: executeCommand}},
		Hooks: []HookRegistration{{Event: "event", Source: "batch", Handler: func(context.Context, map[string]any) (map[string]any, error) {
			return map[string]any{"called": true}, nil
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Tool("owned"); !ok {
		t.Fatal("batch tool was not registered")
	}
	if _, ok := registry.Command("owned"); !ok {
		t.Fatal("batch command was not registered")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := registry.Tool("owned"); ok {
		t.Fatal("owned tool remained after Close")
	}
	if _, ok := registry.Command("owned"); ok {
		t.Fatal("owned command remained after Close")
	}
	result, err = registry.RunHooks(context.Background(), "event", nil)
	if err != nil || result["called"] != nil {
		t.Fatalf("owned hook remained after Close: result=%v err=%v", result, err)
	}
	if _, ok := registry.Tool("taken"); !ok {
		t.Fatal("Close removed an entry owned by another registration")
	}
}

func TestRegistrationCloseDoesNotRemoveReplacement(t *testing.T) {
	registry := NewRegistry()
	handler := func(context.Context, json.RawMessage, func(string)) (ToolResult, error) { return ToolResult{}, nil }
	lease, err := registry.RegisterBatch(Batch{Tools: []Tool{{Definition: model.ToolDefinition{Name: "replace"}, Source: "owner", Execute: handler}}})
	if err != nil {
		t.Fatal(err)
	}
	registry.RemoveTools([]string{"replace"})
	if err := registry.RegisterTool(Tool{Definition: model.ToolDefinition{Name: "replace"}, Source: "replacement", Execute: handler}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Tool("replace")
	if !ok || tool.Source != "replacement" {
		t.Fatalf("replacement was removed: %#v, %v", tool, ok)
	}
}

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

func TestDeferredToolVisibilityPreservesAccessPolicy(t *testing.T) {
	makeRegistry := func() *Registry {
		r := NewRegistry()
		for _, name := range []string{"one", "two"} {
			if err := r.RegisterTool(Tool{Definition: model.ToolDefinition{Name: name}, Deferred: true, Execute: func(context.Context, json.RawMessage, func(string)) (ToolResult, error) { return ToolResult{}, nil }}); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	t.Run("reveal-is-not-permission", func(t *testing.T) {
		r := makeRegistry()
		if len(r.Definitions()) != 0 || len(r.Tools()) != 2 {
			t.Fatal("deferred schemas were advertised or tools lost")
		}
		if _, ok := r.Tool("one"); !ok {
			t.Fatal("deferral must not disable execution")
		}
		if missing := r.RevealTools([]string{"one"}); len(missing) != 0 {
			t.Fatal(missing)
		}
		if defs := r.Definitions(); len(defs) != 1 || defs[0].Name != "one" {
			t.Fatal(defs)
		}
		r.RemoveTools([]string{"two"})
		if missing := r.RevealTools([]string{"two"}); !reflect.DeepEqual(missing, []string{"two"}) {
			t.Fatal(missing)
		}
	})
	t.Run("explicit-allowlist", func(t *testing.T) {
		r := makeRegistry()
		r.RestrictTools([]string{"one"})
		if defs := r.Definitions(); len(defs) != 1 || defs[0].Name != "one" {
			t.Fatal(defs)
		}
		if missing := r.RevealTools([]string{"two"}); len(missing) != 1 {
			t.Fatal("allowlist bypass")
		}
	})
	t.Run("active-filter", func(t *testing.T) {
		r := makeRegistry()
		r.SetActiveTools([]string{"one"})
		if defs := r.Definitions(); len(defs) != 1 || defs[0].Name != "one" {
			t.Fatal(defs)
		}
		if missing := r.RevealTools([]string{"two"}); len(missing) != 1 {
			t.Fatal("active policy bypass")
		}
		r.SetActiveTools(nil)
		if defs := r.Definitions(); len(defs) != 1 {
			t.Fatal("inactive tool was revealed", defs)
		}
		r.RevealTools([]string{"two"})
		if len(r.Definitions()) != 2 {
			t.Fatal("failed to reveal restored tool")
		}
	})
}
