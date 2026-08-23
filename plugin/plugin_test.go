package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

type pipeHost struct {
	toPlugin   *io.PipeWriter
	fromPlugin *bufio.Reader
}

func newPipeHost(t *testing.T, ctx context.Context, extension Extension) (*pipeHost, <-chan error) {
	t.Helper()
	hostRead, pluginWrite := io.Pipe()
	pluginRead, hostWrite := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- ServeIO(ctx, pluginRead, pluginWrite, extension)
		_ = pluginWrite.Close()
		_ = pluginRead.Close()
	}()
	return &pipeHost{toPlugin: hostWrite, fromPlugin: bufio.NewReader(hostRead)}, done
}

func (h *pipeHost) send(t *testing.T, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.toPlugin.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (h *pipeHost) receive(t *testing.T) rpcMessage {
	t.Helper()
	line, err := h.fromPlugin.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var message rpcMessage
	if err := json.Unmarshal(line, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func request(id int, method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func TestProtocolInitializeToolProgressAndHostCalls(t *testing.T) {
	extension := Extension{
		Tools: []Tool{{
			Name: "echo", Description: "echo input", InputSchema: map[string]any{"type": "object"},
			Execute: func(ctx context.Context, args json.RawMessage, progress Progress) (ToolResult, error) {
				var input struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(args, &input); err != nil {
					return ToolResult{}, err
				}
				progress("starting")
				cwd, err := CWD(ctx)
				if err != nil {
					return ToolResult{}, err
				}
				stdout, _, code, err := Exec(ctx, "echo", "ok")
				if err != nil {
					return ToolResult{}, err
				}
				answer, err := Input(ctx, "name", "here")
				if err != nil {
					return ToolResult{}, err
				}
				choice, err := Select(ctx, "pick", []string{"a", "b"})
				if err != nil {
					return ToolResult{}, err
				}
				if err := Notify(ctx, "done", "info"); err != nil {
					return ToolResult{}, err
				}
				return ToolResult{Content: strings.Join([]string{cwd, input.Text, stdout, answer, choice}, ":"), Details: map[string]any{"code": code}}, nil
			},
		}},
		Hooks: []Hook{{Name: "before", Handle: func(_ context.Context, event map[string]any) (map[string]any, error) {
			return map[string]any{"seen": event["value"]}, nil
		}}},
		Commands: []Command{{Name: "say", Description: "say text", Execute: func(_ context.Context, args string) (string, error) {
			return strings.ToUpper(args), nil
		}}},
	}

	host, done := newPipeHost(t, context.Background(), extension)
	host.send(t, request(1, "initialize", map[string]any{}))
	initialized := host.receive(t)
	if initialized.Error != nil {
		t.Fatal(initialized.Error)
	}
	var capabilities struct {
		Tools    []ToolDefinition    `json:"tools"`
		Hooks    []map[string]string `json:"hooks"`
		Commands []map[string]string `json:"commands"`
	}
	if err := json.Unmarshal(initialized.Result, &capabilities); err != nil {
		t.Fatal(err)
	}
	if len(capabilities.Tools) != 1 || capabilities.Tools[0].Name != "echo" || capabilities.Hooks[0]["name"] != "before" || capabilities.Commands[0]["name"] != "say" {
		t.Fatalf("unexpected initialize result: %+v", capabilities)
	}

	host.send(t, request(22, "tool.execute", map[string]any{"name": "echo", "args": map[string]string{"text": "hello"}}))
	progress := host.receive(t)
	if progress.Method != "tool.update" {
		t.Fatalf("first tool message is %q, want tool.update", progress.Method)
	}
	var update struct {
		ID      int    `json:"id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(progress.Params, &update); err != nil {
		t.Fatal(err)
	}
	if update.ID != 22 || update.Message != "starting" {
		t.Fatalf("bad update: %+v", update)
	}

	cwdCall := host.receive(t)
	if cwdCall.Method != "host.cwd" || len(cwdCall.ID) == 0 {
		t.Fatalf("bad host call: %+v", cwdCall)
	}
	host.send(t, map[string]any{"jsonrpc": "2.0", "id": cwdCall.ID, "result": "/work"})

	execCall := host.receive(t)
	if execCall.Method != "host.exec" {
		t.Fatalf("bad exec call: %+v", execCall)
	}
	host.send(t, map[string]any{"jsonrpc": "2.0", "id": execCall.ID, "result": ExecResult{Stdout: "ok", ExitCode: 3}})
	inputCall := host.receive(t)
	if inputCall.Method != "host.ui.input" {
		t.Fatalf("bad input call: %+v", inputCall)
	}
	host.send(t, map[string]any{"jsonrpc": "2.0", "id": inputCall.ID, "result": "Ada"})
	selectCall := host.receive(t)
	if selectCall.Method != "host.ui.select" {
		t.Fatalf("bad select call: %+v", selectCall)
	}
	host.send(t, map[string]any{"jsonrpc": "2.0", "id": selectCall.ID, "result": "b"})
	notifyCall := host.receive(t)
	if notifyCall.Method != "host.ui.notify" {
		t.Fatalf("bad notify call: %+v", notifyCall)
	}
	host.send(t, map[string]any{"jsonrpc": "2.0", "id": notifyCall.ID, "result": nil})

	toolResponse := host.receive(t)
	var result ToolResult
	if err := json.Unmarshal(toolResponse.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Content != "/work:hello:ok:Ada:b" || result.Details["code"] != float64(3) {
		t.Fatalf("tool result = %+v", result)
	}

	host.send(t, request(23, "hook.handle", map[string]any{"name": "before", "event": map[string]string{"value": "yes"}}))
	hookResponse := host.receive(t)
	var hookResult map[string]any
	if err := json.Unmarshal(hookResponse.Result, &hookResult); err != nil {
		t.Fatal(err)
	}
	if hookResult["seen"] != "yes" {
		t.Fatalf("hook result = %#v", hookResult)
	}

	host.send(t, request(24, "command.execute", map[string]any{"name": "say", "args": "hello"}))
	commandResponse := host.receive(t)
	var output string
	if err := json.Unmarshal(commandResponse.Result, &output); err != nil {
		t.Fatal(err)
	}
	if output != "HELLO" {
		t.Fatalf("command output = %q", output)
	}

	_ = host.toPlugin.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeIO did not stop at EOF")
	}
}

func TestProtocolCancellation(t *testing.T) {
	started := make(chan struct{})
	extension := Extension{Tools: []Tool{{
		Name: "wait", Execute: func(ctx context.Context, _ json.RawMessage, _ Progress) (ToolResult, error) {
			close(started)
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	}}}
	host, done := newPipeHost(t, context.Background(), extension)
	host.send(t, request(7, "tool.execute", map[string]any{"name": "wait", "args": map[string]any{}}))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	host.send(t, map[string]any{"jsonrpc": "2.0", "method": "$/cancelRequest", "params": map[string]any{"id": 7}})
	response := host.receive(t)
	if response.Error == nil || response.Error.Code != -32800 {
		t.Fatalf("cancel response = %+v", response)
	}
	_ = host.toPlugin.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServeReturnsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	host, done := newPipeHost(t, ctx, Extension{})
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("ServeIO error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeIO did not honor context cancellation")
	}
	_ = host.toPlugin.Close()
}
