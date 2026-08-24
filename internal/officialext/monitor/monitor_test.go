package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/extension"
)

type testHost struct {
	mu        sync.Mutex
	followups []string
}

func (*testHost) CWD() string { return "." }
func (*testHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, errors.New("unused")
}
func (*testHost) Input(context.Context, string, string) (string, error)    { return "", nil }
func (*testHost) Select(context.Context, string, []string) (string, error) { return "", nil }
func (*testHost) Notify(string, string)                                    {}
func (h *testHost) FollowUp(s string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.followups = append(h.followups, s)
	return nil
}
func (*testHost) Handoff(string, bool) error                                       { return nil }
func (*testHost) SetActiveTools([]string) error                                    { return nil }
func (*testHost) SwitchModel(context.Context, string, string) (string, int, error) { return "", 0, nil }
func (*testHost) SetStatus(string, string)                                         {}
func (*testHost) SetPanel(string, string, []string)                                {}

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

func TestMonitorCommandExitAndList(t *testing.T) {
	r, h := setup(t)
	tool, _ := r.Tool("monitor_command")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf done","name":"test"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Started monitor mon-1") {
		t.Fatalf("result=%#v", result)
	}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.followups) == 1 })
	list, _ := r.Tool("list_monitors")
	listed, err := list.Execute(context.Background(), json.RawMessage(`{"includeOutput":true}`), nil)
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
