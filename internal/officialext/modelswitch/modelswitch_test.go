package modelswitch

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/trobrock/notch/internal/extension"
)

type host struct {
	provider, model string
	window          int
	err             error
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
func (*host) SetStatus(string, string)          {}
func (*host) SetPanel(string, string, []string) {}
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
