package extension

import (
	"context"
	"testing"

	"github.com/trobrock/notch/internal/model"
)

func TestContextToolResultsSnapshot(t *testing.T) {
	messages := []model.Message{
		{Role: "assistant", Content: []model.Block{{Type: "tool_use", ID: "load", Name: "skill"}}},
		{Role: "user", Content: []model.Block{
			{Type: "tool_result", ToolUseID: "load", Text: "instructions"},
			{Type: "tool_result", ToolUseID: "load", Text: "failed", IsError: true},
			{Type: "tool_result", ToolUseID: "missing", Text: "orphan"},
		}},
	}
	ctx := WithContextToolResults(context.Background(), messages)
	messages[0].Content[0].Name = "other"
	messages[1].Content[0].Text = "changed"
	if !HasContextToolResult(ctx, "skill", "instructions") {
		t.Fatal("result snapshot changed")
	}
	for _, text := range []string{"changed", "failed", "orphan"} {
		if HasContextToolResult(ctx, "skill", text) {
			t.Errorf("unexpected result %q", text)
		}
	}
	if HasContextToolResult(context.Background(), "skill", "instructions") {
		t.Fatal("result leaked across contexts")
	}
	if HasContextToolResult(WithContextToolResults(ctx, nil), "skill", "instructions") {
		t.Fatal("result retained after replacement")
	}
}

func TestContextToolResultsLegacyBlocks(t *testing.T) {
	ctx := WithContextToolResults(context.Background(), []model.Message{
		{Role: "assistant", Content: []model.Block{{Type: "function_call", ID: "load", Name: "skill"}}},
		{Role: "tool", Content: []model.Block{{Type: "function_call_output", ToolUseID: "load", Text: "instructions"}}},
	})
	if !HasContextToolResult(ctx, "skill", "instructions") {
		t.Fatal("legacy result not recognized")
	}
}
