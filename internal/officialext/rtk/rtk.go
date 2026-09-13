// Package rtk integrates the optional RTK command-output compressor with bash calls.
package rtk

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/trobrock/notch/internal/extension"
)

const Source = "official:rtk"

type commandRunner func(context.Context, string, []string) (string, string, int, error)

// Register rewrites bash commands through RTK when its executable is available.
// RTK is detected once at startup; a missing executable leaves bash unchanged.
func Register(registry *extension.Registry, host extension.Host) error {
	if registry == nil || host == nil {
		return errors.New("register rtk: registry and host are required")
	}
	path, err := exec.LookPath("rtk")
	if err != nil {
		host.Notify("RTK is not installed or not on PATH; bash output compression is disabled. See https://github.com/rtk-ai/rtk", "warning")
		return nil
	}
	registerHook(registry, path, host.Exec)
	return nil
}

func registerHook(registry *extension.Registry, path string, run commandRunner) {
	registry.On("tool_call", Source, func(ctx context.Context, event map[string]any) (map[string]any, error) {
		if event["name"] != "bash" {
			return nil, nil
		}
		arguments, ok := event["arguments"].(map[string]any)
		if !ok {
			return nil, nil
		}
		command, ok := arguments["command"].(string)
		if !ok || strings.TrimSpace(command) == "" {
			return nil, nil
		}

		stdout, _, exitCode, err := run(ctx, path, []string{"rewrite", command})
		rewritten := strings.TrimSpace(stdout)
		// RTK exits 0 for auto-allowed rewrites and 3 when its host permission
		// policy would ask. Notch owns execution policy, so both are valid here.
		if err != nil || (exitCode != 0 && exitCode != 3) || rewritten == "" || rewritten == command {
			return nil, nil
		}

		replacement := make(map[string]any, len(arguments))
		for key, value := range arguments {
			replacement[key] = value
		}
		replacement["command"] = rewritten
		return map[string]any{"arguments": replacement}, nil
	})
}
