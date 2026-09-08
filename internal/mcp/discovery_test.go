package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

func TestDiscoveryRevealsBoundedMatchesWithoutExecuting(t *testing.T) {
	r := extension.NewRegistry()
	for _, name := range []string{"mcp__demo__alpha", "mcp__demo__beta", "mcp__other__gamma"} {
		if err := r.RegisterTool(extension.Tool{Source: "mcp:test", Deferred: true, Definition: model.ToolDefinition{Name: name, Description: "Find issues " + strings.Repeat("界", 200), InputSchema: map[string]any{"type": "object"}}, Execute: func(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
			t.Error("discovery executed a target")
			return extension.ToolResult{}, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	search := discoveryTool(r)
	if err := r.RegisterTool(search); err != nil {
		t.Fatal(err)
	}
	result, err := search.Execute(context.Background(), json.RawMessage(`{"query":"DEMO issues","limit":1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Details["matches"] != 2 || result.Details["revealed"] != 1 || !strings.Contains(result.Content, "mcp__demo__alpha") || strings.Contains(result.Content, "mcp__demo__beta") {
		t.Fatalf("result=%#v", result)
	}
	if defs := r.Definitions(); len(defs) != 2 {
		t.Fatalf("definitions=%#v", defs)
	}
	r.RemoveTools([]string{"mcp__demo__beta"})
	result, err = search.Execute(context.Background(), json.RawMessage(`{"query":"beta"}`), nil)
	if err != nil || !strings.Contains(result.Content, "No available") {
		t.Fatalf("removed tool searchable: %#v %v", result, err)
	}
	r.SetActiveTools([]string{DiscoveryToolName})
	result, err = search.Execute(context.Background(), json.RawMessage(`{"query":"gamma"}`), nil)
	if err != nil || !strings.Contains(result.Content, "No available") {
		t.Fatalf("inactive tool searchable: %#v %v", result, err)
	}
}

func TestDiscoveryValidation(t *testing.T) {
	search := discoveryTool(extension.NewRegistry())
	for _, raw := range []string{`{`, `{}`, `{"query":" "}`, `{"query":"x","limit":-1}`, `{"query":"x","limit":6}`, `{"query":"` + strings.Repeat("x", 201) + `"}`} {
		if _, err := search.Execute(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := search.Execute(ctx, json.RawMessage(`{"query":"x"}`), nil); err == nil {
		t.Fatal("ignored cancellation")
	}
}
