package tasklist

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/trobrock/notch/internal/extension"
)

type testHost struct {
	statuses [][2]string
	panels   []panelCall
}
type panelCall struct {
	key, title string
	lines      []string
}

func (*testHost) CWD() string { return "/work" }
func (*testHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, errors.New("unexpected exec")
}
func (*testHost) Input(context.Context, string, string) (string, error) {
	return "", errors.New("unexpected input")
}
func (*testHost) Select(context.Context, string, []string) (string, error) {
	return "", errors.New("unexpected select")
}
func (*testHost) Notify(string, string) {}
func (*testHost) FollowUp(string) error { return nil }
func (h *testHost) SetStatus(key, value string) {
	h.statuses = append(h.statuses, [2]string{key, value})
}
func (h *testHost) SetPanel(key, title string, lines []string) {
	h.panels = append(h.panels, panelCall{key: key, title: title, lines: append([]string(nil), lines...)})
}

func setup(t *testing.T) (*extension.Registry, *testHost, extension.Tool) {
	t.Helper()
	registry, host := extension.NewRegistry(), &testHost{}
	if err := Register(registry, host); err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Tool(ToolName)
	if !ok {
		t.Fatal("tool not registered")
	}
	return registry, host, tool
}

func TestUpdateTaskListNormalizesAndPublishesStatus(t *testing.T) {
	_, host, tool := setup(t)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"todos":[
		{"content":"Implement API","status":"in_progress","priority":"high"},
		{"id":"tests","content":"Add tests","status":"pending"},
		{"id":"tests","content":"Run tests","status":"completed"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content != "- [in_progress] implement-api: high Implement API\n- [pending] tests: Add tests\n- [completed] tests-2: Run tests" {
		t.Fatalf("result = %#v", result)
	}
	tasks := result.Details["todos"].([]Task)
	if tasks[0].ID != "implement-api" || tasks[2].ID != "tests-2" {
		t.Fatalf("tasks = %#v", tasks)
	}
	if got := result.Details["summary"].(Summary); got != (Summary{Total: 3, Pending: 1, InProgress: 1, Completed: 1}) {
		t.Fatalf("summary = %#v", got)
	}
	if !reflect.DeepEqual(host.statuses, [][2]string{{"tasks", "tasks 1/3"}}) {
		t.Fatalf("statuses = %#v", host.statuses)
	}
	if len(host.panels) != 1 || host.panels[0].title != "Tasks 1/3" || !reflect.DeepEqual(host.panels[0].lines, []string{"● ! Implement API", "○ Add tests", "✓ Run tests"}) {
		t.Fatalf("panels = %#v", host.panels)
	}
}

func TestUpdateTaskListClearsStatusAndShutdownCleansUp(t *testing.T) {
	registry, host, tool := setup(t)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"todos":[{"content":"Done","status":"completed"}]}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RunHooks(context.Background(), "session_shutdown", nil); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"tasks", ""}, {"tasks", ""}}
	if !reflect.DeepEqual(host.statuses, want) {
		t.Fatalf("statuses = %#v", host.statuses)
	}
	if len(host.panels) != 2 || host.panels[0].title != "" || host.panels[1].title != "" {
		t.Fatalf("panels = %#v", host.panels)
	}
}

func TestUpdateTaskListValidatesInput(t *testing.T) {
	_, _, tool := setup(t)
	for _, raw := range []string{
		`{}`, `{"todos":null}`, `{"todos":[{"content":"","status":"pending"}]}`,
		`{"todos":[{"content":"x","status":"unknown"}]}`,
		`{"todos":[{"content":"x","status":"pending","priority":"urgent"}]}`,
		`{"todos":[],"unknown":true}`,
	} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Fatalf("Execute(%s) succeeded", raw)
		}
	}
}

func TestTaskListSchemaIsStrict(t *testing.T) {
	_, _, tool := setup(t)
	if tool.Source != Source || tool.Definition.InputSchema["additionalProperties"] != false {
		t.Fatalf("tool = %#v", tool)
	}
	properties := tool.Definition.InputSchema["properties"].(map[string]any)
	items := properties["todos"].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Fatalf("items = %#v", items)
	}
}
