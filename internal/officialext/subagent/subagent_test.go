package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/config"
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
	for _, text := range []string{"current parent provider/model", "unqualified model stays on the current provider", "ask before using another provider"} {
		if !strings.Contains(tool.Definition.Description, text) {
			t.Fatalf("description missing %q: %q", text, tool.Definition.Description)
		}
	}
	var updates []string
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"prompt":" inspect this "}`), func(s string) { updates = append(updates, s) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "report" || result.IsError || result.Details["exitCode"] != 0 {
		t.Fatalf("result = %#v", result)
	}
	if runner.input.Prompt != "inspect this" || runner.input.Tools != "find,grep,ls,read" || runner.input.Thinking != "low" || runner.input.TimeoutSeconds != 0 || runner.input.MaxOutputChars != 12000 {
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
		`{"prompt":"x","tools":"read,,grep"}`, `{"prompt":"x","timeoutSeconds":86401}`,
		`{"prompt":"x","maxOutputChars":999}`, `{"prompt":"x","thinking":"extreme"}`,
		`{"prompt":"x","maxTurns":-1}`, `{"prompt":"x","maxCostUSD":-1}`,
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
	if got := modelArgs("anthropic/claude-test", "openai-codex", "parent"); !reflect.DeepEqual(got, []string{"--provider", "anthropic", "--model", "claude-test"}) {
		t.Fatalf("qualified model args = %#v", got)
	}
	if got := modelArgs("model-only", "openai-codex", "parent"); !reflect.DeepEqual(got, []string{"--provider", "openai-codex", "--model", "model-only"}) {
		t.Fatalf("same-provider model args = %#v", got)
	}
	if got := modelArgs("", "openrouter", "anthropic/claude-test"); !reflect.DeepEqual(got, []string{"--provider", "openrouter", "--model", "anthropic/claude-test"}) {
		t.Fatalf("inherited slash model args = %#v", got)
	}
	if got := modelArgs("", "openai-codex", "parent"); !reflect.DeepEqual(got, []string{"--provider", "openai-codex", "--model", "parent"}) {
		t.Fatalf("inherited model args = %#v", got)
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

func TestProcessRunnerCancellationReturnsParsedUsage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := t.TempDir() + "/cancel-subagent.sh"
	script := `#!/bin/sh
