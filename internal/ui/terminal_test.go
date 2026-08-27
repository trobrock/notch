package ui

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/session"
)

func TestTerminalSetActiveToolsUsesConfiguredRegistry(t *testing.T) {
	terminal := NewTerminal(strings.NewReader(""), io.Discard, io.Discard, "/work")
	if err := terminal.SetActiveTools([]string{"read"}); err == nil {
		t.Fatal("unconfigured terminal changed active tools")
	}
	registry := extension.NewRegistry()
	if err := registry.RegisterTool(extension.Tool{Definition: model.ToolDefinition{Name: "read"}, Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
		return extension.ToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	terminal.SetRegistry(registry)
	if err := terminal.SetActiveTools([]string{"read"}); err != nil {
		t.Fatal(err)
	}
	if got := registry.ActiveToolNames(); len(got) != 1 || got[0] != "read" {
		t.Fatalf("active tools = %v", got)
	}
}

func TestTerminalSessionEntriesUseConfiguredSession(t *testing.T) {
	current, err := session.New(t.TempDir(), "/work", "test", "model")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	terminal := NewTerminal(strings.NewReader(""), io.Discard, io.Discard, "/work")
	terminal.SetSession(current)
	if err := terminal.AppendSessionEntry("example", map[string]any{"value": "saved"}); err != nil {
		t.Fatal(err)
	}
	entries, err := terminal.SessionEntries("example")
	if err != nil || len(entries) != 1 || !strings.Contains(string(entries[0]), `"saved"`) {
		t.Fatalf("entries = %q, %v", entries, err)
	}
}
