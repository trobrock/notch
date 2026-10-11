package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/model"
)

func TestEstimateContextMessagesMatchesCompactedMessages(t *testing.T) {
	largeUnicode := strings.Repeat("é", 10000)
	messages := []model.Message{
		{Role: "user", Content: []model.Block{{Type: "text", Text: "run tools"}}},
		{Role: "assistant", Content: []model.Block{{Type: "tool_use", ID: "1", Name: "read", Arguments: []byte(`{"path":"one"}`)}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", Text: largeUnicode, ToolUseID: "1"}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", Text: largeUnicode + "a", ToolUseID: "2"}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", Text: largeUnicode + "ab", ToolUseID: "3"}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", Text: largeUnicode + "abc", ToolUseID: "4"}}},
	}
	got := estimateContextMessages(messages)
	want := estimateMessages(contextMessages(messages))
	if got != want {
		t.Fatalf("estimateContextMessages() = %d, want %d", got, want)
	}
}

func TestEstimateMessageIncludesProviderSignature(t *testing.T) {
	without := estimateMessage(model.Message{Role: "assistant", Content: []model.Block{{Type: "thinking", Text: "reasoning"}}})
	with := estimateMessage(model.Message{Role: "assistant", Content: []model.Block{{Type: "thinking", Text: "reasoning", Signature: strings.Repeat("s", 300)}}})
	if with-without != 100 {
		t.Fatalf("signature token estimate delta = %d, want 100", with-without)
	}
}

func TestCompactionConversationJSONBoundsPayloadAndDropsSignatures(t *testing.T) {
	messages := make([]model.Message, 200)
	for i := range messages {
		messages[i] = model.Message{Role: "assistant", Content: []model.Block{{
			Type: "thinking", Text: fmt.Sprintf("message-%03d %s", i, strings.Repeat("x", 4000)), Signature: strings.Repeat("s", 12000),
		}}}
	}

	got, err := compactionConversationJSON(messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxCompactionConversationBytes {
		t.Fatalf("compaction JSON is %d bytes, limit is %d", len(got), maxCompactionConversationBytes)
	}
	if strings.Contains(string(got), strings.Repeat("s", 100)) {
		t.Fatal("compaction JSON retained an opaque provider signature")
	}
	if !strings.Contains(string(got), "notch compaction omitted") || !strings.Contains(string(got), "message-000") || !strings.Contains(string(got), "message-199") {
		t.Fatalf("bounded payload did not preserve the beginning, omission marker, and end")
	}
	if messages[0].Content[0].Signature == "" {
		t.Fatal("durable messages were mutated")
	}
}

func TestCompactionConversationJSONTrimsSingleOversizedMessage(t *testing.T) {
	messages := []model.Message{{Role: "user", Content: []model.Block{
		{Type: "text", Text: "START-" + strings.Repeat("x", maxCompactionConversationBytes) + "-END"},
		{Type: "tool_use", Name: "edit", Arguments: json.RawMessage(`{"body":"` + strings.Repeat("y", maxCompactionConversationBytes) + `"}`)},
	}}}
	got, err := compactionConversationJSON(messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxCompactionConversationBytes {
		t.Fatalf("compaction JSON is %d bytes, limit is %d", len(got), maxCompactionConversationBytes)
	}
	for _, want := range []string{"START-", "-END", "block text trimmed", "tool arguments trimmed"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("bounded message lost %q", want)
		}
	}
}

func TestContextMessagesRepairsInterruptedToolCalls(t *testing.T) {
	messages := []model.Message{
		model.TextMessage("user", "start"),
		{Role: "assistant", Content: []model.Block{
			{Type: "function_call", ID: "missing", Name: "ask_user_question", Arguments: json.RawMessage(`{"question":"Choose"}`)},
			{Type: "tool_use", ID: "done", Name: "read", Arguments: json.RawMessage(`{"path":"one"}`)},
		}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", ToolUseID: "done", Text: "ok"}}},
		model.TextMessage("user", "my answer"),
	}

	got := contextMessages(messages)
	if len(got) != len(messages)+1 {
		t.Fatalf("context messages = %d, want %d", len(got), len(messages)+1)
	}
	repair := got[2]
	if repair.Role != "user" || len(repair.Content) != 1 {
		t.Fatalf("repair message = %#v", repair)
	}
	block := repair.Content[0]
	if block.Type != "tool_result" || block.ToolUseID != "missing" || !block.IsError || block.Text != interruptedToolResult {
		t.Fatalf("repair block = %#v", block)
	}
	if got[3].Content[0].ToolUseID != "done" || got[4].Content[0].Text != "my answer" {
		t.Fatalf("repair was inserted in the wrong place: %#v", got)
	}
	if len(messages) != 4 || messages[2].Content[0].ToolUseID != "done" {
		t.Fatal("durable messages were mutated")
	}
}

func TestContextMessagesDoesNotRepairCompletedToolCalls(t *testing.T) {
	messages := []model.Message{
		{Role: "assistant", Content: []model.Block{{Type: "tool_use", ID: "done", Name: "read"}}},
		{Role: "user", Content: []model.Block{{Type: "function_call_output", ToolUseID: "done", Text: "ok"}}},
	}
	got := contextMessages(messages)
	if len(got) != len(messages) {
		t.Fatalf("context messages = %#v", got)
	}
}

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
