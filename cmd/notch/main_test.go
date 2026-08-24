package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

func TestToolPolicy(t *testing.T) {
	registry := extension.NewRegistry()
	for _, name := range []string{"read", "write", "bash"} {
		if err := registry.RegisterTool(extension.Tool{
			Definition: model.ToolDefinition{Name: name},
			Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
				return extension.ToolResult{}, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyToolPolicy(registry, options{toolAllow: "read, bash", toolExclude: "bash"}); err != nil {
		t.Fatal(err)
	}
	registered := registry.Tools()
	if len(registered) != 1 || registered[0].Definition.Name != "read" {
		t.Fatalf("tools = %#v", registered)
	}
	if err := applyToolPolicy(registry, options{toolAllow: "missing"}); err == nil {
		t.Fatal("missing allowlisted tool succeeded")
	}

	names, err := parseToolNames(" bash,read,bash ")
	if err != nil || !reflect.DeepEqual(names, []string{"bash", "read"}) {
		t.Fatalf("names = %#v, %v", names, err)
	}
	if _, err := parseToolNames("read,,bash"); err == nil {
		t.Fatal("empty tool name succeeded")
	}
}

func TestRPCAPIForProvider(t *testing.T) {
	want := map[string]string{"anthropic": "anthropic-messages", "openrouter": "openai-completions", "openai-codex": "openai-codex-responses", "openai": "openai-responses"}
	for provider, api := range want {
		if got := rpcAPIForProvider(provider); got != api {
			t.Errorf("%s API = %q", provider, got)
		}
	}
}

func TestCurrentBuildInfoUsesInjectedValues(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := version, commit, buildDate
	version, commit, buildDate = "v1.2.3", "abc123", "2026-01-02T03:04:05Z"
	t.Cleanup(func() { version, commit, buildDate = oldVersion, oldCommit, oldBuildDate })

	info := currentBuildInfo()
	if info.Version != "v1.2.3" || info.Commit != "abc123" || info.BuildDate != "2026-01-02T03:04:05Z" {
		t.Fatalf("build info = %#v", info)
	}
	if info.GoVersion == "" || info.Platform == "" {
		t.Fatalf("incomplete build info = %#v", info)
	}
}
