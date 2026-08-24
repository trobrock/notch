package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

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
	runner := &fakeRunner{result: Result{Output: "report", ExitCode: 0, Usage: Usage{Turns: 1, Input: 20, Output: 4, Model: "test"}}}
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
	if runner.input.Prompt != "inspect this" || runner.input.Tools != "find,grep,ls,read" || runner.input.Thinking != "minimal" || runner.input.TimeoutSeconds != 300 || runner.input.MaxOutputChars != 12000 {
		t.Fatalf("input = %#v", runner.input)
	}
	if !reflect.DeepEqual(updates, []string{"running"}) {
		t.Fatalf("updates = %#v", updates)
	}
}

func TestRunSubagentMarksFailedExitAsToolError(t *testing.T) {
	registry := extension.NewRegistry()
	runner := &fakeRunner{result: Result{Output: "failed", ExitCode: 7, TimedOut: true}}
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
		`{"type":"turn_end","usage":{"input_tokens":10,"output_tokens":2},"message":{"role":"assistant","content":[{"type":"text","text":"first"}]}}`,
		`not json`,
		`{"type":"turn_end","usage":{"input_tokens":20,"output_tokens":4},"message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 1200) + `"}]}}`,
	}, "\n")
	parsed := parseEvents(strings.NewReader(stream), 1000)
	if parsed.turns != 2 || parsed.input != 30 || parsed.outputTokens != 6 || !strings.Contains(parsed.output, "output truncated") {
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

func TestRunnerErrorPropagates(t *testing.T) {
	registry := extension.NewRegistry()
	want := errors.New("start failed")
	if err := RegisterWithRunner(registry, &fakeRunner{err: want}); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"prompt":"work"}`), nil); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}
