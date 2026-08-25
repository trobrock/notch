package ui

import (
	"bytes"
	"testing"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
)

func TestTerminalSanitizerRemovesEscapeAndControlSequences(t *testing.T) {
	var sanitizer terminalSanitizer
	parts := []string{
		"safe\x1b[31", "mred\x1b[0m ",
		"\x1b]0;owned", " title\a",
		"\x1b]8;;https://evil.test\x1b\\link\x1b]8;;\x1b\\",
		"\x00\r\b\u009b2J\tkept\n",
	}
	var got string
	for _, part := range parts {
		got += sanitizer.clean(part)
	}
	if want := "safered link\tkept\n"; got != want {
		t.Fatalf("sanitized text = %q, want %q", got, want)
	}
}

func TestTerminalRedirectedOutputIsPreserved(t *testing.T) {
	var out, errOut bytes.Buffer
	terminal := NewTerminal(bytes.NewReader(nil), &out, &errOut, "")
	payload := "raw\x1b[31m\x00\r"
	terminal.Render(agent.Event{Type: "text_delta", Text: payload})
	terminal.Notify(payload, "warn")
	terminal.Render(agent.Event{Type: "tool_update", Text: payload})
	terminal.Render(agent.Event{Type: "tool_end", Result: &extension.ToolResult{Content: payload, IsError: true}})
	if out.String() != payload {
		t.Fatalf("redirected stdout = %q, want %q", out.String(), payload)
	}
	if got := errOut.String(); got != "[warn] "+payload+"\n  "+payload+"\n  error: "+payload+"\n" {
		t.Fatalf("redirected stderr = %q", got)
	}
}

func TestTerminalTTYOutputIsSanitized(t *testing.T) {
	var out bytes.Buffer
	terminal := NewTerminal(bytes.NewReader(nil), &out, &out, "")
	// NewTerminal derives this from *os.File descriptors in production. Mark
	// this in-memory destination as a TTY to exercise streaming sanitizer state.
	terminal.outTTY = true
	terminal.Render(agent.Event{Type: "text_delta", Text: "ok\x1b[31"})
	terminal.Render(agent.Event{Type: "text_delta", Text: "mred\x1b[0m\x00"})
	terminal.ReadPrompt("prompt\x1b[2J> ")
	if got := out.String(); got != "okredprompt> " {
		t.Fatalf("TTY output = %q", got)
	}
}
