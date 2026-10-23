package officialext

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/config"
	"github.com/trobrock/notch/internal/extension"
)

type testHost struct{}

func (*testHost) CWD() string { return "/work" }
func (*testHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, errors.New("unused")
}
func (*testHost) Input(context.Context, string, string) (string, error)    { return "", nil }
func (*testHost) Select(context.Context, string, []string) (string, error) { return "", nil }
func (*testHost) Notify(string, string)                                    {}
func (*testHost) FollowUp(string) error                                    { return nil }
func (h *testHost) Handoff(string, bool) error                             { return nil }
func (h *testHost) SetActiveTools([]string) error                          { return nil }
func (h *testHost) SwitchModel(context.Context, string, string) (string, int, error) {
	return "", 0, nil
}
func (h *testHost) ListModels(context.Context, string, bool) ([]extension.ModelInfo, error) {
	return nil, nil
}
func (*testHost) SessionID() string                                { return "" }
func (*testHost) AppendSessionEntry(string, string, any) error     { return nil }
func (*testHost) SessionEntries(string) ([]json.RawMessage, error) { return nil, nil }
func (*testHost) EditorText(context.Context) (string, error)       { return "", nil }
func (*testHost) SetEditorText(context.Context, string) error      { return nil }
func (*testHost) SetStatus(string, string)                         {}
func (*testHost) SetPanel(string, string, []string)                {}

func TestRegisterKeepsOfficialExtensionsSegmented(t *testing.T) {
	registry := extension.NewRegistry()
	if err := Register(registry, &testHost{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ask_user_question", "complete_goal", "exit_plan_mode", "explore_codebase", "intercom_list", "intercom_send", "intercom_reply", "intercom_inbox", "intercom_request_status", "intercom_retry", "list_models", "list_monitors", "monitor_command", "monitor_github_pr_checks", "report_goal_progress", "run_subagent", "stop_monitor", "switch_model", "update_task_list"} {
		tool, ok := registry.Tool(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if tool.Source == "" {
			t.Fatalf("%s source is empty", name)
		}
	}
}

func TestCustomRoleWhenToUseAppearsInDelegationSchemas(t *testing.T) {
	registry := extension.NewRegistry()
	cfg := config.Config{Roles: map[string]config.RoleConfig{
		"auditor": {Instructions: "Check policy.", WhenToUse: "Use for policy and compliance checks."},
	}}
	if err := RegisterWithRoleConfig(registry, &testHost{}, "user,project", "", "", "", nil, cfg, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"run_subagent", "explore_codebase", "coordinate_helpers"} {
		tool, ok := registry.Tool(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		encoded, err := json.Marshal(tool.Definition.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		description := string(encoded)
		if !strings.Contains(description, "auditor") || !strings.Contains(description, "Use for policy and compliance checks.") {
			t.Fatalf("%s schema does not describe the custom role: %s", name, description)
		}
	}
}
