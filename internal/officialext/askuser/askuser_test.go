package askuser

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/trobrock/notch/internal/extension"
)

type testHost struct {
	input       string
	selected    string
	inputErr    error
	selectErr   error
	prompts     []string
	placeholder string
	options     []string
}

func (h *testHost) CWD() string { return "/work" }
func (h *testHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, errors.New("unexpected exec")
}
func (h *testHost) Input(_ context.Context, prompt, placeholder string) (string, error) {
	h.prompts = append(h.prompts, prompt)
	h.placeholder = placeholder
	return h.input, h.inputErr
}
func (h *testHost) Select(_ context.Context, prompt string, options []string) (string, error) {
	h.prompts = append(h.prompts, prompt)
	h.options = append([]string(nil), options...)
	return h.selected, h.selectErr
}
func (*testHost) Notify(string, string)           {}
func (*testHost) FollowUp(string) error           { return nil }
func (h *testHost) Handoff(string, bool) error    { return nil }
func (h *testHost) SetActiveTools([]string) error { return nil }
func (h *testHost) SwitchModel(context.Context, string, string) (string, int, error) {
	return "", 0, nil
}
func (*testHost) SetStatus(string, string)          {}
func (*testHost) SetPanel(string, string, []string) {}

func registeredTool(t *testing.T, host extension.Host) extension.Tool {
	t.Helper()
	registry := extension.NewRegistry()
	if err := Register(registry, host); err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Tool("ask_user_question")
	if !ok {
		t.Fatal("ask_user_question was not registered")
	}
	return tool
}

func TestAskUserQuestionSelectsSuggestedAnswer(t *testing.T) {
	host := &testHost{selected: "2. PostgreSQL — Better concurrency"}
	tool := registeredTool(t, host)
	var updates []string
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"question":"Which database?",
		"options":[{"label":"SQLite"},{"label":"PostgreSQL","description":"Better concurrency"}]
	}`), func(update string) { updates = append(updates, update) })
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "User answered: PostgreSQL" || result.Details["answer"] != "PostgreSQL" || result.Details["wasCustom"] != false {
		t.Fatalf("result = %#v", result)
	}
	wantOptions := []string{"1. SQLite", "2. PostgreSQL — Better concurrency"}
	if !reflect.DeepEqual(host.options, wantOptions) {
		t.Fatalf("options = %#v, want %#v", host.options, wantOptions)
	}
	if !reflect.DeepEqual(updates, []string{"waiting for user response"}) {
		t.Fatalf("updates = %#v", updates)
	}
}

func TestAskUserQuestionAcceptsCustomAnswer(t *testing.T) {
	host := &testHost{selected: "Type a custom response", input: "CockroachDB"}
	tool := registeredTool(t, host)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"question":"Which database?", "options":[{"label":"SQLite"}],
		"allowCustomResponse":true, "placeholder":"Database name"
	}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "User answered: CockroachDB" || result.Details["wasCustom"] != true {
		t.Fatalf("result = %#v", result)
	}
	if host.placeholder != "Database name" || !reflect.DeepEqual(host.prompts, []string{"Which database?", "Which database?"}) {
		t.Fatalf("host calls = prompts %#v, placeholder %q", host.prompts, host.placeholder)
	}
}

func TestAskUserQuestionDefaultsToFreeFormWithoutOptions(t *testing.T) {
	host := &testHost{input: "Use the existing API"}
	tool := registeredTool(t, host)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"question":"How should this work?"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Details["answer"] != "Use the existing API" || result.Details["wasCustom"] != true || len(host.options) != 0 {
		t.Fatalf("result = %#v; options = %#v", result, host.options)
	}
}

func TestAskUserQuestionValidatesArgumentsAndHandlesCancellation(t *testing.T) {
	tool := registeredTool(t, &testHost{})
	for _, raw := range []string{
		`{}`,
		`{"question":" "}`,
		`{"question":"Question?","options":[{"label":""}]}`,
		`{"question":"Question?","unknown":true}`,
	} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Fatalf("Execute(%s) unexpectedly succeeded", raw)
		}
	}

	tool = registeredTool(t, &testHost{inputErr: context.Canceled})
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"question":"Question?"}`), nil)
	if err != nil || result.Content != "User cancelled the question." || result.Details["cancelled"] != true || result.Details["answer"] != nil {
		t.Fatalf("canceled result = %#v, %v", result, err)
	}
}

func TestOfficialToolSchemaIsStrict(t *testing.T) {
	tool := registeredTool(t, &testHost{})
	if tool.Source != source || tool.Definition.InputSchema["additionalProperties"] != false {
		t.Fatalf("tool = %#v", tool)
	}
	properties := tool.Definition.InputSchema["properties"].(map[string]any)
	options := properties["options"].(map[string]any)
	items := options["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Fatalf("option schema = %#v", items)
	}
}
