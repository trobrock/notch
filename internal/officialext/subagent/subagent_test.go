package subagent

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/delegation"
	"github.com/trobrock/notch/internal/extension"
)

type fakeRunner struct {
	input  Input
	result Result
	err    error
}

func (r *fakeRunner) Run(_ context.Context, input Input, update func(string)) (Result, error) {
	r.input = input
	if update != nil {
		update("running")
	}
	return r.result, r.err
}

func TestRunSubagentDefaultsAndReturnsDetails(t *testing.T) {
	registry := extension.NewRegistry()
	runner := &fakeRunner{result: Result{Output: "report", ExitCode: 0, Usage: Usage{Turns: 1, Input: 20, Output: 4, WallMS: 17, Model: "test"}}}
	if err := RegisterWithRunner(registry, runner); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	var updates []string
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"prompt":" inspect this "}`), func(s string) { updates = append(updates, s) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "report" || result.IsError || result.Details["exitCode"] != 0 {
		t.Fatalf("result = %#v", result)
	}
	if runner.input.Prompt != "inspect this" || runner.input.Tools != "find,grep,ls,read" || runner.input.Thinking != "low" || runner.input.TimeoutSeconds != 300 || runner.input.MaxOutputChars != 12000 {
		t.Fatalf("input = %#v", runner.input)
	}
	delegatedValue, ok := result.Details["delegated_usage"].(delegation.Usage)
	if !ok || delegatedValue != (delegation.Usage{Turns: 1, InputTokens: 20, OutputTokens: 4, WallMS: 17, Calls: 1}) {
		t.Fatalf("delegated usage = %#v", result.Details["delegated_usage"])
	}
	if usageLine, _ := result.Details["usageLine"].(string); !strings.Contains(usageLine, "17 ms") {
		t.Fatalf("usageLine = %#v", result.Details["usageLine"])
	}
	if !reflect.DeepEqual(updates, []string{"running"}) {
		t.Fatalf("updates = %#v", updates)
	}
}

func TestRunSubagentMarksFailedExitAsToolError(t *testing.T) {
	registry := extension.NewRegistry()
	runner := &fakeRunner{result: Result{Output: "failed", ExitCode: 7, TimedOut: true, Usage: Usage{WallMS: 9}}}
	if err := RegisterWithRunner(registry, runner); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"prompt":"work"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "exit 7") || result.Details["timedOut"] != true {
		t.Fatalf("result = %#v", result)
	}
	if delegatedValue, ok := result.Details["delegated_usage"].(delegation.Usage); !ok || delegatedValue.WallMS != 9 {
		t.Fatalf("delegated usage = %#v", result.Details["delegated_usage"])
	}
}

func TestRunSubagentToolSafetyAndValidation(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"prompt":" "}`, `{"prompt":"x","tools":"read,bash"}`,
		`{"prompt":"x","tools":"read,,grep"}`, `{"prompt":"x","timeoutSeconds":3601}`,
		`{"prompt":"x","maxOutputChars":999}`, `{"prompt":"x","thinking":"extreme"}`,
		`{"prompt":"x","unknown":true}`,
	} {
		if _, err := decodeInput(json.RawMessage(raw)); err == nil {
			t.Fatalf("decodeInput(%s) succeeded", raw)
		}
	}
	input, err := decodeInput(json.RawMessage(`{"prompt":"implement","tools":"write,bash","allowWriteTools":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if input.Tools != "bash,write" {
		t.Fatalf("tools = %q", input.Tools)
	}
}

func TestParseEventsUsesLastAssistantTurnAndTrimsOutput(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"turn_end","usage":{"input_tokens":10,"output_tokens":2,"cache_read_tokens":3,"cache_write_tokens":1,"reasoning_tokens":1,"cost_usd":0.002},"message":{"role":"assistant","content":[{"type":"text","text":"first"}]}}`,
		`not json`,
		`{"type":"turn_end","usage":{"input_tokens":20,"output_tokens":4,"cache_read_tokens":5,"cache_write_tokens":2,"reasoning_tokens":2,"cost_usd":0.003},"message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 1200) + `"}]}}`,
	}, "\n")
	parsed := parseEvents(strings.NewReader(stream), 1000)
	cost := parsed.costUSD()
	if parsed.turns != 2 || parsed.input != 30 || parsed.outputTokens != 6 || parsed.cacheRead != 8 || parsed.cacheWrite != 3 || parsed.reasoning != 3 || cost == nil || *cost != 0.005 || !strings.Contains(parsed.output, "output truncated") {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestModelArgsAndSystemPrompt(t *testing.T) {
	if got := modelArgs("anthropic/claude-test"); !reflect.DeepEqual(got, []string{"--provider", "anthropic", "--model", "claude-test"}) {
		t.Fatalf("model args = %#v", got)
	}
	if got := modelArgs("model-only"); !reflect.DeepEqual(got, []string{"--model", "model-only"}) {
		t.Fatalf("model args = %#v", got)
	}
	if prompt := baseSystemPrompt("Be exact."); !strings.Contains(prompt, "focused Notch subagent") || !strings.HasSuffix(prompt, "Be exact.") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestProcessRunnerMeasuresWallTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := t.TempDir() + "/fake-subagent.sh"
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '{\"type\":\"turn_end\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2},\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"done\"}]}}\\n'\nsleep 0.02\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir()}
	result, err := runner.Run(context.Background(), Input{Prompt: "go", Tools: "read,grep,find,ls", Thinking: DefaultThinking, TimeoutSeconds: 1, MaxOutputChars: 2000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.WallMS <= 0 {
		t.Fatalf("wall_ms = %d", result.Usage.WallMS)
	}
}

func TestProcessRunnerDurationIncludedWhenTimedOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := t.TempDir() + "/slow-subagent.sh"
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir()}
	result, err := runner.Run(context.Background(), Input{Prompt: "go", Tools: "read,grep,find,ls", Thinking: DefaultThinking, TimeoutSeconds: 1, MaxOutputChars: 2000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.Usage.WallMS <= 0 || result.ExitCode != 124 {
		t.Fatalf("result = %#v", result)
	}
}

func TestDelegatedUsageShape(t *testing.T) {
	usage := delegatedUsage(Usage{Turns: 2, Input: 3, Output: 4, WallMS: 5})
	if usage != (delegation.Usage{Turns: 2, InputTokens: 3, OutputTokens: 4, WallMS: 5, Calls: 1}) {
		t.Fatalf("usage = %#v", usage)
	}
}
