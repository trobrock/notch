package explore

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/delegation"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/officialext/subagent"
)

type fakeRunner struct {
	mu                sync.Mutex
	inputs            []subagent.Input
	active, maxActive int
	usageWallMS       int64
}

func (r *fakeRunner) Run(_ context.Context, input subagent.Input, _ func(string)) (subagent.Result, error) {
	r.mu.Lock()
	r.inputs = append(r.inputs, input)
	r.active++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	wallMS := r.usageWallMS
	if wallMS == 0 {
		wallMS = 7
	}
	return subagent.Result{Output: "found " + input.Prompt, Usage: subagent.Usage{Input: 10, Output: 2, WallMS: wallMS, Model: input.Model}}, nil
}

func TestExploreSchemaAndDescriptionGuideCorrectUse(t *testing.T) {
	registry, runner := extension.NewRegistry(), &fakeRunner{}
	if err := RegisterWithRunner(registry, runner); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	if !strings.Contains(tool.Definition.Description, "save parent context") || !strings.Contains(tool.Definition.Description, "tasks array") || !strings.Contains(tool.Definition.Description, "avoid delegation") {
		t.Fatalf("description = %q", tool.Definition.Description)
	}
	schema := tool.Definition.InputSchema
	for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
		if _, present := schema[keyword]; present {
			t.Fatalf("Anthropic-incompatible top-level %s = %#v", keyword, schema[keyword])
		}
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || properties["tasks"] == nil || properties["task"] != nil {
		t.Fatalf("schema should expose one unambiguous tasks mode: %#v", schema)
	}
	if required, ok := schema["required"].([]string); !ok || !reflect.DeepEqual(required, []string{"tasks"}) {
		t.Fatalf("required = %#v", schema["required"])
	}
}

func TestExploreSingleTaskUsesReadOnlyRunner(t *testing.T) {
	registry, runner := extension.NewRegistry(), &fakeRunner{}
	if err := RegisterWithRunner(registry, runner); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"find parser","model":"test/model","cwd":"/work"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content != "found Explore task: find parser" {
		t.Fatalf("result = %#v", result)
	}
	input := runner.inputs[0]
	if input.Tools != "read,grep,find,ls" || input.Thinking != subagent.DefaultThinking || input.Model != "test/model" || input.CWD != "/work" || !strings.Contains(input.SystemPrompt, "read-only") {
		t.Fatalf("input = %#v", input)
	}
}

func TestExploreParallelPreservesOrderAndLimitsConcurrency(t *testing.T) {
	registry, runner := extension.NewRegistry(), &fakeRunner{}
	if err := RegisterWithRunner(registry, runner); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	var updates []string
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"tasks":[
		{"task":"one"},{"task":"two"},{"task":"three"},{"task":"four"},{"task":"five"}
	]}`), func(s string) { updates = append(updates, s) })
	if err != nil {
		t.Fatal(err)
	}
	if runner.maxActive > maxConcurrency || !strings.Contains(result.Content, "Exploration 1: one") || !strings.Contains(result.Content, "Exploration 5: five") {
		t.Fatalf("max=%d result=%q", runner.maxActive, result.Content)
	}
	if len(updates) != 5 || result.Details["count"] != 5 {
		t.Fatalf("updates=%#v details=%#v", updates, result.Details)
	}
	usage, ok := result.Details["delegated_usage"].(delegation.Usage)
	if !ok || usage.Calls != 5 || usage.InputTokens != 50 || usage.OutputTokens != 10 || usage.Turns != 0 || usage.WallMS <= 0 {
		t.Fatalf("delegated usage = %#v", result.Details["delegated_usage"])
	}
}

func TestExploreParallelWallTimeUsesBatchElapsed(t *testing.T) {
	registry := extension.NewRegistry()
	runner := &fakeRunner{usageWallMS: 1000}
	if err := RegisterWithRunner(registry, runner); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"tasks":[{"task":"one"},{"task":"two"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	usage := result.Details["delegated_usage"].(delegation.Usage)
	if usage.WallMS >= 1000 {
		t.Fatalf("wall_ms should be batch elapsed rather than summed child duration, got %#v", usage)
	}
}

func TestExploreValidatesModes(t *testing.T) {
	for _, raw := range []string{`{}`, `{"tasks":[]}`, `{"tasks":[{"task":" "}]}`, `{"unknown":true}`} {
		if _, err := decode(json.RawMessage(raw)); err == nil {
			t.Fatalf("decode(%s) succeeded", raw)
		}
	}
}

func TestExplorePrefersLegacySingleTaskWhenProviderAlsoSendsTasks(t *testing.T) {
	input, err := decode(json.RawMessage(`{"task":"find parser","tasks":[{"task":"placeholder"}],"model":"test/model","cwd":"/work"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Input{Task: "find parser", Tasks: []Task{{Task: "find parser"}}, Model: "test/model", CWD: "/work"}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("input=%#v, want %#v", input, want)
	}
}

func TestExploreTaskOverridesDefaults(t *testing.T) {
	input, err := decode(json.RawMessage(`{"tasks":[{"task":"one","model":"task/model","cwd":"/task"}],"model":"default/model","cwd":"/default"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input.Tasks, []Task{{Task: "one", Model: "task/model", CWD: "/task"}}) {
		t.Fatalf("input=%#v", input)
	}
}
