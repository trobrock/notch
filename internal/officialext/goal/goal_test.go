package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/trobrock/notch/internal/extension"
)

type testHost struct {
	mu        sync.Mutex
	sessionID string
	entries   map[string]map[string][]json.RawMessage
	followUps []string
	statuses  []string
	followErr error
	appendErr error
}

func newTestHost(id string) *testHost {
	return &testHost{sessionID: id, entries: map[string]map[string][]json.RawMessage{id: {}}}
}
func (h *testHost) CWD() string { return "/work" }
func (h *testHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, errors.New("unused")
}
func (h *testHost) Input(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}
func (h *testHost) Select(context.Context, string, []string) (string, error) {
	return "", errors.New("unused")
}
func (h *testHost) Notify(string, string) {}
func (h *testHost) FollowUp(message string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.followErr != nil {
		return h.followErr
	}
	h.followUps = append(h.followUps, message)
	return nil
}
func (h *testHost) Handoff(string, bool) error    { return errors.New("unused") }
func (h *testHost) SetActiveTools([]string) error { return errors.New("unused") }
func (h *testHost) SwitchModel(context.Context, string, string) (string, int, error) {
	return "", 0, errors.New("unused")
}
func (h *testHost) ListModels(context.Context, string, bool) ([]extension.ModelInfo, error) {
	return nil, errors.New("unused")
}
func (h *testHost) SessionID() string { h.mu.Lock(); defer h.mu.Unlock(); return h.sessionID }
func (h *testHost) AppendSessionEntry(expected, kind string, data any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if expected != "" && expected != h.sessionID {
		return errors.New("stale session")
	}
	if h.appendErr != nil {
		return h.appendErr
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if h.entries[h.sessionID] == nil {
		h.entries[h.sessionID] = map[string][]json.RawMessage{}
	}
	h.entries[h.sessionID][kind] = append(h.entries[h.sessionID][kind], raw)
	return nil
}
func (h *testHost) SessionEntries(kind string) ([]json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]json.RawMessage(nil), h.entries[h.sessionID][kind]...), nil
}
func (h *testHost) EditorText(context.Context) (string, error)  { return "", errors.New("unused") }
func (h *testHost) SetEditorText(context.Context, string) error { return errors.New("unused") }
func (h *testHost) SetStatus(_ string, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statuses = append(h.statuses, value)
}
func (h *testHost) SetPanel(string, string, []string) {}

type noSessionHost struct{ *testHost }

func (h *noSessionHost) AppendSessionEntry(string, string, any) error {
	return errors.New("session persistence is unavailable")
}
func (h *noSessionHost) SessionEntries(string) ([]json.RawMessage, error) {
	return nil, errors.New("session persistence is unavailable")
}

func TestGoalDoesNotBreakNoSessionStartup(t *testing.T) {
	host := &noSessionHost{newTestHost("")}
	r := extension.NewRegistry()
	if err := Register(r, host); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunHooks(context.Background(), "session_start", map[string]any{}); err != nil {
		t.Fatalf("session_start = %v", err)
	}
	command, _ := r.Command("goal")
	if _, err := command.Execute(context.Background(), "start work --check done"); err == nil || !strings.Contains(err.Error(), "persistence") {
		t.Fatalf("goal start error = %v", err)
	}
}

func TestParseStartFlagsInAnyOrder(t *testing.T) {
	objective, checks, steps, err := parseStart("fix parser --max-steps 4 --check first check --check second check")
	if err != nil || objective != "fix parser" || steps != 4 || strings.Join(checks, "|") != "first check|second check" {
		t.Fatalf("parse = %q %#v %d, %v", objective, checks, steps, err)
	}
	objective, checks, _, err = parseStart("--check done")
	if err != nil || objective != "" || len(checks) != 1 {
		t.Fatalf("promptable parse = %q %#v, %v", objective, checks, err)
	}
}

