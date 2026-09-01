package plan

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/extension"
)

type testHost struct {
	selected string
	prompts  []string
	active   [][]string
	statuses [][2]string
	handoffs []handoff
}
type handoff struct {
	message string
	fresh   bool
}

func (*testHost) CWD() string { return "/work" }
func (*testHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, errors.New("unused")
}
func (*testHost) Input(context.Context, string, string) (string, error) { return "", nil }
func (h *testHost) Select(_ context.Context, prompt string, _ []string) (string, error) {
	h.prompts = append(h.prompts, prompt)
	return h.selected, nil
}
func (*testHost) Notify(string, string) {}
func (*testHost) FollowUp(string) error { return nil }
func (h *testHost) Handoff(m string, f bool) error {
	h.handoffs = append(h.handoffs, handoff{m, f})
	return nil
}
func (h *testHost) SetActiveTools(n []string) error {
	h.active = append(h.active, append([]string(nil), n...))
	return nil
}
func (*testHost) SwitchModel(context.Context, string, string) (string, int, error) { return "", 0, nil }
func (*testHost) ListModels(context.Context, string, bool) ([]extension.ModelInfo, error) {
	return nil, nil
}
func (*testHost) AppendSessionEntry(string, any) error             { return nil }
func (*testHost) SessionEntries(string) ([]json.RawMessage, error) { return nil, nil }
func (*testHost) EditorText(context.Context) (string, error)       { return "", nil }
func (*testHost) SetEditorText(context.Context, string) error      { return nil }
func (h *testHost) SetStatus(k, v string)                          { h.statuses = append(h.statuses, [2]string{k, v}) }
func (*testHost) SetPanel(string, string, []string)                {}

func setup(t *testing.T) (*extension.Registry, *testHost, *state) {
	t.Helper()
	r, h := extension.NewRegistry(), &testHost{}
	if err := Register(r, h); err != nil {
		t.Fatal(err)
	}
	command, _ := r.Command("plan")
	if _, err := command.Execute(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	return r, h, &state{host: h, enabled: true}
}
func TestPlanCommandEnablesReadOnlyTools(t *testing.T) {
	r, h, _ := setup(t)
	if !reflect.DeepEqual(h.active[0], readOnly) {
		t.Fatalf("active=%#v", h.active)
	}
	command, _ := r.Command("plan")
	text, err := command.Execute(context.Background(), "status")
	if err != nil || text != "Plan mode is active." {
		t.Fatalf("status=%q %v", text, err)
	}
}
func TestPlanHooksInjectPromptAndDenyWrites(t *testing.T) {
	r, _, _ := setup(t)
	event, err := r.RunHooks(context.Background(), "before_agent_start", map[string]any{"system_prompt": "base"})
	if err != nil || !strings.Contains(event["system_prompt"].(string), "PLAN MODE IS ACTIVE") || !strings.Contains(event["system_prompt"].(string), "use explore_codebase proactively") {
		t.Fatalf("event=%#v err=%v", event, err)
	}
	denied, err := r.RunHooks(context.Background(), "tool_call", map[string]any{"name": "write"})
	if err != nil || denied["denied"] != true {
		t.Fatalf("denied=%#v", denied)
	}
	denied, err = r.RunHooks(context.Background(), "tool_call", map[string]any{"name": "run_subagent", "arguments": map[string]any{"tools": "read,write", "allowWriteTools": true}})
	if err != nil || denied["denied"] != true {
		t.Fatalf("write subagent denied=%#v", denied)
	}
}
func TestPlanApprovalFreshContextDefault(t *testing.T) {
	r, h, _ := setup(t)
	h.selected = "Implement in fresh context (recommended)"
	tool, _ := r.Tool("exit_plan_mode")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"plan":"1. Edit x\n2. Test"}`), nil)
	if err != nil || result.Details["fresh"] != true {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(h.prompts) != 1 || !strings.Contains(h.prompts[0], "1. Edit x\n2. Test") || !strings.HasSuffix(h.prompts[0], "How should implementation start?") {
		t.Fatalf("approval prompt=%#v", h.prompts)
	}
	if _, err := r.RunHooks(context.Background(), "agent_end", nil); err != nil {
		t.Fatal(err)
	}
	if len(h.handoffs) != 1 || !h.handoffs[0].fresh || !strings.Contains(h.handoffs[0].message, "Edit x") {
		t.Fatalf("handoffs=%#v", h.handoffs)
	}
	if h.active[len(h.active)-1] != nil {
		t.Fatalf("tools not restored: %#v", h.active)
	}
}
func TestPlanApprovalCanKeepContextOrStay(t *testing.T) {
	r, h, _ := setup(t)
	tool, _ := r.Tool("exit_plan_mode")
	h.selected = "Stay in plan mode"
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"plan":"plan"}`), nil)
	if err != nil || !strings.Contains(result.Content, "stay") {
		t.Fatalf("result=%#v", result)
	}
	if _, err := r.RunHooks(context.Background(), "agent_end", nil); err != nil || len(h.handoffs) != 0 {
		t.Fatalf("handoffs=%#v err=%v", h.handoffs, err)
	}
	h.selected = "Implement with current context"
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"plan":"plan"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.RunHooks(context.Background(), "agent_end", nil)
	if err != nil || len(h.handoffs) != 1 || h.handoffs[0].fresh {
		t.Fatalf("handoffs=%#v err=%v", h.handoffs, err)
	}
}
