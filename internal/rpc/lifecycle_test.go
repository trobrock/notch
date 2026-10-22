package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
)

func TestExclusiveCommandReservesRunnerAndQueuesIdleWake(t *testing.T) {
	registry := extension.NewRegistry()
	provider := &rpcTestProvider{started: make(chan struct{}), release: make(chan struct{})}
	runner, err := agent.New(agent.Config{Registry: registry, Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	server := New(strings.NewReader(""), &output, "/work")
	server.Configure(runner, registry, nil, StateConfig{})
	server.runCtx = context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	if err := registry.RegisterCommand(extension.Command{Name: "exclusive", Execute: func(context.Context, string) (string, error) { close(started); <-release; return "ok", nil }}); err != nil {
		t.Fatal(err)
	}
	if err := server.handle(context.Background(), command{Type: "extension_command", Name: "exclusive"}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := server.handle(context.Background(), command{Type: "prompt", Message: "must not start"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "exclusive extension command is running") {
		t.Fatal("prompt bypassed exclusive command")
	}
	if err := server.handle(context.Background(), command{Type: "extension_command", Name: "exclusive"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "another extension command is running") {
		t.Fatal("second command was not rejected")
	}
	if err := server.FollowUp("queued wake"); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	queued := len(server.pending)
	active := server.active
	server.mu.Unlock()
	if queued != 1 || active {
		t.Fatalf("wake was not reserved: queued=%d active=%v", queued, active)
	}
	close(release)
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("queued wake did not start")
	}
	close(provider.release)
	server.wg.Wait()
	if !strings.Contains(output.String(), "agent_settled") {
		t.Fatal("wake did not finish")
	}
}

type settlingWriter struct {
	bytes.Buffer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *settlingWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"type":"agent_settled"`)) {
		w.once.Do(func() { close(w.entered); <-w.release })
	}
	return w.Buffer.Write(p)
}
func TestWakeDuringSettlementStartsAfterTerminalEvent(t *testing.T) {
	registry := extension.NewRegistry()
	release := make(chan struct{})
	close(release)
	provider := &rpcTestProvider{started: make(chan struct{}), release: release}
	runner, err := agent.New(agent.Config{Registry: registry, Provider: provider, Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	writer := &settlingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	server := New(strings.NewReader(""), writer, "/work")
	server.Configure(runner, registry, nil, StateConfig{})
	server.runCtx = context.Background()
	if err := server.FollowUp("first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first run did not settle")
	}
	if err := server.FollowUp("second"); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatal("next run started before terminal output")
	}
	close(writer.release)
	server.wg.Wait()
	var events []string
	decoder := json.NewDecoder(bytes.NewReader(writer.Bytes()))
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
		kind, _ := event["type"].(string)
		if kind == "agent_start" || kind == "agent_settled" {
			events = append(events, kind)
		}
	}
	if strings.Join(events, ",") != "agent_start,agent_settled,agent_start,agent_settled" {
		t.Fatalf("terminal ordering = %v", events)
	}
}

func TestAsyncExtensionResponseFailureStopsRPC(t *testing.T) {
	inR, inW := io.Pipe()
	defer inW.Close()
	registry := extension.NewRegistry()
	if err := registry.RegisterCommand(extension.Command{Name: "ok", Execute: func(context.Context, string) (string, error) { return "ok", nil }}); err != nil {
		t.Fatal(err)
	}
	server := New(inR, failingWriter{}, "/work")
	server.Configure(&agent.Agent{}, registry, nil, StateConfig{})
	done := make(chan error, 1)
	go func() { done <- server.Run(context.Background()) }()
	if _, err := io.WriteString(inW, "{\"type\":\"extension_command\",\"name\":\"ok\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "write failed") {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broken output left RPC running")
	}
}

type readingInput struct {
	*io.PipeReader
	started chan struct{}
	once    sync.Once
}

func (r *readingInput) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.PipeReader.Read(p)
}
func TestCancellationInterruptsIdleRPCInput(t *testing.T) {
	inR, inW := io.Pipe()
	defer inW.Close()
	input := &readingInput{PipeReader: inR, started: make(chan struct{})}
	server := New(input, io.Discard, "/work")
	server.Configure(&agent.Agent{}, extension.NewRegistry(), nil, StateConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	<-input.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel left RPC blocked on stdin")
	}
}

func TestAcceptedWakeCannotBeOvertakenAfterCommandRelease(t *testing.T) {
	for _, incomingPrompt := range []bool{false, true} {
		t.Run(fmt.Sprintf("prompt=%v", incomingPrompt), func(t *testing.T) {
			registry := extension.NewRegistry()
			release := make(chan struct{})
			close(release)
			provider := &rpcTestProvider{started: make(chan struct{}), release: release}
			runner, err := agent.New(agent.Config{Registry: registry, Provider: provider, Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			server := New(strings.NewReader(""), io.Discard, "/work")
			server.Configure(runner, registry, nil, StateConfig{})
			server.runCtx = context.Background()
			// Exact state after an exclusive command releases its reservation but
			// before its response write finishes and flushes the older queued wake.
			server.pending = []string{"older wake"}
			if incomingPrompt {
				if err := server.handlePrompt(context.Background(), command{Type: "prompt", Message: "newer wake", StreamingBehavior: "followUp"}); err != nil {
					t.Fatal(err)
				}
			} else if err := server.FollowUp("newer wake"); err != nil {
				t.Fatal(err)
			}
			server.wg.Wait()
			var users []string
			for _, message := range runner.Messages() {
				if message.Role == "user" {
					for _, block := range message.Content {
						users = append(users, block.Text)
					}
				}
			}
			if len(users) == 0 || users[0] != "older wake" {
				t.Fatalf("accepted wake overtaken: %v", users)
			}
			if !incomingPrompt && (len(users) != 2 || users[1] != "newer wake") {
				t.Fatalf("wake order=%v", users)
			}
		})
	}
}
