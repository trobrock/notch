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
	editorText    string
	entries       []json.RawMessage
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
func (h *testHost) FollowUp(string) error         { return nil }
func (h *testHost) Handoff(string, bool) error    { return nil }
func (h *testHost) SetActiveTools([]string) error { return nil }
func (h *testHost) SwitchModel(context.Context, string, string) (string, int, error) {
	return "", 0, nil
}
func (h *testHost) ListModels(context.Context, string, bool) ([]extension.ModelInfo, error) {
	return nil, nil
}
func (h *testHost) AppendSessionEntry(_ string, data any) error {
	raw, _ := json.Marshal(data)
	h.entries = append(h.entries, raw)
	return nil
}
func (h *testHost) SessionEntries(string) ([]json.RawMessage, error) {
	return append([]json.RawMessage(nil), h.entries...), nil
}
func (h *testHost) EditorText(context.Context) (string, error) { return h.editorText, nil }
func (h *testHost) SetEditorText(_ context.Context, value string) error {
	h.editorText = value
	return nil
}
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
    notch.session.append("sample", {action = "add", value = args.name})
    local saved = notch.session.entries("sample")
    local draft = notch.ui.editor_text()
    notch.ui.set_editor_text(draft .. args.name)
    return {content = notch.cwd() .. ":" .. args.name .. ":" .. run.stdout .. selected .. ":" .. saved[1].value,
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
	if result.Content != "/work:Lua:hello\nb:Lua" || result.Details["input"] != "typed" {
		t.Fatalf("tool result = %#v", result)
	}
	if host.editorText != "Lua" || len(host.entries) != 1 {
		t.Fatalf("host editor = %q entries = %q", host.editorText, host.entries)
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

func TestLoadDirsRollsBackFilesLoadedByFailedCall(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.lua": `notch.register_tool({name="temporary", execute=function() return "ok" end})`,
		"b.lua": `notch.register_command({name="taken", execute=function() return "bad" end})`,
	}
	for name, source := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry := extension.NewRegistry()
	if err := registry.RegisterCommand(extension.Command{Name: "taken", Source: "existing", Execute: func(context.Context, string) (string, error) { return "", nil }}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(registry, &testHost{})
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.LoadDirs(dir); err == nil {
		t.Fatal("LoadDirs succeeded despite duplicate command")
	}
	if _, ok := registry.Tool("temporary"); ok {
		t.Fatal("failed LoadDirs left prior file's tool registered")
	}
	command, ok := registry.Command("taken")
	if !ok || command.Source != "existing" {
		t.Fatalf("existing command changed: %#v, %v", command, ok)
	}
}

func TestLuaCallHonorsCancellationAndClose(t *testing.T) {
	dir := t.TempDir()
	source := `
notch.register_command({name="spin", execute=function() while true do end end})
notch.register_tool({name="close_tool", execute=function() return "ok" end})
notch.on("close_hook", function() return {called=true} end)
`
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
	if _, ok := registry.Command("spin"); ok {
		t.Fatal("Lua command remained registered after manager Close")
	}
	if _, ok := registry.Tool("close_tool"); ok {
		t.Fatal("Lua tool remained registered after manager Close")
	}
	event, hookErr := registry.RunHooks(context.Background(), "close_hook", nil)
	if hookErr != nil || event["called"] != nil {
		t.Fatalf("Lua hook remained registered after manager Close: event=%v err=%v", event, hookErr)
	}
	_, err = command.Execute(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("previously captured post-close command error = %v", err)
	}
}

func TestLoadDirsIgnoresMissingDirectories(t *testing.T) {
	manager := New(extension.NewRegistry(), &testHost{})
	defer manager.Close()
	if err := manager.LoadDirs(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("LoadDirs missing directory: %v", err)
	}
}
