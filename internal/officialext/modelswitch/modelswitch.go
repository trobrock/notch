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
	tool := extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "switch_model", Description: "Change the model and optionally provider used by subsequent Notch turns while preserving the current conversation.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
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
	if err := registry.RegisterTool(tool); err != nil {
		return fmt.Errorf("register switch_model: %w", err)
	}
	return nil
}