func setup(t *testing.T, host *testHost) *extension.Registry {
	t.Helper()
	r := extension.NewRegistry()
	if err := Register(r, host); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunHooks(context.Background(), "session_start", map[string]any{"session_id": host.sessionID}); err != nil {
		t.Fatal(err)
	}
	return r
}

func runCommand(t *testing.T, r *extension.Registry, args string) string {
	t.Helper()
	command, ok := r.Command("goal")
	if !ok {
		t.Fatal("goal command not registered")
	}
	result, err := command.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func latest(t *testing.T, host *testHost) snapshot {
	t.Helper()
	host.mu.Lock()
	defer host.mu.Unlock()
	values := host.entries[host.sessionID][entryKind]
	if len(values) == 0 {
		t.Fatal("no persisted goal state")
	}
	var got snapshot
	if err := json.Unmarshal(values[len(values)-1], &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestStartGoalTool(t *testing.T) {
	host := newTestHost("tool-start")
	r := setup(t, host)
	tool, ok := r.Tool("start_goal")
	if !ok || !strings.Contains(tool.Definition.Description, "explicitly asks") {
		t.Fatalf("start_goal tool = %#v, registered = %v", tool.Definition, ok)
	}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"objective":"fix the parser",
		"checks":["focused test passes","docs explain it"],
		"max_steps":4
	}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("start_goal = %#v, %v", result, err)
	}
	if strings.Contains(result.Content, "pause") || !strings.Contains(result.Content, "complete only with evidence") {
		t.Fatalf("start_goal guidance = %q", result.Content)
	}
	got := latest(t, host)
	if got.Status != "active" || got.Objective != "fix the parser" || got.MaxSteps != 4 || len(got.Checks) != 2 {
		t.Fatalf("started goal = %#v", got)
	}
	if len(host.followUps) != 0 {
		t.Fatalf("tool start queued duplicate follow-up = %#v", host.followUps)
	}

	result, err = tool.Execute(context.Background(), json.RawMessage(`{"objective":"other","checks":["done"],"max_steps":8}`), nil)
	if err != nil || !result.IsError || !strings.Contains(result.Content, "already active") {
		t.Fatalf("second start_goal = %#v, %v", result, err)
	}

	defaultHost := newTestHost("tool-default")
	defaultRegistry := setup(t, defaultHost)
	defaultTool, _ := defaultRegistry.Tool("start_goal")
	result, err = defaultTool.Execute(context.Background(), json.RawMessage(`{"objective":"work","checks":["done"]}`), nil)
	if err != nil || result.IsError || latest(t, defaultHost).MaxSteps != defaultMaxSteps {
		t.Fatalf("default start_goal = %#v, state = %#v, error = %v", result, latest(t, defaultHost), err)
	}
}

func TestStartGoalToolValidatesInput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "missing objective", raw: `{"checks":["done"],"max_steps":8}`, want: "objective"},
		{name: "empty check", raw: `{"objective":"work","checks":[""],"max_steps":8}`, want: "cannot be empty"},
		{name: "too many steps", raw: `{"objective":"work","checks":["done"],"max_steps":1000001}`, want: "max steps"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := newTestHost("invalid")
			r := setup(t, host)
			tool, _ := r.Tool("start_goal")
			result, err := tool.Execute(context.Background(), json.RawMessage(test.raw), nil)
			if err != nil || !result.IsError || !strings.Contains(result.Content, test.want) {
				t.Fatalf("start_goal = %#v, %v", result, err)
			}
		})
	}
}

