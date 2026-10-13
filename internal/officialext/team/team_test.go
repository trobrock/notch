package team

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/delegation"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/officialext/subagent"
)

type fakeHost struct {
	cwd      string
	answer   string
	inputErr error
	asked    chan string
	followup string
}

func (h *fakeHost) CWD() string { return h.cwd }
func (*fakeHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, nil
}
func (h *fakeHost) Input(ctx context.Context, prompt, _ string) (string, error) {
	if h.asked != nil {
		h.asked <- prompt
	}
	if h.inputErr != nil {
		return "", h.inputErr
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
		return h.answer, nil
	}
}
func (*fakeHost) Select(context.Context, string, []string) (string, error)         { return "", nil }
func (*fakeHost) Notify(string, string)                                            {}
func (h *fakeHost) FollowUp(value string) error                                    { h.followup = value; return nil }
func (*fakeHost) Handoff(string, bool) error                                       { return nil }
func (*fakeHost) SetActiveTools([]string) error                                    { return nil }
func (*fakeHost) SwitchModel(context.Context, string, string) (string, int, error) { return "", 0, nil }
func (*fakeHost) ListModels(context.Context, string, bool) ([]extension.ModelInfo, error) {
	return nil, nil
}
func (*fakeHost) SessionID() string                                { return "session" }
func (*fakeHost) AppendSessionEntry(string, string, any) error     { return nil }
func (*fakeHost) SessionEntries(string) ([]json.RawMessage, error) { return nil, nil }
func (*fakeHost) EditorText(context.Context) (string, error)       { return "", nil }
func (*fakeHost) SetEditorText(context.Context, string) error      { return nil }
func (*fakeHost) SetStatus(string, string)                         {}
func (*fakeHost) SetPanel(string, string, []string)                {}

type scriptedRunner struct {
	mu      sync.Mutex
	outputs map[string][]subagent.Result
	inputs  []subagent.Input
}

func (r *scriptedRunner) Run(_ context.Context, in subagent.Input, update func(string)) (subagent.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs = append(r.inputs, in)
	if update != nil {
		update("done")
	}
	for id, values := range r.outputs {
		if strings.Contains(in.Prompt, "Task: "+id) && len(values) > 0 {
			out := values[0]
			r.outputs[id] = values[1:]
			return out, nil
		}
	}
	return subagent.Result{}, errors.New("no scripted result")
}

type gitSnapshot struct {
	root, common, branch, head string
	dirty                      bool
}
type fakeGit struct {
	mu             sync.Mutex
	states         map[string][]gitSnapshot
	ancestor       bool
	conflict       bool
	conflictPairs  map[string]bool
	conflictDetail string
	inspectCalls   map[string]int
}

func (g *fakeGit) Inspect(_ context.Context, cwd string) (string, string, string, string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inspectCalls == nil {
		g.inspectCalls = map[string]int{}
	}
	i := g.inspectCalls[cwd]
	g.inspectCalls[cwd] = i + 1
	values := g.states[cwd]
	if len(values) == 0 {
		return "", "", "", "", false, errors.New("missing git state")
	}
	if i >= len(values) {
		i = len(values) - 1
	}
	v := values[i]
	common := v.common
	if common == "" {
		common = "/repo/.git"
	}
	return v.root, common, v.branch, v.head, v.dirty, nil
}
func (g *fakeGit) IsAncestor(context.Context, string, string, string) (bool, error) {
	return g.ancestor, nil
}
func (g *fakeGit) Conflicts(_ context.Context, _, left, right string) (bool, string, error) {
	conflict := g.conflict
	if g.conflictPairs != nil {
		conflict = g.conflictPairs[left+"|"+right]
	}
	return conflict, g.conflictDetail, nil
}

func complete(summary string) string {
	return `{"status":"complete","summary":"` + summary + `","artifacts":["file.go:12"],"checks":["focused test: pass"]}`
}

