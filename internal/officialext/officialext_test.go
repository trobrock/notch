package officialext

import (
	"context"
	"errors"
	"testing"

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
func (*testHost) SetStatus(string, string)                                 {}
func (*testHost) SetPanel(string, string, []string)                        {}

func TestRegisterKeepsOfficialExtensionsSegmented(t *testing.T) {
	registry := extension.NewRegistry()
	if err := Register(registry, &testHost{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ask_user_question", "exit_plan_mode", "explore_codebase", "list_monitors", "monitor_command", "monitor_github_pr_checks", "run_subagent", "stop_monitor", "update_task_list"} {
		tool, ok := registry.Tool(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if tool.Source == "" {
			t.Fatalf("%s source is empty", name)
		}
	}
}
