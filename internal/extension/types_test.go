package extension

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/trobrock/notch/internal/model"
)

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