func TestIndependentHelpersAggregateUsageOnce(t *testing.T) {
	costA, costB := 0.2, 0.3
	runner := &scriptedRunner{outputs: map[string][]subagent.Result{
		"research one": {{Output: complete("one"), Usage: subagent.Usage{Turns: 1, Input: 10, Output: 2, CostUSD: &costA, WallMS: 20}}},
		"review two":   {{Output: complete("two"), Usage: subagent.Usage{Turns: 2, Input: 20, Output: 3, CostUSD: &costB, WallMS: 30}}},
	}}
	host := &fakeHost{cwd: "/lead"}
	git := &fakeGit{states: map[string][]gitSnapshot{"/lead": {{root: "/lead", branch: "main", head: "base"}}}}
	registry := extension.NewRegistry()
	if err := register(registry, host, runner, git); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Tool(ToolName)
	raw := json.RawMessage(`{"tasks":[{"id":"a","kind":"research","prompt":"research one","acceptance_checks":["cite code"]},{"id":"b","kind":"review","prompt":"review two","acceptance_checks":["name risk"]}]}`)
	result, err := tool.Execute(context.Background(), raw, nil)
	if err != nil || result.IsError {
		t.Fatalf("execute = %#v, %v", result, err)
	}
	usage, ok := result.Details["delegated_usage"].(delegation.Usage)
	if !ok || usage.Turns != 3 || usage.InputTokens != 30 || usage.OutputTokens != 5 || usage.Calls != 2 || usage.CostUSD == nil || *usage.CostUSD != 0.5 {
		t.Fatalf("usage = %#v", result.Details["delegated_usage"])
	}
	if len(runner.inputs) != 2 {
		t.Fatalf("runner calls = %d", len(runner.inputs))
	}
	for _, input := range runner.inputs {
		if input.AllowWriteTools || input.Tools != "read,grep,find,ls" {
			t.Fatalf("unsafe helper input = %#v", input)
		}
		if strings.Contains(input.Prompt, "summary\":\"one") || strings.Contains(input.Prompt, "summary\":\"two") {
			t.Fatal("helper result leaked into independent prompt")
		}
	}
}

