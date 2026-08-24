// Package modelswitch implements Notch's official switch_model tool.
package modelswitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

const Source = "official:model-switch"

func Register(registry *extension.Registry, host extension.Host) error {
	if registry == nil || host == nil {
		return errors.New("register model switch: registry and host are required")
	}
	for _, tool := range []extension.Tool{switchTool(host), listTool(host)} {
		if err := registry.RegisterTool(tool); err != nil {
			return fmt.Errorf("register model tools: %w", err)
		}
	}
	return nil
}

func switchTool(host extension.Host) extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "switch_model", Description: "Change the model and optionally provider used by subsequent Notch turns while preserving the current conversation.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"model":    map[string]any{"type": "string", "minLength": 1, "description": "Model ID to use for subsequent turns."},
		"provider": map[string]any{"type": "string", "enum": []string{"openai-codex", "anthropic", "openrouter", "openai"}, "description": "Provider to use. Omit to keep the current provider."},
	}, "required": []string{"model"}, "additionalProperties": false}}, Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		var input struct {
			Model    string `json:"model"`
			Provider string `json:"provider,omitempty"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return extension.ToolResult{}, fmt.Errorf("decode arguments: %w", err)
		}
		input.Model = strings.TrimSpace(input.Model)
		if input.Model == "" {
			return extension.ToolResult{}, errors.New("model must not be empty")
		}
		provider, window, err := host.SwitchModel(ctx, input.Provider, input.Model)
		if err != nil {
			return extension.ToolResult{}, err
		}
		return extension.ToolResult{Content: fmt.Sprintf("Switched subsequent turns to %s/%s.", provider, input.Model), Details: map[string]any{"provider": provider, "model": input.Model, "contextWindow": window}}, nil
	}}
}

func listTool(host extension.Host) extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "list_models", Description: "List models available from a provider before choosing one with switch_model.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"provider": map[string]any{"type": "string", "enum": []string{"openai-codex", "anthropic", "openrouter", "openai"}, "description": "Provider to list. Omit for the current provider."},
		"refresh":  map[string]any{"type": "boolean", "description": "Refresh provider model discovery instead of using the cache."},
	}, "additionalProperties": false}}, Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		var input struct {
			Provider string `json:"provider,omitempty"`
			Refresh  bool   `json:"refresh,omitempty"`
		}
		if len(raw) != 0 {
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&input); err != nil {
				return extension.ToolResult{}, fmt.Errorf("decode arguments: %w", err)
			}
		}
		models, err := host.ListModels(ctx, input.Provider, input.Refresh)
		if err != nil {
			return extension.ToolResult{}, err
		}
		if len(models) == 0 {
			return extension.ToolResult{Content: "No models available.", Details: map[string]any{"models": models}}, nil
		}
		lines := make([]string, len(models))
		for i, entry := range models {
			details := ""
			if entry.ContextWindow > 0 {
				details = fmt.Sprintf(" [%d context", entry.ContextWindow)
				if entry.Reasoning {
					details += ", reasoning"
				}
				details += "]"
			} else if entry.Reasoning {
				details = " [reasoning]"
			}
			name := ""
			if entry.Name != "" && entry.Name != entry.ID {
				name = " — " + entry.Name
			}
			lines[i] = fmt.Sprintf("- %s/%s%s%s", entry.Provider, entry.ID, name, details)
		}
		return extension.ToolResult{Content: strings.Join(lines, "\n"), Details: map[string]any{"models": models}}, nil
	}}
}
