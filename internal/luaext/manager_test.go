package luaext

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/extension"
)

type testHost struct {
	mu            sync.Mutex
	notifications []string
	statuses      [][2]string
	panels        []struct {
		key, title string
		lines      []string
	}
}

func (h *testHost) CWD() string { return "/work" }
func (h *testHost) Exec(ctx context.Context, command string, args []string) (string, string, int, error) {
	if command != "echo" || !reflect.DeepEqual(args, []string{"hello"}) {
		return "", "", 0, errors.New("unexpected command")
	}
	return "hello\n", "", 0, nil
}
func (h *testHost) Input(context.Context, string, string) (string, error)    { return "typed", nil }
func (h *testHost) Select(context.Context, string, []string) (string, error) { return "b", nil }
func (h *testHost) Notify(message, level string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifications = append(h.notifications, level+":"+message)
}
func (h *testHost) FollowUp(string) error { return nil }
func (h *testHost) SetStatus(key, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statuses = append(h.statuses, [2]string{key, value})
}

func (h *testHost) SetPanel(key, title string, lines []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.panels = append(h.panels, struct {
		key, title string
		lines      []string
	}{key, title, append([]string(nil), lines...)})
}

func TestManagerLoadsAndBridgesExtension(t *testing.T) {
	dir := t.TempDir()
	script := `
notch.register_tool({
  name = "sample",
  description = "a sample tool",
  input_schema = {
    type = "object",
    properties = {name = {type = "string"}},
    required = {"name"},
  },
  execute = function(args, update)
    update("working")
    local run = notch.exec("echo", {"hello"})
    local selected = notch.ui.select("choose", {"a", "b"})
    notch.ui.notify("done", "success")
    notch.ui.set_status("sample", "active")
    notch.ui.set_panel("sample", "Sample", {"one", "two"})
    return {content = notch.cwd() .. ":" .. args.name .. ":" .. run.stdout .. selected,
            details = {input = notch.ui.input("prompt", "placeholder")}}
  end,
})
notch.register_command({name = "greet", description = "greets", execute = function(args)
  return "hello " .. args
end})
notch.on("before", function(event)
  return {seen = event.value .. "!"}
end)
`
	if err := os.WriteFile(filepath.Join(dir, "extension.lua"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	registry := extension.NewRegistry()
	host := &testHost{}
	manager := NewManager(registry, host)
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.LoadDirs(dir); err != nil {
		t.Fatalf("LoadDirs: %v", err)
	}

	tool, ok := registry.Tool("sample")
	if !ok {
		t.Fatal("sample tool was not registered")
	}
	if got := tool.Definition.InputSchema["type"]; got != "object" {
		t.Fatalf("schema type = %#v", got)
	}
	if got := tool.Definition.InputSchema["required"]; !reflect.DeepEqual(got, []any{"name"}) {
		t.Fatalf("schema required = %#v", got)
	}
	var updates []string
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"Lua"}`), func(s string) {
		updates = append(updates, s)
	})
	if err != nil {
		t.Fatalf("execute tool: %v", err)
	}
	if result.Content != "/work:Lua:hello\nb" || result.Details["input"] != "typed" {
		t.Fatalf("tool result = %#v", result)
	}
	if !reflect.DeepEqual(updates, []string{"working"}) {
		t.Fatalf("updates = %#v", updates)
	}

	command, ok := registry.Command("greet")
	if !ok {
		t.Fatal("greet command was not registered")
	}
	if got, err := command.Execute(context.Background(), "world"); err != nil || got != "hello world" {
		t.Fatalf("command result = %q, %v", got, err)
	}
	event, err := registry.RunHooks(context.Background(), "before", map[string]any{"value": "yes"})
	if err != nil || event["seen"] != "yes!" {
		t.Fatalf("hook result = %#v, %v", event, err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if !reflect.DeepEqual(host.notifications, []string{"success:done"}) {
		t.Fatalf("notifications = %#v", host.notifications)
	}
	if !reflect.DeepEqual(host.statuses, [][2]string{{"sample", "active"}}) {
		t.Fatalf("statuses = %#v", host.statuses)
	}
	if len(host.panels) != 1 || host.panels[0].key != "sample" || host.panels[0].title != "Sample" || !reflect.DeepEqual(host.panels[0].lines, []string{"one", "two"}) {
		t.Fatalf("panels = %#v", host.panels)
	}
}

func TestManagerLoadsFilesInNameOrder(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"z.lua":       `notch.on("order", function(e) return {order = e.order .. "z"} end)`,
		"a.lua":       `notch.on("order", function(e) return {order = e.order .. "a"} end)`,
		"ignored.txt": `error("must not load")`,
	}
	for name, source := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry := extension.NewRegistry()
	manager := NewManager(registry, &testHost{})
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.LoadDirs(dir); err != nil {
		t.Fatal(err)
	}
	result, err := registry.RunHooks(context.Background(), "order", map[string]any{"order": ""})
	if err != nil {
		t.Fatal(err)
	}
	if result["order"] != "az" {
		t.Fatalf("order = %q", result["order"])
	}
}

func TestLuaCallHonorsCancellationAndClose(t *testing.T) {
	dir := t.TempDir()
	source := `notch.register_command({name="spin", execute=function() while true do end end})`
	if err := os.WriteFile(filepath.Join(dir, "spin.lua"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := extension.NewRegistry()
	manager := NewManager(registry, &testHost{})
	if err := manager.LoadDirs(dir); err != nil {
		t.Fatal(err)
	}
	command, _ := registry.Command("spin")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := command.Execute(ctx, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = command.Execute(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("post-close error = %v", err)
	}
}
