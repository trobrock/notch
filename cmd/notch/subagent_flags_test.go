package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSystemPromptFileFlag(t *testing.T) {
	// Parsing and provider startup are composed in run, so verify the mutually
	// exclusive validation without requiring credentials.
	path := filepath.Join(t.TempDir(), "SYSTEM.md")
	if err := os.WriteFile(path, []byte("subagent prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--system-prompt", "inline", "--system-prompt-file", path, "--print", "test"}); err == nil {
		t.Fatal("combined system prompt flags succeeded")
	}
}
