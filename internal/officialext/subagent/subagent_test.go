package subagent

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

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
	if tool.UpdateMode != "replace" {
		t.Fatalf("update mode = %q", tool.UpdateMode)
	}
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
		`{"type":"tool_start","tool_name":"read","tool_call_id":"1"}`,
		`not json`,
		`{"type":"turn_end","usage":{"input_tokens":20,"output_tokens":4,"cache_read_tokens":5,"cache_write_tokens":2,"reasoning_tokens":2,"cost_usd":0.003},"message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 1200) + `"}]}}`,
	}, "\n")
	var progress []childProgress
	parsed := parseEvents(strings.NewReader(stream), 1000, func(update childProgress) { progress = append(progress, update) })
	cost := parsed.costUSD()
	if parsed.turns != 2 || parsed.toolCalls != 1 || parsed.lastTool != "read" || parsed.input != 30 || parsed.outputTokens != 6 || parsed.cacheRead != 8 || parsed.cacheWrite != 3 || parsed.reasoning != 3 || cost == nil || *cost != 0.005 || !strings.Contains(parsed.output, "output truncated") {
		t.Fatalf("parsed = %#v", parsed)
	}
	if len(progress) != 3 || progress[len(progress)-1] != (childProgress{Turns: 2, ToolCalls: 1, LastTool: "read"}) {
		t.Fatalf("progress = %#v", progress)
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
	script := "#!/bin/sh\nprintf '{\"type\":\"turn_end\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n'\nprintf '{\"type\":\"tool_start\",\"tool_name\":\"grep\",\"tool_call_id\":\"1\"}\n'\nsleep 5\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir()}
	result, err := runner.Run(context.Background(), Input{Prompt: "go", Tools: "read,grep,find,ls", Thinking: DefaultThinking, TimeoutSeconds: 1, MaxOutputChars: 2000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.Usage.WallMS <= 0 || result.ExitCode != 124 || result.Usage.Turns != 1 || result.Usage.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
	for _, text := range []string{"1 completed turn", "1 tool call", "last tool: grep"} {
		if !strings.Contains(result.Output, text) {
			t.Fatalf("timeout output missing %q: %q", text, result.Output)
		}
	}
}

func TestDelegatedUsageShape(t *testing.T) {
	usage := delegatedUsage(Usage{Turns: 2, Input: 3, Output: 4, WallMS: 5})
	if usage != (delegation.Usage{Turns: 2, InputTokens: 3, OutputTokens: 4, WallMS: 5, Calls: 1}) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestProcessRunnerSendsPromptOnStdinAndReportsHeartbeat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := t.TempDir() + "/stdin-subagent.sh"
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "prompt from stdin" ]; then
    echo "prompt leaked into argv" >&2
    exit 9
  fi
done
prompt=$(cat)
if [ "$prompt" != "prompt from stdin" ]; then
  echo "bad stdin: $prompt" >&2
  exit 8
fi
sleep 0.03
printf '{"type":"turn_end","usage":{"input_tokens":1,"output_tokens":2},"message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}\n'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir(), heartbeatPeriod: 5 * time.Millisecond}
	var updates []string
	result, err := runner.Run(context.Background(), Input{Prompt: "prompt from stdin", Tools: defaultTools, Thinking: DefaultThinking, TimeoutSeconds: 1, MaxOutputChars: 2000}, func(update string) {
		updates = append(updates, update)
	})
	if err != nil || result.ExitCode != 0 || result.Output != "done" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if len(updates) == 0 || !strings.Contains(updates[len(updates)-1], "elapsed") {
		t.Fatalf("heartbeat updates = %#v", updates)
	}
}

func TestProcessRunnerRejectsCleanExitWithoutTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := t.TempDir() + "/empty-subagent.sh"
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'credential setup failed' >&2\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir()}
	result, err := runner.Run(context.Background(), Input{Prompt: "go", Tools: defaultTools, Thinking: DefaultThinking, TimeoutSeconds: 1, MaxOutputChars: 2000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 || !strings.Contains(result.Output, "without a completed model turn") || !strings.Contains(result.Output, "credential setup failed") {
		t.Fatalf("result = %#v", result)
	}
}

func TestProcessRunnerPropagatesSettingSources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := t.TempDir() + "/args-subagent.sh"
	script := `#!/bin/sh
previous=""
for arg in "$@"; do
  if [ "$previous" = "--setting-sources" ] && [ "$arg" != "project" ]; then exit 7; fi
  previous="$arg"
done
printf '{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}\n'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir(), settingSources: "project"}
	result, err := runner.Run(context.Background(), Input{Prompt: "go", Tools: defaultTools, Thinking: DefaultThinking, TimeoutSeconds: 1, MaxOutputChars: 2000}, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}
