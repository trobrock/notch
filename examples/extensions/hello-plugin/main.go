package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/trobrock/notch/plugin"
)

func main() {
	ext := plugin.Extension{
		Tools: []plugin.Tool{{
			Name:        "plugin_hello",
			Description: "Return a greeting from an executable plugin",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
				"required":   []string{"name"},
			},
			Execute: func(ctx context.Context, args json.RawMessage, progress plugin.Progress) (plugin.ToolResult, error) {
				var input struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(args, &input); err != nil {
					return plugin.ToolResult{}, err
				}
				progress("building greeting")
				client, ok := plugin.ClientFromContext(ctx)
				if !ok {
					return plugin.ToolResult{}, fmt.Errorf("plugin host is unavailable")
				}
				cwd, err := client.CWD(ctx)
				if err != nil {
					return plugin.ToolResult{}, err
				}
				return plugin.ToolResult{Content: fmt.Sprintf("Hello, %s from %s", input.Name, cwd)}, nil
			},
		}},
		Commands: []plugin.Command{{
			Name: "plugin-hello", Description: "Run the executable plugin directly",
			Execute: func(_ context.Context, args string) (string, error) { return "Hello, " + args, nil },
		}},
	}
	if err := plugin.Serve(context.Background(), ext); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
