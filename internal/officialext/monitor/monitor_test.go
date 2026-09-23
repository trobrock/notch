package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/extension"
)

type testHost struct {
	mu          sync.Mutex
	followups   []string
	followUpErr error
	notices     []string
}

func (*testHost) CWD() string { return "." }
func (*testHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, errors.New("unused")
}
func (*testHost) Input(context.Context, string, string) (string, error)    { return "", nil }
func (*testHost) Select(context.Context, string, []string) (string, error) { return "", nil }
func (h *testHost) Notify(message, _ string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notices = append(h.notices, message)
}
func (h *testHost) FollowUp(s string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.followups = append(h.followups, s)
	return h.followUpErr
}
func (*testHost) Handoff(string, bool) error                                       { return nil }
func (*testHost) SetActiveTools([]string) error                                    { return nil }
func (*testHost) SwitchModel(context.Context, string, string) (string, int, error) { return "", 0, nil }
func (*testHost) ListModels(context.Context, string, bool) ([]extension.ModelInfo, error) {
	return nil, nil
}
func (*testHost) AppendSessionEntry(string, any) error             { return nil }
func (*testHost) SessionEntries(string) ([]json.RawMessage, error) { return nil, nil }
func (*testHost) EditorText(context.Context) (string, error)       { return "", nil }
func (*testHost) SetEditorText(context.Context, string) error      { return nil }
func (*testHost) SetStatus(string, string)                         {}
func (*testHost) SetPanel(string, string, []string)                {}

func setup(t *testing.T) (*extension.Registry, *testHost) {
	t.Helper()
	r, h := extension.NewRegistry(), &testHost{}
	if err := Register(r, h); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.RunHooks(context.Background(), "session_shutdown", nil) })
	return r, h
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMonitorToolsTellAgentNotToWait(t *testing.T) {
	r, _ := setup(t)
	for _, name := range []string{"monitor_command", "monitor_github_pr_checks"} {
		tool, ok := r.Tool(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		if !strings.Contains(strings.ToLower(tool.Definition.Description), "do not use sleep or polling") {
			t.Fatalf("%s description does not discourage waiting: %q", name, tool.Definition.Description)
		}
	}
}

func TestMonitorLifecycleHooks(t *testing.T) {
	r, _ := setup(t)
	started := make(chan map[string]any, 1)
	ended := make(chan map[string]any, 1)
	r.On("monitor_start", "test", func(_ context.Context, event map[string]any) (map[string]any, error) {
		started <- event
		return nil, nil
	})
	r.On("monitor_end", "test", func(_ context.Context, event map[string]any) (map[string]any, error) {
		ended <- event
		return nil, nil
	})

	tool, _ := r.Tool("monitor_command")
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf done","name":"lifecycle"}`), nil); err != nil {
		t.Fatal(err)
	}

	startEvent := <-started
	if active, _ := startEvent["active"].(bool); !active {
		t.Fatalf("monitor_start active = %#v", startEvent["active"])
	}
	startMonitor, _ := startEvent["monitor"].(map[string]any)
	if startMonitor["id"] != "mon-1" || startMonitor["status"] != "running" {
		t.Fatalf("monitor_start monitor = %#v", startMonitor)
	}
	if monitors, _ := startEvent["monitors"].([]map[string]any); len(monitors) != 1 {
		t.Fatalf("monitor_start monitors = %#v", startEvent["monitors"])
	}

	select {
	case endEvent := <-ended:
		if active, _ := endEvent["active"].(bool); active {
			t.Fatalf("monitor_end active = %#v", endEvent["active"])
		}
		endMonitor, _ := endEvent["monitor"].(map[string]any)
		if endMonitor["id"] != "mon-1" || endMonitor["status"] != "completed" {
			t.Fatalf("monitor_end monitor = %#v", endMonitor)
		}
		if endMonitor["completed_at"] == nil {
			t.Fatalf("monitor_end missing completed_at: %#v", endMonitor)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("monitor_end hook was not emitted")
	}
}

func TestMonitorCommandExitAndList(t *testing.T) {
	r, h := setup(t)
	tool, _ := r.Tool("monitor_command")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf done","name":"test"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Started monitor mon-1") || !strings.Contains(result.Content, waitGuidance) {
		t.Fatalf("result=%#v", result)
	}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.followups) == 1 })
	list, _ := r.Tool("list_monitors")
	listed, err := list.Execute(context.Background(), json.RawMessage(`{"id":"mon-1","includeOutput":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed.Content, "[completed]") || !strings.Contains(listed.Content, "done") {
		t.Fatalf("list=%q", listed.Content)
	}
}

func TestMonitorOutputMatch(t *testing.T) {
	r, h := setup(t)
	tool, _ := r.Tool("monitor_command")
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf ready; sleep 1","trigger":"output_match","pattern":"ready","stopOnTrigger":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.followups) == 1 })
	h.mu.Lock()
	message := h.followups[0]
	h.mu.Unlock()
	if !strings.Contains(message, "output matched /ready/") {
		t.Fatalf("message=%q", message)
	}
}

func TestMonitorValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"command":" "}`, `{"command":"x","trigger":"bad"}`, `{"command":"x","trigger":"output_match"}`, `{"command":"x","pattern":"["}`, `{"command":"x","trigger":"timeout"}`} {
		if _, err := decodeCommand(json.RawMessage(raw)); err == nil {
			t.Fatalf("decode(%s) succeeded", raw)
		}
	}
}

