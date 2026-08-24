package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/modelregistry"
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

func TestSessionLifecycleUsesFreshShutdownContext(t *testing.T) {
	registry := extension.NewRegistry()
	var started, stopped bool
	registry.On("session_start", "test", func(_ context.Context, event map[string]any) (map[string]any, error) {
		started = event["session_id"] == "session-1"
		return nil, nil
	})
	registry.On("session_shutdown", "test", func(ctx context.Context, event map[string]any) (map[string]any, error) {
		if err := ctx.Err(); err != nil {
			t.Fatalf("shutdown context was already canceled: %v", err)
		}
		stopped = event["session_id"] == "session-1" && event["reason"] == "canceled"
		return nil, nil
	})
	parent, cancel := context.WithCancel(context.Background())
	shutdown, err := beginSessionLifecycle(parent, registry, map[string]any{"session_id": "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := shutdown("canceled"); err != nil {
		t.Fatal(err)
	}
	if !started || !stopped {
		t.Fatalf("started = %v, stopped = %v", started, stopped)
	}
}

func TestWriteModelListJSON(t *testing.T) {
	models := []modelregistry.Entry{{Provider: "anthropic", ID: "claude-test", Name: "Claude Test", ContextWindow: 200000, Reasoning: true, Source: "builtin"}}
	var output bytes.Buffer
	if err := writeModelList(&output, models, true); err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Version int                   `json:"version"`
		Models  []modelregistry.Entry `json:"models"`
	}
	if err := json.Unmarshal(output.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Version != 1 || !reflect.DeepEqual(catalog.Models, models) {
		t.Fatalf("catalog = %#v", catalog)
	}

	output.Reset()
	if err := writeModelList(&output, models, false); err != nil {
		t.Fatal(err)
	}
	if text := output.String(); !strings.Contains(text, "anthropic") || !strings.Contains(text, "claude-test") {
		t.Fatalf("text model list = %q", text)
	}
}

func TestValidThinkingLevel(t *testing.T) {
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh"} {
		if !validThinkingLevel(level) {
			t.Errorf("%q rejected", level)
		}
	}
	for _, level := range []string{"", "max", "HIGH"} {
		if validThinkingLevel(level) {
			t.Errorf("%q accepted", level)
		}
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
