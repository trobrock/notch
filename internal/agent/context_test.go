package agent

import (
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/model"
)

func TestContextMessagesTrimOlderToolResults(t *testing.T) {
	large := strings.Repeat("x", 20000)
	messages := []model.Message{
		{Role: "user", Content: []model.Block{{Type: "tool_result", Text: large, ToolUseID: "old"}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", Text: large, ToolUseID: "recent-1"}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", Text: large, ToolUseID: "recent-2"}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", Text: large, ToolUseID: "recent-3"}}},
	}
	got := contextMessages(messages)
	if len(got[0].Content[0].Text) > maxOldToolResultBytes+100 || !strings.Contains(got[0].Content[0].Text, "older tool result trimmed") {
		t.Fatalf("old result length=%d", len(got[0].Content[0].Text))
	}
	if len(got[3].Content[0].Text) > maxRecentToolResultBytes+100 {
		t.Fatalf("recent result length=%d", len(got[3].Content[0].Text))
	}
	if messages[0].Content[0].Text != large {
		t.Fatal("durable messages were mutated")
	}
}