func TestStopMonitor(t *testing.T) {
	r, _ := setup(t)
	start, _ := r.Tool("monitor_command")
	_, err := start.Execute(context.Background(), json.RawMessage(`{"command":"sleep 5"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	stop, _ := r.Tool("stop_monitor")
	result, err := stop.Execute(context.Background(), json.RawMessage(`{"id":"mon-1"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "Stopped monitor mon-1" {
		t.Fatalf("result=%#v", result)
	}
}

func TestMonitorReportsWakeupDeliveryFailure(t *testing.T) {
	r, h := setup(t)
	h.followUpErr = errors.New("agent is idle")
	tool, _ := r.Tool("monitor_command")
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf done","name":"delivery"}`), nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.notices) != 0
	})
	list, _ := r.Tool("list_monitors")
	result, err := list.Execute(context.Background(), json.RawMessage(`{"status":"all"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Wake-up delivery failed: agent is idle") {
		t.Fatalf("list=%q", result.Content)
	}
}

func TestListMonitorsFiltersAndChanges(t *testing.T) {
	now := time.Now().Add(-time.Minute)
	output := &lockedBuffer{}
	_, _ = output.Write([]byte("live output"))
	s := &state{monitors: map[string]*Monitor{
		"active": {ID: "active", Name: "active job", Status: "running", StartedAt: now, output: output},
		"done":   {ID: "done", Name: "done job", Status: "completed", StartedAt: now, UpdatedAt: now, Output: "finished"},
	}}
	tool := s.listTool()
	run := func(raw string) extension.ToolResult {
		t.Helper()
		result, err := tool.Execute(context.Background(), json.RawMessage(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	initial := run(`{}`)
	if !strings.Contains(initial.Content, "active [running]") || strings.Contains(initial.Content, "done [completed]") || strings.Contains(initial.Content, "live output") {
		t.Fatalf("defaults: %s", initial.Content)
	}
	all := run(`{"status":"all"}`)
	if !strings.Contains(all.Content, "done [completed]") {
		t.Fatalf("all: %s", all.Content)
	}
	targeted := run(`{"id":"done","includeOutput":true}`)
	if !strings.Contains(targeted.Content, "finished") || strings.Contains(targeted.Content, "active job") {
		t.Fatalf("targeted: %s", targeted.Content)
	}
	since := initial.Details["snapshot"].(string)
	unchanged := run(`{"since":"` + since + `"}`)
	if !strings.Contains(unchanged.Content, "No matching monitors.") {
		t.Fatalf("unchanged: %s", unchanged.Content)
	}
	_, _ = output.Write([]byte(" updated"))
	changed := run(`{"id":"active","includeOutput":true,"since":"` + since + `"}`)
	if !strings.Contains(changed.Content, "live output updated") {
		t.Fatalf("updated output: %s", changed.Content)
	}
	s.monitors["active"].Status = "completed"
	s.monitors["active"].UpdatedAt = time.Now()
	completed := run(`{"since":"` + changed.Details["snapshot"].(string) + `"}`)
	if !strings.Contains(completed.Content, "active [completed]") {
		t.Fatalf("completion delta: %s", completed.Content)
	}
	for _, raw := range []string{`{"includeOutput":true}`, `{"since":"bad"}`, `{"status":"bad"}`, `{"id":"absent"}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestLockedBufferCoalescesWriteNotifications(t *testing.T) {
	b := &lockedBuffer{changed: make(chan struct{}, 1)}
	for _, text := range []string{"rea", "dy"} {
		if _, err := b.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-b.changed:
	default:
		t.Fatal("no output notification")
	}
	select {
	case <-b.changed:
		t.Fatal("writes were not coalesced")
	default:
	}
	if b.String() != "ready" {
		t.Fatalf("lost output: %q", b.String())
	}
	if _, err := b.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.changed:
	default:
		t.Fatal("no subsequent notification")
	}
}

func TestOutputMatchOnImmediateExit(t *testing.T) {
	r, h := setup(t)
	tool, _ := r.Tool("monitor_command")
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf ready","trigger":"output_match","pattern":"ready"}`), nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.followups) > 0 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.followups) != 1 || !strings.Contains(h.followups[0], "output matched /ready/") {
		t.Fatalf("followups: %#v", h.followups)
	}
}

func TestMonitorTimeoutNotification(t *testing.T) {
	r, h := setup(t)
	tool, _ := r.Tool("monitor_command")
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"exec sleep 5","trigger":"timeout","timeoutSeconds":1,"stopOnTrigger":true}`), nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.followups) > 0 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.followups) != 1 || !strings.Contains(h.followups[0], "timeout after 1 seconds") {
		t.Fatalf("followups: %#v", h.followups)
	}
}

func TestTriggeredMonitorRemainsManaged(t *testing.T) {
	for _, action := range []string{"stop", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			h := &testHost{}
			r := extension.NewRegistry()
			s := &state{host: h, registry: r, cwd: ".", monitors: map[string]*Monitor{}}
			t.Cleanup(s.close)
			m, err := s.start(commandInput{Command: "printf ready; exec sleep 30", Trigger: "output_match", Pattern: "ready"})
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return m.Status == "triggered" })
			s.mu.Lock()
			for i := 0; i < maxCompleted+1; i++ {
				id := fmt.Sprintf("done-%d", i)
				s.monitors[id] = &Monitor{ID: id, Status: "completed", CompletedAt: time.Now()}
			}
			s.pruneLocked()
			retained := s.monitors[m.ID] == m
			s.mu.Unlock()
			if !retained {
				t.Fatal("live triggered monitor pruned")
			}
			active := false
			r.On("test_activity", "test", func(_ context.Context, event map[string]any) (map[string]any, error) {
				active = event["active"].(bool)
				return nil, nil
			})
			s.emitLifecycle("test_activity", m)
			if !active {
				t.Fatal("triggered monitor absent from lifecycle activity")
			}
			if action == "stop" {
				if err := s.stop(m.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				s.close()
			}
			waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return m.output == nil })
			s.mu.Lock()
			defer s.mu.Unlock()
			if m.Status != "cancelled" {
				t.Fatalf("status=%s", m.Status)
			}
		})
	}
}