func TestClarificationIsCorrelatedBoundedAndUsageIncluded(t *testing.T) {
	cost := 0.1
	runner := &scriptedRunner{outputs: map[string][]subagent.Result{"blocked task": {
		{Output: `{"status":"clarification","question":"Which format?"}`, Usage: subagent.Usage{Turns: 1, Input: 4, CostUSD: &cost}},
		{Output: complete("used answer"), Usage: subagent.Usage{Turns: 1, Input: 6, Output: 2, CostUSD: &cost}},
	}}}
	host := &fakeHost{cwd: "/lead", answer: "JSON", asked: make(chan string, 1)}
	git := &fakeGit{states: map[string][]gitSnapshot{"/lead": {{root: "/lead", branch: "main", head: "base"}}}}
	registry := extension.NewRegistry()
	_ = register(registry, host, runner, git)
	tool, _ := registry.Tool(ToolName)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"clarify_wait_seconds":1,"tasks":[{"id":"ask","kind":"research","prompt":"blocked task","acceptance_checks":["format chosen"]}]}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("execute = %#v, %v", result, err)
	}
	prompt := <-host.asked
	if !strings.Contains(prompt, "help-") || !strings.Contains(prompt, "Which format?") {
		t.Fatalf("prompt = %q", prompt)
	}
	if len(runner.inputs) != 2 || !strings.Contains(runner.inputs[1].Prompt, "answer: JSON") {
		t.Fatalf("inputs = %#v", runner.inputs)
	}
	usage := result.Details["delegated_usage"].(delegation.Usage)
	if usage.Turns != 2 || usage.InputTokens != 10 || usage.Calls != 2 || usage.CostUSD == nil || *usage.CostUSD != 0.2 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestClarificationCancellationStopsWithoutDeadlock(t *testing.T) {
	runner := &scriptedRunner{outputs: map[string][]subagent.Result{"blocked": {{Output: `{"status":"clarification","question":"Need choice"}`}}}}
	host := &fakeHost{cwd: "/lead", inputErr: context.Canceled}
	git := &fakeGit{states: map[string][]gitSnapshot{"/lead": {{root: "/lead", branch: "main", head: "base"}}}}
	registry := extension.NewRegistry()
	_ = register(registry, host, runner, git)
	tool, _ := registry.Tool(ToolName)
	done := make(chan extension.ToolResult, 1)
	go func() {
		result, _ := tool.Execute(context.Background(), json.RawMessage(`{"tasks":[{"id":"x","kind":"research","prompt":"blocked","acceptance_checks":["answer"]}]}`), nil)
		done <- result
	}()
	select {
	case result := <-done:
		if !result.IsError || !strings.Contains(result.Content, "cancelled") {
			t.Fatalf("result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("clarification cancellation deadlocked")
	}
}

func TestWriterIsolationStaleBaseConflictAndNoCleanup(t *testing.T) {
	base := "base"
	validInput := func(expected string) json.RawMessage {
		return json.RawMessage(`{"allow_write_tools":true,"integration_owner":"lead","tasks":[{"id":"change","kind":"change","prompt":"small change","cwd":"/writer","tools":"edit,read","expected_base":"` + expected + `","acceptance_checks":["test passes"]}]}`)
	}
	for _, tc := range []struct {
		name, expected                                                  string
		sameRoot, sameBranch, dirty, finalDirty, leadAdvanced, conflict bool
		wantError                                                       bool
		wantResult                                                      string
	}{
		{"stale base", "old", false, false, false, false, false, false, true, ""},
		{"same worktree", base, true, false, false, false, false, false, true, ""},
		{"same branch", base, false, true, false, false, false, false, true, ""},
		{"dirty worktree", base, false, false, true, false, false, false, true, ""},
		{"uncommitted result", base, false, false, false, true, false, false, false, "uncommitted"},
		{"lead advanced", base, false, false, false, false, true, false, false, "lead HEAD changed"},
		{"conflict", base, false, false, false, false, false, true, false, "conflict"},
		{"clean writer", base, false, false, false, false, false, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writerRoot := "/writer"
			if tc.sameRoot {
				writerRoot = "/lead"
			}
			writerBranch := "helper"
			if tc.sameBranch {
				writerBranch = "main"
			}
			leadEnd := base
			if tc.leadAdvanced {
				leadEnd = "new-lead"
			}
			git := &fakeGit{ancestor: true, conflict: tc.conflict, conflictDetail: "conflict detail", states: map[string][]gitSnapshot{
				"/lead": {{root: "/lead", branch: "main", head: base}, {root: "/lead", branch: "main", head: leadEnd}}, "/writer": {{root: writerRoot, branch: writerBranch, head: base, dirty: tc.dirty}, {root: writerRoot, branch: writerBranch, head: "commit", dirty: tc.finalDirty}},
			}}
			runner := &scriptedRunner{outputs: map[string][]subagent.Result{"small change": {{Output: complete("changed")}}}}
			registry := extension.NewRegistry()
			_ = register(registry, &fakeHost{cwd: "/lead"}, runner, git)
			tool, _ := registry.Tool(ToolName)
			result, err := tool.Execute(context.Background(), validInput(tc.expected), nil)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected validation error, result=%#v", result)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantResult == "" {
				if result.IsError {
					t.Fatalf("clean writer result = %#v", result)
				}
			} else if !result.IsError || !strings.Contains(result.Content, tc.wantResult) {
				t.Fatalf("result = %#v", result)
			}
			if len(git.states["/writer"]) != 2 {
				t.Fatal("coordinator mutated or cleaned writer state")
			}
		})
	}
}

