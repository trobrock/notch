package modelswitch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/extension"
)

type host struct {
	provider, model string
	window          int
	err             error
	models          []extension.ModelInfo
	listProvider    string
	refresh         bool
}

func (*host) CWD() string { return "/work" }
func (*host) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, errors.New("unused")
}
func (*host) Input(context.Context, string, string) (string, error)    { return "", nil }
func (*host) Select(context.Context, string, []string) (string, error) { return "", nil }
func (*host) Notify(string, string)                                    {}
func (*host) FollowUp(string) error                                    { return nil }
func (*host) Handoff(string, bool) error                               { return nil }
func (*host) SetActiveTools([]string) error                            { return nil }
func (h *host) SwitchModel(_ context.Context, p, m string) (string, int, error) {
	h.provider, h.model = p, m
	if h.err != nil {
		return "", 0, h.err
	}
	if p == "" {
		p = "anthropic"
	}
	return p, h.window, nil
}
func (h *host) ListModels(_ context.Context, p string, r bool) ([]extension.ModelInfo, error) {
	h.listProvider, h.refresh = p, r
	return h.models, h.err
}
func (*host) AppendSessionEntry(string, any) error             { return nil }
func (*host) SessionEntries(string) ([]json.RawMessage, error) { return nil, nil }
func (*host) EditorText(context.Context) (string, error)       { return "", nil }
func (*host) SetEditorText(context.Context, string) error      { return nil }
func (*host) SetStatus(string, string)                         {}
func (*host) SetPanel(string, string, []string)                {}
func TestSwitchModel(t *testing.T) {
	r, h := extension.NewRegistry(), &host{window: 200000}
	if err := Register(r, h); err != nil {
		t.Fatal(err)
	}
	tool, _ := r.Tool("switch_model")
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"provider":"anthropic","model":"claude-test"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "Switched subsequent turns to anthropic/claude-test." || result.Details["contextWindow"] != 200000 {
		t.Fatalf("result=%#v", result)
	}
	if h.provider != "anthropic" || h.model != "claude-test" {
		t.Fatalf("host=%#v", h)
	}
}
func TestListModels(t *testing.T) {
	r := extension.NewRegistry()
	h := &host{models: []extension.ModelInfo{{Provider: "anthropic", ID: "claude-test", Name: "Claude Test", ContextWindow: 200000, Reasoning: true}}}
	if err := Register(r, h); err != nil {
		t.Fatal(err)
	}
	tool, _ := r.Tool("list_models")
	if !strings.Contains(tool.Definition.Description, "run_subagent") || !strings.Contains(tool.Definition.Description, "explore_codebase") || !strings.Contains(tool.Definition.Description, "rather than guessing") {
		t.Fatalf("description = %q", tool.Definition.Description)
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"provider":"anthropic","refresh":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "anthropic/claude-test") || !strings.Contains(result.Content, "200000 context") || h.listProvider != "anthropic" || !h.refresh {
		t.Fatalf("result=%#v host=%#v", result, h)
	}
}
func TestSwitchModelValidates(t *testing.T) {
	r, h := extension.NewRegistry(), &host{}
	if err := Register(r, h); err != nil {
		t.Fatal(err)
	}
	tool, _ := r.Tool("switch_model")
	for _, raw := range []string{`{}`, `{"model":" "}`, `{"model":"x","unknown":true}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Fatalf("Execute(%s) succeeded", raw)
		}
	}
}