func TestGoalPersistenceRestoreAndSessionIsolation(t *testing.T) {
	host := newTestHost("one")
	r := setup(t, host)
	runCommand(t, r, "start fix the parser --check focused test passes --check docs explain it --max-steps 4")
	got := latest(t, host)
	if got.Status != "active" || got.MaxSteps != 4 || len(got.Checks) != 2 || len(host.followUps) != 1 {
		t.Fatalf("started goal = %#v, follow-ups = %#v", got, host.followUps)
	}

	// A new extension process loads the append-only snapshots but never restarts work silently.
	host.followUps = nil
	r = setup(t, host)
	got = latest(t, host)
	if got.Status != "paused" || !strings.Contains(got.Reason, "restored paused") || len(host.followUps) != 0 {
		t.Fatalf("restored goal = %#v, follow-ups = %#v", got, host.followUps)
	}

	host.mu.Lock()
	host.sessionID = "two"
	host.entries["two"] = map[string][]json.RawMessage{}
	host.mu.Unlock()
	if _, err := r.RunHooks(context.Background(), "session_change", map[string]any{"session_id": "two"}); err != nil {
		t.Fatal(err)
	}
	if status := runCommand(t, r, "status"); status != "No goal is set." {
		t.Fatalf("new session status = %q", status)
	}
}