printf '{"type":"turn_end","usage":{"input_tokens":7,"output_tokens":3},"message":{"role":"assistant","content":[{"type":"text","text":"partial"}]}}\n'
sleep 10
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := runner.Run(ctx, Input{Prompt: "go", Tools: defaultTools, Thinking: DefaultThinking, TimeoutSeconds: 10, MaxOutputChars: 2000}, func(update string) {
		if strings.Contains(update, "1 turn") {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if result.Usage.Turns != 1 || result.Usage.Input != 7 || result.Usage.Output != 3 || result.Output != "partial" {
		t.Fatalf("result = %#v", result)
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

func TestProcessRunnerPassesBudgetsAndStripsHerdrEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "pane-1")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	path := t.TempDir() + "/budget-subagent.sh"
	script := `#!/bin/sh
[ -z "${HERDR_ENV:-}" ] || exit 8
[ -z "${HERDR_PANE_ID:-}" ] || exit 9
previous=""
turns=""
cost=""
provider=""
model=""
for arg in "$@"; do
  if [ "$previous" = "--max-turns" ]; then turns="$arg"; fi
  if [ "$previous" = "--max-cost-usd" ]; then cost="$arg"; fi
  if [ "$previous" = "--provider" ]; then provider="$arg"; fi
  if [ "$previous" = "--model" ]; then model="$arg"; fi
  previous="$arg"
done
[ "$turns" = "4" ] || exit 10
[ "$cost" = "0.25" ] || exit 11
[ "$provider" = "openai-codex" ] || exit 12
[ "$model" = "parent-model" ] || exit 13
printf '{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}\n'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir(), currentModel: func() (string, string) { return "openai-codex", "parent-model" }}
	result, err := runner.Run(context.Background(), Input{Prompt: "go", Tools: defaultTools, Thinking: DefaultThinking, TimeoutSeconds: 1, MaxOutputChars: 2000, MaxTurns: 4, MaxCostUSD: 0.25}, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

func TestChildEnvironmentRemovesOnlyHerdrValues(t *testing.T) {
	got := childEnvironment([]string{"PATH=/bin", "HERDR_ENV=1", "HERDR_PANE_ID=pane", "HOME=/tmp"})
	want := []string{"PATH=/bin", "HOME=/tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("childEnvironment() = %#v, want %#v", got, want)
	}
}

func TestReadOnlyShellRejectionGivesSafeRecovery(t *testing.T) {
	_, err := decodeInput(json.RawMessage(`{"prompt":"inspect","tools":"read,bash"}`))
	if err == nil {
		t.Fatal("bash accepted without authorization")
	}
	for _, text := range []string{"bash is write-capable", "read,grep,find,ls", "parent", "allowWriteTools=true"} {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("error lacks %q: %v", text, err)
		}
	}
	properties := schema()["properties"].(map[string]any)
	for _, name := range []string{"tools", "allowWriteTools"} {
		description := properties[name].(map[string]any)["description"].(string)
		if !strings.Contains(description, "bash") {
			t.Errorf("%s schema must warn about bash: %s", name, description)
		}
	}
}

func TestProcessRunnerEnforcesProfileBeforeChildDispatch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	path := filepath.Join(t.TempDir(), "child.sh")
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir(), defaultProvider: "anthropic", defaultModel: "safe", allowedProviders: map[string]bool{"anthropic": true}}
	_, err := runner.Run(context.Background(), Input{Prompt: "go", Model: "openai/forbidden", Tools: defaultTools, Thinking: DefaultThinking, TimeoutSeconds: 1, MaxOutputChars: 2000}, nil)
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("child was dispatched: %v", statErr)
	}
}

func TestProcessRunnerPassesImmutableRoutingSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := filepath.Join(t.TempDir(), "routing-child.sh")
	script := `#!/bin/sh
previous=""
snapshot=""
for arg in "$@"; do
  if [ "$previous" = "--routing-snapshot" ]; then snapshot="$arg"; fi
  previous="$arg"
done
[ "$snapshot" = "pinned-snapshot" ] || exit 14
printf '{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}\n'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir(), defaultProvider: "anthropic", defaultModel: "safe", routingSnapshot: "pinned-snapshot", allowedProviders: map[string]bool{"anthropic": true}}
	result, err := runner.Run(context.Background(), Input{Prompt: "go", Tools: defaultTools, Thinking: DefaultThinking, TimeoutSeconds: 1, MaxOutputChars: 2000}, nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

type roleTestHost struct {
	models  []extension.ModelInfo
	session string
	entries []string
}

func (*roleTestHost) CWD() string { return "." }
func (*roleTestHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, nil
}
func (*roleTestHost) Input(context.Context, string, string) (string, error)    { return "", nil }
func (*roleTestHost) Select(context.Context, string, []string) (string, error) { return "", nil }
func (*roleTestHost) Notify(string, string)                                    {}
func (*roleTestHost) FollowUp(string) error                                    { return nil }
func (*roleTestHost) Handoff(string, bool) error                               { return nil }
func (*roleTestHost) SetActiveTools([]string) error                            { return nil }
func (*roleTestHost) SwitchModel(context.Context, string, string) (string, int, error) {
	return "", 0, nil
}
func (h *roleTestHost) ListModels(context.Context, string, bool) ([]extension.ModelInfo, error) {
	return h.models, nil
}
func (h *roleTestHost) SessionID() string { return h.session }
func (h *roleTestHost) AppendSessionEntry(_ string, kind string, _ any) error {
	h.entries = append(h.entries, kind)
	return nil
}
func (*roleTestHost) SessionEntries(string) ([]json.RawMessage, error) { return nil, nil }
func (*roleTestHost) EditorText(context.Context) (string, error)       { return "", nil }
func (*roleTestHost) SetEditorText(context.Context, string) error      { return nil }
func (*roleTestHost) SetStatus(string, string)                         {}
func (*roleTestHost) SetPanel(string, string, []string)                {}

func TestRoleRunnerPrecedenceProvenanceAndAdditivePrompt(t *testing.T) {
	base := &fakeRunner{result: Result{Output: "ok"}}
	host := &roleTestHost{models: []extension.ModelInfo{{ID: "profile-review"}, {ID: "explicit-review"}}, session: "saved"}
	cfg := config.Config{Roles: map[string]config.RoleConfig{"reviewer": {
		Instructions: "ROLE CONTRACT", Model: "role-review", Thinking: "high", TimeoutSeconds: 40, MaxOutputChars: 4000, MaxTurns: 4, MaxCostUSD: .2,
	}}}
	routing := &config.RoutingSnapshot{Profile: "work", Provider: "openai", AllowedProviders: []string{"openai"}, RoleModels: map[string]string{"reviewer": "profile-review"}}
	runner := NewRoleRunner(base, host, cfg, routing, "openai")
	result, err := runner.Run(context.Background(), Input{Prompt: "task", Role: "reviewer", Tools: "read", Model: "explicit-review", ModelExplicit: true, SystemPrompt: "CALLER ADDITION", TimeoutSeconds: 99, TimeoutExplicit: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.input.Tools != "read" {
		t.Fatalf("role changed tools: %#v", base.input)
	}
	if base.input.Model != "explicit-review" || base.input.Thinking != "high" || base.input.TimeoutSeconds != 99 || base.input.MaxTurns != 4 {
		t.Fatalf("input=%#v", base.input)
	}
	if !strings.Contains(base.input.SystemPrompt, "ROLE CONTRACT\nCALLER ADDITION") {
		t.Fatalf("system prompt=%q", base.input.SystemPrompt)
	}
	if result.Role == nil || result.Role.Name != "reviewer" || result.Role.ModelProvenance != "explicit task override" || result.Role.DefinitionHash == "" {
		t.Fatalf("role=%#v", result.Role)
	}
	if !reflect.DeepEqual(host.entries, []string{"delegation_role", "delegation_role_result"}) {
		t.Fatalf("session entries=%v", host.entries)
	}
	prompt := baseSystemPrompt(base.input.SystemPrompt)
	if strings.Index(prompt, "focused Notch subagent") > strings.Index(prompt, "ROLE CONTRACT") {
		t.Fatalf("invariant replaced: %q", prompt)
	}
}

func TestRoleRunnerRejectsUnavailableModelBeforeDispatch(t *testing.T) {
	base := &fakeRunner{}
	runner := NewRoleRunner(base, &roleTestHost{}, config.Config{}, &config.RoutingSnapshot{Profile: "work", Provider: "openai", AllowedProviders: []string{"openai"}, RoleModels: map[string]string{"researcher": "missing"}}, "openai")
	_, err := runner.Run(context.Background(), Input{Prompt: "task", Role: "researcher"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err=%v", err)
	}
	if base.input.Prompt != "" {
		t.Fatalf("runner dispatched: %#v", base.input)
	}
}

func TestRunSubagentAcceptsGlobalCustomRole(t *testing.T) {
	base := &fakeRunner{result: Result{Output: "audited", Usage: Usage{Model: "audit-model"}}}
	host := &roleTestHost{models: []extension.ModelInfo{{ID: "audit-model"}}}
	cfg := config.Config{Roles: map[string]config.RoleConfig{"auditor": {Instructions: "Audit carefully.", Model: "audit-model", Thinking: "high"}}}
	registry := extension.NewRegistry()
	if err := RegisterWithRunner(registry, NewRoleRunner(base, host, cfg, nil, "openai")); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"prompt":"check","role":"auditor"}`), nil)
	if err != nil || result.Content != "audited" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	role, ok := result.Details["role"].(*config.RoleResolution)
	if !ok || role.Name != "auditor" || base.input.Thinking != "high" {
		t.Fatalf("role=%#v input=%#v", result.Details["role"], base.input)
	}
}

func TestProcessRunnerDefaultAllowsActiveWorkPastIdleWindow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := filepath.Join(t.TempDir(), "active.sh")
	script := `#!/bin/sh
printf '{"type":"turn_start"}\n'
i=0
while [ "$i" -lt 12 ]; do
  printf '{"type":"thinking_delta","text":"working"}\n'
  sleep 0.1
  i=$((i+1))
done
printf '{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}\n'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir(), idleTimeout: time.Second}
	result, err := runner.Run(context.Background(), Input{Prompt: "work", MaxOutputChars: 2000}, nil)
	if err != nil || result.ExitCode != 0 || result.TimedOut || result.Output != "done" {
		t.Fatalf("active child = %#v, %v", result, err)
	}
}

func TestProcessRunnerStopsSilentChildAndPreservesPartialOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := filepath.Join(t.TempDir(), "silent.sh")
	script := "#!/bin/sh\nprintf '{\"type\":\"text_delta\",\"text\":\"partial findings\"}\\n'\nsleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{executable: path, defaultCWD: t.TempDir(), idleTimeout: 500 * time.Millisecond}
	result, err := runner.Run(context.Background(), Input{Prompt: "work", MaxOutputChars: 2000}, nil)
	if err != nil || !result.TimedOut || result.ExitCode != 124 || !strings.Contains(result.Output, "without provider or tool events") || !strings.Contains(result.Output, "partial findings") {
		t.Fatalf("silent child = %#v, %v", result, err)
	}
}

func TestProcessRunnerDefaultStillHonorsCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	path := filepath.Join(t.TempDir(), "cancel.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	runner := &processRunner{executable: path, defaultCWD: t.TempDir()}
	result, err := runner.Run(ctx, Input{Prompt: "work", MaxOutputChars: 2000}, nil)
	if !errors.Is(err, context.Canceled) || result.ExitCode == 0 || result.TimedOut {
		t.Fatalf("cancelled child = %#v, %v", result, err)
	}
}

func TestPartialStreamPreservesWhitespaceAndUnicode(t *testing.T) {
	var stream strings.Builder
	for _, text := range []string{"hello ", "世界", "\n", "  indented", "\n"} {
		line, _ := json.Marshal(map[string]string{"type": "text_delta", "text": text})
		stream.Write(line)
		stream.WriteByte('\n')
	}
	parsed := parseEvents(strings.NewReader(stream.String()), 1000, nil)
	if parsed.output != "hello 世界\n  indented" {
		t.Fatalf("partial text = %q", parsed.output)
	}
}

func TestPartialStreamTruncatesOnceWithAccurateRuneCount(t *testing.T) {
	var stream strings.Builder
	for i := 0; i < 100; i++ {
		line, _ := json.Marshal(map[string]string{"type": "text_delta", "text": strings.Repeat("界", 30)})
		stream.Write(line)
		stream.WriteByte('\n')
	}
	parsed := parseEvents(strings.NewReader(stream.String()), 1000, nil)
	want := strings.Repeat("界", 650) + "\n\n[run_subagent output truncated: 3000 chars total]\n\n" + strings.Repeat("界", 350)
	if parsed.output != want {
		t.Fatalf("unexpected partial truncation: %q", parsed.output)
	}
}

func TestRunSubagentCancellationPreservesPublicResult(t *testing.T) {
	registry := extension.NewRegistry()
	runner := &fakeRunner{result: Result{Output: "partial findings", ExitCode: -1, Usage: Usage{Turns: 1, Input: 7, Output: 3, WallMS: 1}}, err: context.Canceled}
	if err := RegisterWithRunner(registry, runner); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := tool.Execute(ctx, json.RawMessage(`{"prompt":"work"}`), nil)
	if err != nil || !result.IsError || !strings.Contains(result.Content, "partial findings") || !strings.Contains(result.Content, "context canceled") {
		t.Fatalf("cancel result = %#v, %v", result, err)
	}
	usage, ok := result.Details["delegated_usage"].(delegation.Usage)
	if !ok || usage.Turns != 1 || usage.InputTokens != 7 || usage.Calls != 1 {
		t.Fatalf("usage lost: %#v", result.Details)
	}
}

func TestProcessRunnerStopsTermResistantChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process-group test")
	}
	for _, closeStdout := range []bool{false, true} {
		t.Run(fmt.Sprint(closeStdout), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resistant.sh")
			script := "#!/bin/sh\ntrap '' TERM\nprintf '{\"type\":\"text_delta\",\"text\":\"partial findings\"}\\n'\n"
			if closeStdout {
				script += "exec 1>&-\n"
			}
			script += "while :; do sleep 30; done\n"
			if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			runner := &processRunner{executable: path, defaultCWD: t.TempDir(), idleTimeout: 100 * time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			start := time.Now()
			result, err := runner.Run(ctx, Input{Prompt: "work", MaxOutputChars: 2000}, nil)
			if err != nil || !result.TimedOut || result.ExitCode != 124 || time.Since(start) >= 5*time.Second || !strings.Contains(result.Output, "partial findings") {
				t.Fatalf("resistant child = %#v, %v after %s", result, err, time.Since(start))
			}
		})
	}
}
