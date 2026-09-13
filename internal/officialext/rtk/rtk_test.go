package rtk

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/trobrock/notch/internal/extension"
)

func TestHookRewritesBashAndPreservesArguments(t *testing.T) {
	registry := extension.NewRegistry()
	var gotPath string
	var gotArgs []string
	registerHook(registry, "/usr/local/bin/rtk", func(_ context.Context, path string, args []string) (string, string, int, error) {
		gotPath = path
		gotArgs = append([]string(nil), args...)
		return "rtk git status\n", "", 3, nil
	})

	result, err := registry.RunHooks(context.Background(), "tool_call", map[string]any{
		"name": "bash",
		"arguments": map[string]any{
			"command":    "git status",
			"cwd":        "/work",
			"timeout_ms": float64(1000),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/usr/local/bin/rtk" || !reflect.DeepEqual(gotArgs, []string{"rewrite", "git status"}) {
		t.Fatalf("runner call = %q %v", gotPath, gotArgs)
	}
	want := map[string]any{"command": "rtk git status", "cwd": "/work", "timeout_ms": float64(1000)}
	if !reflect.DeepEqual(result["arguments"], want) {
		t.Fatalf("arguments = %#v, want %#v", result["arguments"], want)
	}
}

func TestHookGracefullyLeavesCallsUnchanged(t *testing.T) {
	tests := []struct {
		name     string
		event    map[string]any
		stdout   string
		exitCode int
		err      error
	}{
		{name: "other tool", event: map[string]any{"name": "read", "arguments": map[string]any{"path": "README.md"}}},
		{name: "unsupported", event: map[string]any{"name": "bash", "arguments": map[string]any{"command": "custom-command"}}, exitCode: 1},
		{name: "execution error", event: map[string]any{"name": "bash", "arguments": map[string]any{"command": "git status"}}, err: errors.New("rtk failed")},
		{name: "empty rewrite", event: map[string]any{"name": "bash", "arguments": map[string]any{"command": "git status"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := extension.NewRegistry()
			registerHook(registry, "rtk", func(context.Context, string, []string) (string, string, int, error) {
				return test.stdout, "ignored", test.exitCode, test.err
			})
			before := cloneMap(test.event)
			result, err := registry.RunHooks(context.Background(), "tool_call", test.event)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result, before) {
				t.Fatalf("result = %#v, want unchanged %#v", result, before)
			}
		})
	}
}

func TestRegisterWithoutRTKWarnsAndOtherwiseNoops(t *testing.T) {
	t.Setenv("PATH", "")
	registry := extension.NewRegistry()
	host := &unusedHost{}
	if err := Register(registry, host); err != nil {
		t.Fatal(err)
	}
	if len(host.notifications) != 1 || host.notifications[0] != "warning:RTK is not installed or not on PATH; bash output compression is disabled. See https://github.com/rtk-ai/rtk" {
		t.Fatalf("notifications = %#v", host.notifications)
	}
	event := map[string]any{"name": "bash", "arguments": map[string]any{"command": "git status"}}
	result, err := registry.RunHooks(context.Background(), "tool_call", event)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, event) {
		t.Fatalf("result = %#v, want %#v", result, event)
	}
}

func cloneMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		if nested, ok := item.(map[string]any); ok {
			out[key] = cloneMap(nested)
		} else {
			out[key] = item
		}
	}
	return out
}

type unusedHost struct {
	extension.Host
	notifications []string
}

func (h *unusedHost) Notify(message, level string) {
	h.notifications = append(h.notifications, level+":"+message)
}

func (*unusedHost) Exec(context.Context, string, []string) (string, string, int, error) {
	return "", "", 0, errors.New("unexpected execution")
}