func TestGoalNoProgressAndStepLimits(t *testing.T) {
	t.Run("repeated no progress", func(t *testing.T) {
		host := newTestHost("no-progress")
		r := setup(t, host)
		runCommand(t, r, "start do work --check check result --max-steps 10")
		for cycle := 1; cycle <= noProgressLimit; cycle++ {
			end, err := r.RunHooks(context.Background(), "agent_end", nil)
			if err != nil {
				t.Fatal(err)
			}
			if cycle < noProgressLimit && end["follow_up"] == nil {
				t.Fatalf("cycle %d stopped too early: %#v", cycle, end)
			}
			if cycle == noProgressLimit && (end["follow_up"] != nil || latest(t, host).Reason != "stopped after repeated no progress") {
				t.Fatalf("cycle %d end = %#v, state = %#v", cycle, end, latest(t, host))
			}
		}
	})

	t.Run("finite step limit", func(t *testing.T) {
		host := newTestHost("limit")
		r := setup(t, host)
		runCommand(t, r, "start do work --check check result --max-steps 1")
		progress, _ := r.Tool("report_goal_progress")
		result, err := progress.Execute(context.Background(), json.RawMessage(`{"summary":"made a change"}`), nil)
		if err != nil || result.IsError {
			t.Fatalf("progress = %#v, %v", result, err)
		}
		end, err := r.RunHooks(context.Background(), "agent_end", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := latest(t, host)
		if end["follow_up"] != nil || got.Status != "paused" || got.Reason != "goal step limit reached" {
			t.Fatalf("end = %#v, state = %#v", end, got)
		}
	})
}

func TestGoalAbortAndErrorsPause(t *testing.T) {
	for _, reason := range []string{"canceled", "max_turns", "max_cost", "error"} {
		t.Run(reason, func(t *testing.T) {
			host := newTestHost(reason)
			r := setup(t, host)
			runCommand(t, r, "start do work --check check result")
			if _, err := r.RunHooks(context.Background(), "run_end", map[string]any{"stop_reason": reason, "cost_known": false}); err != nil {
				t.Fatal(err)
			}
			got := latest(t, host)
			if got.Status != "paused" || got.Reason == "" || !strings.Contains(got.CostSummary, "unknown") {
				t.Fatalf("paused state = %#v", got)
			}
		})
	}
}

func TestGoalRejectsFalseCompletion(t *testing.T) {
	host := newTestHost("completion")
	r := setup(t, host)
	runCommand(t, r, "start ship it --check tests pass --check docs updated")

	// Prose and an ordinary agent end do not mark a goal complete.
	if _, err := r.RunHooks(context.Background(), "agent_end", map[string]any{"final_assistant_text": "Everything is done"}); err != nil {
		t.Fatal(err)
	}
	if got := latest(t, host); got.Status != "active" {
		t.Fatalf("prose completed goal: %#v", got)
	}

	complete, _ := r.Tool("complete_goal")
	premature, err := complete.Execute(context.Background(), json.RawMessage(`{"evidence":[{"check":"tests pass","evidence":"go test passed"},{"check":"docs updated","evidence":"README changed"}]}`), nil)
	if err != nil || !premature.IsError {
		t.Fatalf("premature completion result = %#v, %v", premature, err)
	}
	progress, _ := r.Tool("report_goal_progress")
	if result, err := progress.Execute(context.Background(), json.RawMessage(`{"summary":"implemented and checked the change","evidence":"working tree and test output"}`), nil); err != nil || result.IsError {
		t.Fatalf("progress result = %#v, %v", result, err)
	}
	bad, err := complete.Execute(context.Background(), json.RawMessage(`{"evidence":[{"check":"tests pass","evidence":"go test passed"}]}`), nil)
	if err != nil || !bad.IsError {
		t.Fatalf("missing evidence result = %#v, %v", bad, err)
	}
	if got := latest(t, host); got.Status != "active" {
		t.Fatalf("bad evidence completed goal: %#v", got)
	}

	good, err := complete.Execute(context.Background(), json.RawMessage(`{"evidence":[{"check":"docs updated","evidence":"README section added"},{"check":"tests pass","evidence":"go test ./... passed"}]}`), nil)
	if err != nil || good.IsError {
		t.Fatalf("valid evidence result = %#v, %v", good, err)
	}
	got := latest(t, host)
	if got.Status != "completed" || len(got.Completion) != 2 {
		t.Fatalf("completed state = %#v", got)
	}
	if status := runCommand(t, r, "status"); !strings.Contains(status, "Completion evidence:") {
		t.Fatalf("status = %q", status)
	}
}

func TestGoalPersistenceFailureRollsBackState(t *testing.T) {
	host := newTestHost("disk-error")
	r := setup(t, host)
	runCommand(t, r, "start work --check done")
	progress, _ := r.Tool("report_goal_progress")
	if result, err := progress.Execute(context.Background(), json.RawMessage(`{"summary":"work changed"}`), nil); err != nil || result.IsError {
		t.Fatalf("progress = %#v, %v", result, err)
	}
	host.mu.Lock()
	host.appendErr = errors.New("disk full")
	host.mu.Unlock()
	complete, _ := r.Tool("complete_goal")
	if _, err := complete.Execute(context.Background(), json.RawMessage(`{"evidence":[{"check":"done","evidence":"focused check passed"}]}`), nil); err == nil {
		t.Fatal("completion succeeded despite persistence failure")
	}
	host.mu.Lock()
	host.appendErr = nil
	host.mu.Unlock()
	if got := latest(t, host); got.Status != "active" {
		t.Fatalf("durable state = %#v", got)
	}
	if status := runCommand(t, r, "status"); !strings.Contains(status, "Goal active") {
		t.Fatalf("in-memory status after rollback = %q", status)
	}
}

func TestGoalPauseRemovedClearAndFailedQueue(t *testing.T) {
	host := newTestHost("commands")
	r := setup(t, host)
	runCommand(t, r, "start work --check done")
	command, _ := r.Command("goal")
	if _, err := command.Execute(context.Background(), "pause"); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("pause command err = %v", err)
	}
	if _, ok := r.Tool("pause_goal"); ok {
		t.Fatal("pause_goal tool is still registered")
	}
	runCommand(t, r, "clear")
	if got := latest(t, host); got.Status != "cleared" {
		t.Fatalf("cleared = %#v", got)
	}

	host = newTestHost("queue-error")
	host.followErr = errors.New("queue unavailable")
	r = setup(t, host)
	command, _ = r.Command("goal")
	if _, err := command.Execute(context.Background(), "start work --check done"); err == nil {
		t.Fatal("expected queue error")
	}
	if got := latest(t, host); got.Status != "paused" || !strings.Contains(got.Reason, "could not start") {
		t.Fatalf("queue failure state = %#v", got)
	}
}

