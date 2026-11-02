package claudeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/model"
)

func TestStreamTextThroughClaudeProtocol(t *testing.T) {
	script := writeFakeClaude(t, `
read init
echo '{"type":"control_response","response":{"subtype":"success","request_id":"notch-init","response":{}}}'
read user
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":3,"output_tokens":1}}}'
echo '{"type":"result","subtype":"success","result":"hello","usage":{"input_tokens":3,"output_tokens":1},"stop_reason":"end_turn"}'
`)
	p := New(Config{Executable: script, Env: os.Environ()})
	var streamed strings.Builder
	response, err := p.Stream(context.Background(), model.Request{Model: "sonnet", SystemPrompt: "Be clear.", Messages: []model.Message{model.TextMessage("user", "hi")}}, func(event model.StreamEvent) {
		if event.Type == "text_delta" {
			streamed.WriteString(event.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "hello" || len(response.Content) != 1 || response.Content[0].Text != "hello" {
		t.Fatalf("stream = %q, response = %#v", streamed.String(), response)
	}
	if response.InputTokens != 3 || response.OutputTokens != 1 || response.StopReason != "end_turn" {
		t.Fatalf("usage/stop = %#v", response)
	}
}

func TestStreamPausesForNotchToolResult(t *testing.T) {
	script := writeFakeClaude(t, `
read init
echo '{"type":"control_response","response":{"subtype":"success","request_id":"notch-init","response":{}}}'
read user
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"mcp__notch__read","input":{"path":"README.md"}}],"usage":{"input_tokens":4,"output_tokens":2}}}'
echo '{"type":"control_request","request_id":"hook-1","request":{"subtype":"hook_callback","callback_id":"notch-pre","input":{"hook_event_name":"PreToolUse","tool_use_id":"tool-1","tool_name":"mcp__notch__read"}}}'
read hook_response
echo '{"type":"stream_event","event":{"type":"message_stop"}}'
echo '{"type":"control_request","request_id":"mcp-1","request":{"subtype":"mcp_message","server_name":"notch","message":{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read","arguments":{"path":"README.md"}}}}}'
read mcp_response
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}],"usage":{"input_tokens":8,"output_tokens":3}}}'
echo '{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":8,"output_tokens":3},"stop_reason":"end_turn"}'
`)
	p := New(Config{Executable: script, Env: os.Environ()})
	request := model.Request{
		Model: "sonnet", Messages: []model.Message{model.TextMessage("user", "read it")},
		Tools: []model.ToolDefinition{{Name: "read", Description: "Read a file", InputSchema: map[string]any{"type": "object"}}},
	}
	first, err := p.Stream(context.Background(), request, func(model.StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if first.StopReason != "tool_use" || len(first.Content) != 1 || first.Content[0].Name != "read" || first.Content[0].ID != "tool-1" {
		t.Fatalf("first response = %#v", first)
	}
	if first.InputTokens != 4 || first.OutputTokens != 2 {
		t.Fatalf("first usage = %#v", first)
	}
	request.Messages = append(request.Messages,
		model.Message{Role: "assistant", Content: first.Content},
		model.Message{Role: "user", Content: []model.Block{{Type: "tool_result", ToolUseID: "tool-1", Text: "file text"}}},
	)
	second, err := p.Stream(context.Background(), request, func(model.StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if second.StopReason != "end_turn" || len(second.Content) != 1 || second.Content[0].Text != "done" {
		t.Fatalf("second response = %#v", second)
	}
	if second.InputTokens != 4 || second.OutputTokens != 1 {
		t.Fatalf("second usage should exclude the accounted tool step: %#v", second)
	}
}

func TestParallelAssistantEnvelopesAreCombinedAndUsageCountedOnce(t *testing.T) {
	script := writeFakeClaude(t, `
read init
echo '{"type":"control_response","response":{"subtype":"success","request_id":"notch-init","response":{}}}'
read user
echo '{"type":"assistant","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tool-1","name":"mcp__notch__odd_name_919b02c38c","input":{}}],"usage":{"input_tokens":4,"output_tokens":2}}}'
echo '{"type":"assistant","message":{"id":"msg-1","content":[{"type":"tool_use","id":"tool-2","name":"mcp__notch__odd_name_919b02c38c","input":{}}],"usage":{"input_tokens":4,"output_tokens":2}}}'
echo '{"type":"stream_event","event":{"type":"message_delta","usage":{"input_tokens":4,"output_tokens":7}}}'
echo '{"type":"stream_event","event":{"type":"message_stop"}}'
read forever
`)
	provider := New(Config{Executable: script, Env: os.Environ()}).(*provider)
	toolName := "odd.name"
	response, err := provider.Stream(context.Background(), model.Request{
		Messages: []model.Message{model.TextMessage("user", "go")},
		Tools:    []model.ToolDefinition{{Name: toolName, InputSchema: map[string]any{"type": "object"}}},
	}, func(model.StreamEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.AbortPendingTurn()
	if len(response.Content) != 2 || response.InputTokens != 4 || response.OutputTokens != 7 {
		t.Fatalf("parallel response = %#v", response)
	}
	for _, block := range response.Content {
		if block.Name != toolName {
			t.Fatalf("mapped tool name = %q, want %q", block.Name, toolName)
		}
	}
}

func TestResultAPIErrorStatusIsFailure(t *testing.T) {
	script := writeFakeClaude(t, `
read init
echo '{"type":"control_response","response":{"subtype":"success","request_id":"notch-init","response":{}}}'
read user
echo '{"type":"result","subtype":"success","api_error_status":529}'
`)
	p := New(Config{Executable: script, Env: os.Environ()})
	_, err := p.Stream(context.Background(), model.Request{Messages: []model.Message{model.TextMessage("user", "hi")}}, func(model.StreamEvent) {})
	if err == nil || !strings.Contains(err.Error(), "529") {
		t.Fatalf("error = %v", err)
	}
}

func TestResultErrorUsesErrorsArray(t *testing.T) {
	script := writeFakeClaude(t, `
read init
echo '{"type":"control_response","response":{"subtype":"success","request_id":"notch-init","response":{}}}'
read user
echo '{"type":"result","subtype":"error_during_execution","errors":["login required"]}'
`)
	p := New(Config{Executable: script, Env: os.Environ()})
	_, err := p.Stream(context.Background(), model.Request{Messages: []model.Message{model.TextMessage("user", "hi")}}, func(model.StreamEvent) {})
	if err == nil || !strings.Contains(err.Error(), "login required") {
		t.Fatalf("error = %v", err)
	}
}

func TestInitialPromptReplaysTranscriptWithoutThinking(t *testing.T) {
	prompt := initialPrompt([]model.Message{
		model.TextMessage("user", "first"),
		{Role: "assistant", Content: []model.Block{{Type: "thinking", Text: "secret"}, {Type: "text", Text: "answer"}}},
		model.TextMessage("user", "next"),
	})
	if !strings.Contains(prompt, "first") || !strings.Contains(prompt, "answer") || !strings.Contains(prompt, "next") || strings.Contains(prompt, "secret") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func writeFakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
