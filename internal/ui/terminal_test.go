package ui

import (
	"io"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/session"
)

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