func TestGoalHardModelTurnLimitStopsToolLoops(t *testing.T) {
	host := newTestHost("turn-limit")
	r := setup(t, host)
	runCommand(t, r, "start bounded work --check done --max-steps 1")
	for turn := 0; turn < turnsPerStep; turn++ {
		result, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
		if err != nil || result["stop"] == true {
			t.Fatalf("turn %d result = %#v, %v", turn, result, err)
		}
	}
	result, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
	if err != nil || result["stop"] != true || result["stop_reason"] != "goal_turn_limit" {
		t.Fatalf("limit result = %#v, %v", result, err)
	}
	got := latest(t, host)
	if got.Status != "paused" || got.Turns != turnsPerStep || got.Reason != "goal model-turn limit reached" {
		t.Fatalf("limited state = %#v", got)
	}
	command, _ := r.Command("goal")
	if _, err := command.Execute(context.Background(), "resume"); err == nil || !strings.Contains(err.Error(), "model-turn limit") {
		t.Fatalf("resume exhausted goal err = %v", err)
	}
}

func TestGoalResumeRaceRestoresPausedBeforeAgentStart(t *testing.T) {
	host := newTestHost("one")
	r := setup(t, host)
	runCommand(t, r, "start first work --check done")

	loaded := snapshot{Version: 1, ID: "saved", Objective: "saved work", Checks: []string{"done"}, Status: "active", MaxSteps: 4}
	raw, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.sessionID = "two"
	host.entries["two"] = map[string][]json.RawMessage{entryKind: {raw}}
	host.mu.Unlock()

	// before_agent_start may beat the asynchronous TUI session_change hook.
	result, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result["system_prompt"].(string), "saved work") {
		t.Fatalf("saved active goal restarted silently: %#v", result)
	}
	got := latest(t, host)
	if got.Status != "paused" || !strings.Contains(got.Reason, "restored paused") {
		t.Fatalf("restored state = %#v", got)
	}
}

func TestGoalDeniedToolPausesAndStopsCurrentRun(t *testing.T) {
	host := newTestHost("denied")
	r := setup(t, host)
	runCommand(t, r, "start protected work --check done")
	if _, err := r.RunHooks(context.Background(), "tool_denied", map[string]any{"name": "deploy", "reason": "approval required"}); err != nil {
		t.Fatal(err)
	}
	got := latest(t, host)
	if got.Status != "paused" || !strings.Contains(got.Reason, "approval required") {
		t.Fatalf("denied state = %#v", got)
	}
	result, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
	if err != nil || result["stop"] != true {
		t.Fatalf("stop after denial = %#v, %v", result, err)
	}
}

func TestGoalRunEndPersistenceFailureStaysPausedInMemory(t *testing.T) {
	host := newTestHost("run-end-disk")
	r := setup(t, host)
	runCommand(t, r, "start work --check done")
	host.mu.Lock()
	host.appendErr = errors.New("disk full")
	host.mu.Unlock()
	if _, err := r.RunHooks(context.Background(), "run_end", map[string]any{"stop_reason": "canceled", "cost_known": false}); err != nil {
		t.Fatal(err)
	}
	command, _ := r.Command("goal")
	status, err := command.Execute(context.Background(), "status")
	if err != nil || !strings.Contains(status, "Goal paused") || !strings.Contains(status, "could not be persisted") {
		t.Fatalf("status = %q, %v", status, err)
	}
	result, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
	if err != nil || result["stop"] != true {
		t.Fatalf("unsafe restart result = %#v, %v", result, err)
	}
}

func TestGoalCompletionAllowsOneFinalTurnThenStops(t *testing.T) {
	host := newTestHost("completion-bound")
	r := setup(t, host)
	runCommand(t, r, "start work --check done")
	progress, _ := r.Tool("report_goal_progress")
	if result, err := progress.Execute(context.Background(), json.RawMessage(`{"summary":"work finished"}`), nil); err != nil || result.IsError {
		t.Fatalf("progress = %#v, %v", result, err)
	}
	complete, _ := r.Tool("complete_goal")
	if result, err := complete.Execute(context.Background(), json.RawMessage(`{"evidence":[{"check":"done","evidence":"focused check passed"}]}`), nil); err != nil || result.IsError {
		t.Fatalf("completion = %#v, %v", result, err)
	}
	final, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
	if err != nil || final["stop"] == true {
		t.Fatalf("final turn = %#v, %v", final, err)
	}
	stopped, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
	if err != nil || stopped["stop"] != true || stopped["stop_reason"] != "goal_completed" {
		t.Fatalf("post-completion stop = %#v, %v", stopped, err)
	}
}

