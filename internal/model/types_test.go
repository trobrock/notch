package model

import (
	"encoding/json"
	"testing"
)

func TestNormalizeToolCallArguments(t *testing.T) {
	blocks := []Block{
		{Type: "tool_use", ID: "empty", Arguments: json.RawMessage{}},
		{Type: "tool_use", ID: "cut-off", Arguments: json.RawMessage(`{"a":1`)},
	}

	invalid := NormalizeToolCallArguments(blocks)
	if invalid != 1 {
		t.Fatalf("invalid count = %d, want 1", invalid)
	}
	if string(blocks[0].Arguments) != `{}` || blocks[0].IsError {
		t.Fatalf("empty arguments = %q, error=%v", blocks[0].Arguments, blocks[0].IsError)
	}
	if string(blocks[1].Arguments) != `{}` || !blocks[1].IsError {
		t.Fatalf("cut-off arguments = %q, error=%v", blocks[1].Arguments, blocks[1].IsError)
	}
}

func TestRepairToolCallResultsDoesNotDuplicateExistingResult(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "bad", Name: "write", Arguments: json.RawMessage(`{}`), IsError: true}}},
		{Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: "bad", Text: "already recorded", IsError: true}}},
	}
	repaired, inserted := RepairToolCallResults(messages, ToolCallInputErrorMessage)
	if inserted != 0 || len(repaired) != 2 || len(repaired[1].Content) != 1 {
		t.Fatalf("repaired = %#v, inserted=%d", repaired, inserted)
	}

	repaired, inserted = RepairToolCallResults(messages[:1], ToolCallInputErrorMessage)
	if inserted != 1 || len(repaired) != 2 || repaired[1].Content[0].ToolUseID != "bad" {
		t.Fatalf("missing result repair = %#v, inserted=%d", repaired, inserted)
	}

	functionOutput, _ := CleanMessages(messages)
	functionOutput[1].Content[0].Type = "function_call_output"
	repaired, inserted = RepairToolCallResults(functionOutput, ToolCallInputErrorMessage)
	if inserted != 0 || len(repaired) != 2 {
		t.Fatalf("function output repair = %#v, inserted=%d", repaired, inserted)
	}

	wrongRole, _ := CleanMessages(messages)
	wrongRole[1].Role = "assistant"
	repaired, inserted = RepairToolCallResults(wrongRole, ToolCallInputErrorMessage)
	if inserted != 1 || len(repaired) != 3 || repaired[1].Role != "user" {
		t.Fatalf("wrong-role repair = %#v, inserted=%d", repaired, inserted)
	}

	mixed := []Message{{Role: "assistant", Content: []Block{
		{Type: "tool_use", ID: "valid", Arguments: json.RawMessage(`{}`)},
		{Type: "tool_use", ID: "invalid", Arguments: json.RawMessage(`{"cutoff"`), IsError: true},
	}}}
	repaired, inserted = RepairToolCallResults(mixed, ToolCallInputErrorMessage)
	if inserted != 2 || len(repaired) != 2 || len(repaired[1].Content) != 2 {
		t.Fatalf("mixed batch repair = %#v, inserted=%d", repaired, inserted)
	}
}
