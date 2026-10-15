package herdr

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/extension"
)

type recordedCall struct {
	name string
	args []string
}

func testReporter(t *testing.T) (*Reporter, <-chan recordedCall) {
	t.Helper()
	calls := make(chan recordedCall, 20)
	r := newReporter("/tmp/herdr", "w1:p1", func(_ context.Context, name string, args ...string) error {
		calls <- recordedCall{name: name, args: append([]string(nil), args...)}
		return nil
	})
	t.Cleanup(r.Close)
	return r, calls
}

func nextCall(t *testing.T, calls <-chan recordedCall) recordedCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Herdr call")
		return recordedCall{}
	}
}

func argValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func resumeCommand(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:]
		}
	}
	return nil
}

func TestNewFromEnvironmentRequiresHerdrPane(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	if reporter := NewFromEnvironment(); reporter != nil {
		reporter.Close()
		t.Fatal("reporter created outside Herdr")
	}

	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_BIN_PATH", "")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	if reporter := NewFromEnvironment(); reporter != nil {
		reporter.Close()
		t.Fatal("reporter created without Herdr binary")
	}
}

func TestReporterPublishesStateResumeAndRelease(t *testing.T) {
	r, calls := testReporter(t)
	r.Start(Session{ID: "session-1", Provider: "openai", Model: "gpt-test", Thinking: "high", Mode: "line"})

	initial := nextCall(t, calls)
	if initial.name != "/tmp/herdr" || argValue(initial.args, "--state") != "idle" || argValue(initial.args, "--agent-session-id") != "session-1" {
		t.Fatalf("initial call = %#v", initial)
	}
	wantResume := []string{"notch", "--resume", "session-1", "--provider", "openai", "--model", "gpt-test", "--thinking", "high", "--no-tui"}
	if got := resumeCommand(initial.args); !reflect.DeepEqual(got, wantResume) {
		t.Fatalf("resume command = %#v, want %#v", got, wantResume)
	}

	r.SetState("working", "")
	working := nextCall(t, calls)
	if got := argValue(working.args, "--state"); got != "working" {
		t.Fatalf("state = %q, want working", got)
	}
	if argValue(working.args, "--seq") <= argValue(initial.args, "--seq") {
		t.Fatalf("sequence did not increase: %q then %q", argValue(initial.args, "--seq"), argValue(working.args, "--seq"))
	}

	r.SetThinking("minimal")
	thinking := nextCall(t, calls)
	if got := resumeCommand(thinking.args); len(got) == 0 || argValue(got, "--thinking") != "minimal" {
		t.Fatalf("thinking resume command = %#v", got)
	}

	r.Close()
	release := nextCall(t, calls)
	if len(release.args) < 2 || release.args[0] != "pane" || release.args[1] != "release-agent" {
		t.Fatalf("release call = %#v", release)
	}
}

func TestReporterHooksTrackRunsAndSessionChanges(t *testing.T) {
	r, calls := testReporter(t)
	registry := extension.NewRegistry()
	r.Register(registry)
	r.Start(Session{ID: "old", Provider: "anthropic", Model: "model"})
	_ = nextCall(t, calls)

	if _, err := registry.RunHooks(context.Background(), "before_agent_start", map[string]any{
		"provider": "openai", "model": "new-model", "thinking_level": "high",
	}); err != nil {
		t.Fatal(err)
	}
	started := nextCall(t, calls)
	if got := argValue(started.args, "--state"); got != "working" {
		t.Fatalf("start state = %q", got)
	}
	if got := resumeCommand(started.args); !reflect.DeepEqual(got, []string{"notch", "--resume", "old", "--provider", "openai", "--model", "new-model", "--thinking", "high"}) {
		t.Fatalf("updated resume command = %#v", got)
	}
	if _, err := registry.RunHooks(context.Background(), "run_end", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := argValue(nextCall(t, calls).args, "--state"); got != "idle" {
		t.Fatalf("end state = %q", got)
	}
	if _, err := registry.RunHooks(context.Background(), "session_change", map[string]any{"session_id": "new"}); err != nil {
		t.Fatal(err)
	}
	changed := nextCall(t, calls)
	if got := argValue(changed.args, "--agent-session-id"); got != "new" {
		t.Fatalf("changed session = %q", got)
	}
}

type blockingHost struct {
	extension.Host
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockingHost) Input(context.Context, string, string) (string, error) {
	h.once.Do(func() { close(h.entered) })
	<-h.release
	return "answer", nil
}

func TestWrappedHostReportsBlockedAndRestoresWorking(t *testing.T) {
	r, calls := testReporter(t)
	r.Start(Session{})
	_ = nextCall(t, calls)
	r.SetState("working", "")
	_ = nextCall(t, calls)

	base := &blockingHost{entered: make(chan struct{}), release: make(chan struct{})}
	host := WrapHost(base, r)
	done := make(chan struct{})
	go func() {
		_, _ = host.Input(context.Background(), "Choose a path", "")
		close(done)
	}()
	<-base.entered

	blocked := nextCall(t, calls)
	if got := argValue(blocked.args, "--state"); got != "blocked" {
		t.Fatalf("blocked state = %q", got)
	}
	if got := argValue(blocked.args, "--message"); got != "Choose a path" {
		t.Fatalf("blocked message = %q", got)
	}

	r.SetModel("openai", "new-model")
	metadata := nextCall(t, calls)
	if got := argValue(metadata.args, "--state"); got != "blocked" {
		t.Fatalf("state after blocked metadata update = %q", got)
	}
	if got := argValue(metadata.args, "--message"); got != "Choose a path" {
		t.Fatalf("message after blocked metadata update = %q", got)
	}

	close(base.release)
	<-done
	if got := argValue(nextCall(t, calls).args, "--state"); got != "working" {
		t.Fatalf("restored state = %q", got)
	}
}

func TestResumeArgsRejectUnsafeAndPrintValues(t *testing.T) {
	if got := resumeArgs(Session{ID: "bad'id"}); got != nil {
		t.Fatalf("unsafe resume args = %#v", got)
	}
	if got := resumeArgs(Session{ID: "print", Mode: "print"}); got != nil {
		t.Fatalf("print resume args = %#v", got)
	}
	if got := resumeArgs(Session{ID: "safe", Mode: "rpc"}); !reflect.DeepEqual(got, []string{"notch", "--resume", "safe", "--rpc"}) {
		t.Fatalf("rpc resume args = %#v", got)
	}
}