func TestDelayedSessionChangeDoesNotUndoExplicitResume(t *testing.T) {
	host := newTestHost("delayed")
	r := setup(t, host)
	runCommand(t, r, "start work --check done")
	if _, err := r.RunHooks(context.Background(), "session_shutdown", nil); err != nil {
		t.Fatal(err)
	}
	runCommand(t, r, "resume")
	if _, err := r.RunHooks(context.Background(), "session_change", map[string]any{"session_id": "delayed"}); err != nil {
		t.Fatal(err)
	}
	if got := latest(t, host); got.Status != "active" {
		t.Fatalf("delayed session hook overwrote resume: %#v", got)
	}
}

func TestDefaultGoalContinuesWhileProgressing(t *testing.T) {
	host := newTestHost("productive")
	r := setup(t, host)
	runCommand(t, r, "start substantial work --check verified")
	progress, _ := r.Tool("report_goal_progress")
	for cycle := 0; cycle < 60; cycle++ {
		for turn := 0; turn < turnsPerStep+1; turn++ {
			before, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
			if err != nil || before["stop"] == true {
				t.Fatalf("cycle %d turn %d stopped: %#v, %v", cycle, turn, before, err)
			}
		}
		raw := json.RawMessage(fmt.Sprintf(`{"summary":"step %d","evidence":"verified change %d"}`, cycle, cycle))
		result, err := progress.Execute(context.Background(), raw, nil)
		if err != nil || result.IsError {
			t.Fatalf("progress = %#v, %v", result, err)
		}
		end, err := r.RunHooks(context.Background(), "agent_end", nil)
		if err != nil || end["follow_up"] == nil {
			t.Fatalf("cycle %d end = %#v, %v", cycle, end, err)
		}
	}
	if got := latest(t, host); got.MaxSteps != 0 || got.Status != "active" || got.Steps != 60 {
		t.Fatalf("productive goal = %#v", got)
	}
}

func TestResumeClearsStallCount(t *testing.T) {
	host := newTestHost("stalled")
	r := setup(t, host)
	runCommand(t, r, "start work --check verified")
	for cycle := 0; cycle < noProgressLimit; cycle++ {
		if _, err := r.RunHooks(context.Background(), "agent_end", nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := latest(t, host); got.Status != "paused" {
		t.Fatalf("stalled goal = %#v", got)
	}
	runCommand(t, r, "resume")
	if got := latest(t, host); got.Status != "active" || got.NoProgress != 0 {
		t.Fatalf("resumed goal = %#v", got)
	}
}

func TestDefaultGoalRestoreAndResumeKeepsUnlimitedLimits(t *testing.T) {
	host := newTestHost("unlimited-restore")
	r := setup(t, host)
	runCommand(t, r, "start work --check verified")
	host.followUps = nil
	r = setup(t, host)
	if got := latest(t, host); got.Status != "paused" || got.MaxSteps != 0 || len(host.followUps) != 0 {
		t.Fatalf("restored default goal = %#v", got)
	}
	runCommand(t, r, "resume")
	before, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
	if err != nil || before["stop"] == true || latest(t, host).Status != "active" {
		t.Fatalf("resumed default goal = %#v, %v", before, err)
	}
}

func TestExplicitGoalLimitRemainsExhaustedAfterRestore(t *testing.T) {
	host := newTestHost("legacy-limit")
	r := setup(t, host)
	runCommand(t, r, "start bounded work --check verified --max-steps 1")
	if _, err := r.RunHooks(context.Background(), "agent_end", nil); err != nil {
		t.Fatal(err)
	}
	r = setup(t, host)
	command, _ := r.Command("goal")
	if _, err := command.Execute(context.Background(), "resume"); err == nil || !strings.Contains(err.Error(), "step limit") {
		t.Fatalf("stored explicit limit was lost: %v", err)
	}
}
