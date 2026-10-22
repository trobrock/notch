package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/officialext/intercom"
	"github.com/trobrock/notch/internal/session"
)

func TestIdleRPCIntercomWakeAndBusyFollowUp(t *testing.T) {
	store, err := session.New(t.TempDir(), t.TempDir(), "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &rpcTestProvider{started: make(chan struct{}), release: make(chan struct{})}
	registry := extension.NewRegistry()
	runner, err := agent.New(agent.Config{Provider: provider, Registry: registry, Model: "test", Session: store})
	if err != nil {
		t.Fatal(err)
	}
	inputR, inputW := io.Pipe()
	defer inputW.Close()
	outputR, outputW := io.Pipe()
	defer outputR.Close()
	server := New(inputR, outputW, store.Header.CWD)
	server.SetSession(store)
	server.Configure(runner, registry, nil, StateConfig{})
	records := make(chan map[string]any, 128)
	go decodeRecords(outputR, records)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx); _ = outputW.Close() }()
	// A state response proves Run is accepting work without timing or polling.
	_, _ = io.WriteString(inputW, "{\"id\":\"ready\",\"type\":\"get_state\"}\n")
	select {
	case <-records:
	case <-time.After(5 * time.Second):
		t.Fatal("RPC not ready")
	}
	if err := server.FollowUpFor("stale", "do not deliver"); err == nil {
		t.Fatal("stale wake accepted")
	}
	if err := server.FollowUpFor(store.Header.ID, "first wake"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("idle wake did not start provider")
	}
	if err := server.FollowUpFor(store.Header.ID, "busy follow-up"); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	// Wait for terminal agent_end rather than polling active state.
	for {
		select {
		case record := <-records:
			if record["type"] == "agent_end" {
				goto settled
			}
		case <-time.After(5 * time.Second):
			t.Fatal("wake did not settle")
		}
	}
settled:
	messages := runner.Messages()
	var text []string
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == "text" {
				text = append(text, block.Text)
			}
		}
	}
	joined := strings.Join(text, "\n")
	if !strings.Contains(joined, "first wake") || !strings.Contains(joined, "busy follow-up") {
		t.Fatalf("messages = %q", joined)
	}
	_ = inputW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RPC shutdown stuck")
	}
	if err := server.FollowUpFor(store.Header.ID, "after close"); err == nil {
		t.Fatal("closed RPC accepted wake")
	}
}

func TestRPCUserCanEnableIntercomWithoutModelTool(t *testing.T) {
	base := t.TempDir()
	registry := extension.NewRegistry()
	server := New(strings.NewReader(""), io.Discard, base)
	if err := intercom.Register(registry, server); err != nil {
		t.Fatal(err)
	}
	server.registry, server.runCtx = registry, context.Background()
	if _, err := registry.RunHooks(context.Background(), "session_start", map[string]any{"mode": "rpc", "cwd": base}); err != nil {
		t.Fatal(err)
	}
	// Without a durable session this must fail normally, not enable a listener.
	var output strings.Builder
	server.out = &output
	if err := server.handle(context.Background(), command{Type: "extension_command", Name: "intercom", Args: "on"}); err != nil {
		t.Fatal(err)
	}
	var result response
	server.wg.Wait()
	if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
		t.Fatal(err)
	}
	if result.Success || !strings.Contains(result.Error, "durable session") {
		t.Fatalf("response = %#v", result)
	}
	output.Reset()
	if err := server.handle(context.Background(), command{Type: "extension_command", Name: "intercom", Args: "status"}); err != nil {
		t.Fatal(err)
	}
	server.wg.Wait()
	if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("status = %#v", result)
	}
}

func TestRPCCommandHonorsStreamingPolicy(t *testing.T) {
	registry := extension.NewRegistry()
	called := false
	if err := registry.RegisterCommand(extension.Command{Name: "test", Execute: func(context.Context, string) (string, error) { called = true; return "ok", nil }}); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	server := New(strings.NewReader(""), &output, "/work")
	server.registry, server.active, server.runCtx = registry, true, context.Background()
	if err := server.handle(context.Background(), command{Type: "extension_command", Name: "test"}); err != nil {
		t.Fatal(err)
	}
	if called || !strings.Contains(output.String(), "unavailable while streaming") {
		t.Fatalf("command bypassed policy: %s", output.String())
	}
}

type intercomReplyProvider struct{}

func (*intercomReplyProvider) Stream(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	if request.Messages[len(request.Messages)-1].Role == "user" {
		for _, block := range request.Messages[len(request.Messages)-1].Content {
			_, rest, ok := strings.Cut(block.Text, "Request ID: ")
			if !ok {
				continue
			}
			id, _, _ := strings.Cut(rest, "\n")
			args, _ := json.Marshal(map[string]string{"request_id": id, "message": "review complete"})
			return model.Response{Content: []model.Block{{Type: "tool_use", ID: "reply-call", Name: "intercom_reply", Arguments: args}}, StopReason: "tool_use"}, nil
		}
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: "done"}}, StopReason: "end_turn"}, nil
}