func TestParallelWriterConflictIsReportedBeforeIntegration(t *testing.T) {
	base := "base"
	git := &fakeGit{ancestor: true, conflictPairs: map[string]bool{"commit-a|commit-b": true}, conflictDetail: "same lines", states: map[string][]gitSnapshot{
		"/lead": {{root: "/lead", branch: "main", head: base}},
		"/a":    {{root: "/a", branch: "writer-a", head: base}, {root: "/a", branch: "writer-a", head: "commit-a"}},
		"/b":    {{root: "/b", branch: "writer-b", head: base}, {root: "/b", branch: "writer-b", head: "commit-b"}},
	}}
	runner := &scriptedRunner{outputs: map[string][]subagent.Result{
		"change a": {{Output: complete("a")}},
		"change b": {{Output: complete("b")}},
	}}
	registry := extension.NewRegistry()
	_ = register(registry, &fakeHost{cwd: "/lead"}, runner, git)
	tool, _ := registry.Tool(ToolName)
	raw := json.RawMessage(`{"allow_write_tools":true,"integration_owner":"lead","tasks":[{"id":"a","kind":"change","prompt":"change a","cwd":"/a","tools":"edit,read","expected_base":"base","acceptance_checks":["test a"]},{"id":"b","kind":"change","prompt":"change b","cwd":"/b","tools":"edit,read","expected_base":"base","acceptance_checks":["test b"]}]}`)
	result, err := tool.Execute(context.Background(), raw, nil)
	if err != nil || !result.IsError || strings.Count(result.Content, `"status": "conflict"`) != 2 || !strings.Contains(result.Content, "same lines") {
		t.Fatalf("result = %#v, err=%v", result, err)
	}
}

func TestFailedWriterStillRunsPostRunAudit(t *testing.T) {
	base := "base"
	git := &fakeGit{ancestor: true, states: map[string][]gitSnapshot{
		"/lead":   {{root: "/lead", branch: "main", head: base}},
		"/writer": {{root: "/writer", branch: "helper", head: base}, {root: "/writer", branch: "helper", head: base, dirty: true}},
	}}
	runner := &scriptedRunner{outputs: map[string][]subagent.Result{"bad change": {{Output: "not json"}}}}
	registry := extension.NewRegistry()
	_ = register(registry, &fakeHost{cwd: "/lead"}, runner, git)
	tool, _ := registry.Tool(ToolName)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"allow_write_tools":true,"integration_owner":"lead","tasks":[{"id":"change","kind":"change","prompt":"bad change","cwd":"/writer","tools":"edit,read","expected_base":"base","acceptance_checks":["test"]}]}`), nil)
	if err != nil || !result.IsError || !strings.Contains(result.Content, "post-run audit") || !strings.Contains(result.Content, "uncommitted") {
		t.Fatalf("result = %#v, err=%v", result, err)
	}
}

func TestWriteAuthorizationAndTeamCommand(t *testing.T) {
	runner := &scriptedRunner{outputs: map[string][]subagent.Result{}}
	host := &fakeHost{cwd: "/lead"}
	git := &fakeGit{states: map[string][]gitSnapshot{"/lead": {{root: "/lead", branch: "main", head: "base"}}}}
	registry := extension.NewRegistry()
	_ = register(registry, host, runner, git)
	tool, _ := registry.Tool(ToolName)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"tasks":[{"id":"w","kind":"change","prompt":"write","cwd":"/writer","tools":"edit","expected_base":"base","acceptance_checks":["test"]}]}`), nil)
	if err == nil || !strings.Contains(err.Error(), "explicit authorization") {
		t.Fatalf("error = %v", err)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"max_output_chars":-1,"tasks":[{"id":"r","kind":"research","prompt":"read","acceptance_checks":["cite"]}]}`), nil)
	if err == nil || !strings.Contains(err.Error(), "max_output_chars") {
		t.Fatalf("max output error = %v", err)
	}
	command, ok := registry.Command("team")
	if !ok {
		t.Fatal("team command missing")
	}
	text, err := command.Execute(context.Background(), "ship a parser fix")
	if err != nil || !strings.Contains(text, "queued") || !strings.Contains(host.followup, "ship a parser fix") {
		t.Fatalf("command=%q followup=%q err=%v", text, host.followup, err)
	}
}

func TestUnknownCostRemainsUnknownAndEnvelopeValidation(t *testing.T) {
	known := 0.2
	a := addSubagentUsage(subagent.Usage{CostUSD: &known}, subagent.Usage{CostUSD: nil})
	if a.CostUSD != nil {
		t.Fatalf("unknown cost hidden as %#v", a.CostUSD)
	}
	if _, err := parseEnvelope(`{"status":"complete","summary":"x"}`); err == nil {
		t.Fatal("accepted result without check evidence")
	}
	if got := unsafeTools("read, bash, edit"); !reflect.DeepEqual(got, []string{"bash", "edit"}) {
		t.Fatalf("unsafe = %#v", got)
	}
}
