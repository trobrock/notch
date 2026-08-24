package extension

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type testHost struct {
	mu            sync.Mutex
	notifications []string
	statuses      [][2]string
}

func (h *testHost) CWD() string { return "/host/work" }
func (h *testHost) Exec(_ context.Context, command string, args []string) (string, string, int, error) {
	return command + " " + strings.Join(args, " "), "", 0, nil
}
func (h *testHost) Input(_ context.Context, prompt, placeholder string) (string, error) {
	return prompt + ":" + placeholder, nil
}
func (h *testHost) Select(_ context.Context, _ string, options []string) (string, error) {
	return options[len(options)-1], nil
}
func (h *testHost) Notify(message, level string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifications = append(h.notifications, level+":"+message)
}

func (h *testHost) SetStatus(key, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statuses = append(h.statuses, [2]string{key, value})
}

func (h *testHost) SetPanel(string, string, []string) {}

func writeTestManifest(t *testing.T, root, name string, enabled bool) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Name: name, Command: []string{executable, "-test.run=^TestPluginHelper$"}, Enabled: enabled}
	data, _ := json.Marshal(manifest)
	path := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverRegisterAndExecutePlugin(t *testing.T) {
	t.Setenv("NOTCH_PLUGIN_HELPER", "1")
	root := t.TempDir()
	writeTestManifest(t, root, "working", true)
	writeTestManifest(t, root, "disabled", false)
	badDir := filepath.Join(root, "bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "plugin.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := NewRegistry()
	host := &testHost{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	plugins, warnings := DiscoverAndLoad(ctx, []string{root, root}, registry, host)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one malformed manifest warning", warnings)
	}
	if len(plugins) != 1 || plugins[0].Name != "working" {
		t.Fatalf("plugins = %#v", plugins)
	}
	t.Cleanup(func() { _ = plugins[0].Close() })

	tool, ok := registry.Tool("echo")
	if !ok || tool.Source != "working" {
		t.Fatalf("registered tool = %#v, %v", tool, ok)
	}
	var updates []string
	result, err := tool.Execute(ctx, json.RawMessage(`{"text":"hello"}`), func(update string) { updates = append(updates, update) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "/host/work:hello" || len(updates) != 1 || updates[0] != "starting" {
		t.Fatalf("tool result = %#v, updates = %v", result, updates)
	}

	command, ok := registry.Command("say")
	if !ok {
		t.Fatal("say command was not registered")
	}
	large := strings.Repeat("x", 100_000)
	output, err := command.Execute(ctx, large)
	if err != nil || output != large {
		t.Fatalf("large command result len=%d err=%v", len(output), err)
	}

	event, err := registry.RunHooks(ctx, "before", map[string]any{"old": true})
	if err != nil || event["plugin"] != "working" || event["old"] != true {
		t.Fatalf("hook result = %#v, err = %v", event, err)
	}

	// Responses may arrive while many IDs are outstanding.
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("concurrent-%d", i)
			got, err := command.Execute(ctx, want)
			if err != nil || got != want {
				errs <- fmt.Errorf("got %q: %w", got, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if _, err := command.Execute(ctx, "protocol-error"); err == nil || !strings.Contains(err.Error(), "protocol error") {
		t.Fatalf("malformed stdout error = %v", err)
	}
}

func TestDispatchHostMethods(t *testing.T) {
	host := &testHost{}
	plugin := &Plugin{host: host, ctx: context.Background()}
	cases := []struct {
		method string
		params string
	}{
		{"host.cwd", `{}`},
		{"host.exec", `{"command":"echo","args":["hello"]}`},
		{"host.ui.input", `{"prompt":"name","placeholder":"here"}`},
		{"host.ui.select", `{"prompt":"pick","options":["a","b"]}`},
		{"host.ui.notify", `{"message":"done","level":"info"}`},
		{"host.ui.set_status", `{"key":"tasks","value":"tasks 1/3"}`},
		{"host.ui.set_panel", `{"key":"tasks","title":"Tasks","lines":["one"]}`},
	}
	for _, test := range cases {
		if _, rpcErr := plugin.dispatchHost(test.method, json.RawMessage(test.params)); rpcErr != nil {
			t.Errorf("%s: %v", test.method, rpcErr)
		}
	}
	if _, rpcErr := plugin.dispatchHost("host.nope", nil); rpcErr == nil || rpcErr.Code != -32601 {
		t.Fatalf("unknown host method error = %#v", rpcErr)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.notifications) != 1 || host.notifications[0] != "info:done" {
		t.Fatalf("notifications = %v", host.notifications)
	}
	if !reflect.DeepEqual(host.statuses, [][2]string{{"tasks", "tasks 1/3"}}) {
		t.Fatalf("statuses = %v", host.statuses)
	}
}

func TestPluginCallCancellation(t *testing.T) {
	t.Setenv("NOTCH_PLUGIN_HELPER", "1")
	root := t.TempDir()
	writeTestManifest(t, root, "cancel", true)
	registry := NewRegistry()
	plugins, warnings := DiscoverAndLoad(context.Background(), []string{root}, registry, &testHost{})
	if len(warnings) != 0 || len(plugins) != 1 {
		t.Fatalf("plugins=%d warnings=%v", len(plugins), warnings)
	}
	t.Cleanup(func() { _ = plugins[0].Close() })

	tool, _ := registry.Tool("echo")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := tool.Execute(ctx, json.RawMessage(`{"text":"cancel"}`), nil)
	if err != context.DeadlineExceeded {
		t.Fatalf("cancelled call error = %v", err)
	}

	command, _ := registry.Command("say")
	deadline := time.Now().Add(time.Second)
	for {
		result, err := command.Execute(context.Background(), "cancelled?")
		if err == nil && result == "yes" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin did not receive cancellation: result=%q err=%v", result, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPluginHelper is run in a subprocess by the integration tests. Keeping the
// fake plugin in this test binary avoids non-standard dependencies or scripts.
func TestPluginHelper(t *testing.T) {
	if os.Getenv("NOTCH_PLUGIN_HELPER") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), maxPluginMessage)
	writer := bufio.NewWriter(os.Stdout)
	write := func(value any) {
		data, err := json.Marshal(value)
		if err != nil {
			os.Exit(2)
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			os.Exit(2)
		}
		if err := writer.Flush(); err != nil {
			os.Exit(2)
		}
	}

	cancelled := false
	for scanner.Scan() {
		var request rpcMessage
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		switch request.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{
				"tools":    []any{map[string]any{"name": "echo", "description": "echo text", "input_schema": map[string]any{"type": "object"}}},
				"hooks":    []any{"before"},
				"commands": []any{map[string]any{"name": "say", "description": "say text"}},
			}})
		case "tool.execute":
			var params struct {
				Args struct {
					Text string `json:"text"`
				} `json:"args"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if params.Args.Text == "cancel" {
				continue
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "tool.update", "params": map[string]any{"id": request.ID, "message": "starting"}})
			hostID := json.RawMessage(fmt.Sprintf("%q", "host-"+string(request.ID)))
			write(map[string]any{"jsonrpc": "2.0", "id": hostID, "method": "host.cwd", "params": map[string]any{}})
			if !scanner.Scan() {
				return
			}
			var hostResponse rpcMessage
			if json.Unmarshal(scanner.Bytes(), &hostResponse) != nil || string(hostResponse.ID) != string(hostID) {
				os.Exit(2)
			}
			var cwd string
			_ = json.Unmarshal(hostResponse.Result, &cwd)
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": ToolResult{Content: cwd + ":" + params.Args.Text}})
		case "command.execute":
			var params struct {
				Args string `json:"args"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if params.Args == "protocol-error" {
				_, _ = writer.WriteString("this is not json\n")
				_ = writer.Flush()
				continue
			}
			if params.Args == "cancelled?" {
				if cancelled {
					params.Args = "yes"
				} else {
					params.Args = "no"
				}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": params.Args})
		case "hook.handle":
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"plugin": "working"}})
		case "$/cancelRequest":
			cancelled = true
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
		}
	}
}