func TestTwoIdleRPCSessionsExchangeRequestAndAutomaticReply(t *testing.T) {
	base, err := os.MkdirTemp("", "notch-rpc-ic-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	t.Setenv("NOTCH_INTERCOM_DIR", base)
	cwd := t.TempDir()
	type running struct {
		server   *Server
		registry *extension.Registry
		records  chan map[string]any
		input    *io.PipeWriter
	}
	await := func(records <-chan map[string]any, match func(map[string]any) bool) map[string]any {
		t.Helper()
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()
		for {
			select {
			case value := <-records:
				if value == nil {
					t.Fatal("RPC output closed")
				}
				if match(value) {
					return value
				}
			case <-timeout.C:
				t.Fatal("RPC response timed out")
			}
		}
	}
	start := func(provider model.Provider, name string) running {
		t.Helper()
		store, err := session.New(t.TempDir(), cwd, "test", "test")
		if err != nil {
			t.Fatal(err)
		}
		registry := extension.NewRegistry()
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		server := New(inR, outW, cwd)
		server.SetSession(store)
		if err := intercom.Register(registry, server); err != nil {
			t.Fatal(err)
		}
		runner, err := agent.New(agent.Config{Provider: provider, Registry: registry, Session: store, Model: "test"})
		if err != nil {
			t.Fatal(err)
		}
		server.Configure(runner, registry, nil, StateConfig{})
		records := make(chan map[string]any, 128)
		go decodeRecords(outR, records)
		event := map[string]any{"session_id": store.Header.ID, "cwd": cwd, "mode": "rpc"}
		if _, err := registry.RunHooks(context.Background(), "session_start", event); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- server.Run(context.Background()); _ = outW.Close() }()
		t.Cleanup(func() {
			_ = inW.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("RPC did not stop")
			}
			_, _ = registry.RunHooks(context.Background(), "session_shutdown", event)
			_ = outR.Close()
			_ = store.Close()
		})
		for _, args := range []string{"name " + name, "on", "wake on"} {
			data, _ := json.Marshal(map[string]string{"id": "setup", "type": "extension_command", "name": "intercom", "args": args})
			if _, err := inW.Write(append(data, '\n')); err != nil {
				t.Fatal(err)
			}
			result := await(records, func(r map[string]any) bool { return r["id"] == "setup" })
			if result["success"] != true {
				t.Fatalf("setup: %#v", result)
			}
		}
		return running{server, registry, records, inW}
	}
	release := make(chan struct{})
	close(release)
	a := start(&rpcTestProvider{started: make(chan struct{}), release: release}, "primary")
	_ = start(&intercomReplyProvider{}, "reviewer")
	tool, _ := a.registry.Tool("intercom_send")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"target":"reviewer","message":"review this change","wake":true}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("send = %#v, %v", result, err)
	}
	// Primary had no active model run. Its agent_end proves the peer's tool reply
	// woke it and completed a model run without a second user prompt.
	await(a.records, func(r map[string]any) bool { return r["type"] == "agent_end" })
	inbox, _ := a.registry.Tool("intercom_inbox")
	received, err := inbox.Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !strings.Contains(received.Content, "review complete") {
		t.Fatalf("reply inbox = %#v, %v", received, err)
	}
}

func TestRPCExtensionCommandAllowsInputResponsesAndCancelsOnEOF(t *testing.T) {
	for _, eof := range []bool{false, true} {
		t.Run(fmt.Sprintf("eof=%v", eof), func(t *testing.T) {
			registry := extension.NewRegistry()
			provider := &rpcTestProvider{}
			runner, err := agent.New(agent.Config{Provider: provider, Registry: registry, Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			defer inW.Close()
			defer outR.Close()
			server := New(inR, outW, "/work")
			server.Configure(runner, registry, nil, StateConfig{})
			if err := registry.RegisterCommand(extension.Command{Name: "ask", Execute: func(ctx context.Context, _ string) (string, error) { return server.Input(ctx, "Question?", "") }}); err != nil {
				t.Fatal(err)
			}
			records := make(chan map[string]any, 128)
			go decodeRecords(outR, records)
			done := make(chan error, 1)
			go func() { done <- server.Run(context.Background()); _ = outW.Close() }()
			_, _ = io.WriteString(inW, "{\"id\":\"ask\",\"type\":\"extension_command\",\"name\":\"ask\"}\n")
			var input map[string]any
			select {
			case input = <-records:
			case <-time.After(5 * time.Second):
				t.Fatal("extension input did not arrive")
			}
			if input["type"] != "input_request" {
				t.Fatalf("unexpected output: %#v", input)
			}
			if eof {
				_ = inW.Close()
			} else {
				data, _ := json.Marshal(map[string]any{"id": "answer", "type": "input_response", "requestId": input["requestId"], "value": "hello"})
				if _, err := inW.Write(append(data, '\n')); err != nil {
					t.Fatal(err)
				}
				for {
					select {
					case record := <-records:
						if record["id"] == "ask" {
							if record["success"] != true || record["data"].(map[string]any)["content"] != "hello" {
								t.Fatalf("command response: %#v", record)
							}
							goto answered
						}
					case <-time.After(5 * time.Second):
						t.Fatal("extension command blocked input_response")
					}
				}
			answered:
				_ = inW.Close()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("command not cancelled on EOF")
			}
		})
	}
}
