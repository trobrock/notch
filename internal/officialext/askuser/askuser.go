// Package askuser implements Notch's official ask_user_question extension.
package askuser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

const source = "official:ask-user-question"

type questionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type askUserQuestionArgs struct {
	Question            string           `json:"question"`
	Options             []questionOption `json:"options,omitempty"`
	AllowCustomResponse *bool            `json:"allowCustomResponse,omitempty"`
	Placeholder         string           `json:"placeholder,omitempty"`
}

// Register registers ask_user_question.
func Register(registry *extension.Registry, host extension.Host) error {
	if registry == nil {
		return errors.New("register official extensions: nil registry")
	}
	if host == nil {
		return errors.New("register official extensions: nil host")
	}
	if err := registry.RegisterTool(newAskUserQuestion(host)); err != nil {
		return fmt.Errorf("register official extension %q: %w", source, err)
	}
	return nil
}

func newAskUserQuestion(host extension.Host) extension.Tool {
	return extension.Tool{
		Source: source,
		Definition: model.ToolDefinition{
			Name:        "ask_user_question",
			Description: "Ask the user a blocking question and return their selected or typed answer. Use this for clarification, preferences, or decisions instead of guessing.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string", "minLength": 1, "description": "The question to ask the user."},
					"options": map[string]any{
						"type": "array", "maxItems": 100, "description": "Suggested answers. Omit or leave empty for free-form input.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"label":       map[string]any{"type": "string", "minLength": 1, "maxLength": 8000, "description": "Short answer label shown to the user."},
								"description": map[string]any{"type": "string", "maxLength": 16000, "description": "Optional detail shown next to the label."},
							},
							"required": []string{"label"}, "additionalProperties": false,
						},
					},
					"allowCustomResponse": map[string]any{"type": "boolean", "description": "Whether the user may type a custom answer. Defaults to true without options and false with options."},
					"placeholder":         map[string]any{"type": "string", "description": "Placeholder text for a custom or free-form answer."},
				},
				"required": []string{"question"}, "additionalProperties": false,
			},
		},
		Execute: func(ctx context.Context, raw json.RawMessage, update func(string)) (extension.ToolResult, error) {
			args, err := decodeQuestionArgs(raw)
			if err != nil {
				return extension.ToolResult{}, err
			}
			if update != nil {
				update("waiting for user response")
			}
			return askUser(ctx, host, args)
		},
	}
}

func decodeQuestionArgs(raw json.RawMessage) (askUserQuestionArgs, error) {
	var args askUserQuestionArgs
	if len(raw) == 0 {
		return args, errors.New("arguments are required")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return args, fmt.Errorf("decode arguments: %w", err)
	}
	if strings.TrimSpace(args.Question) == "" {
		return args, errors.New("question must not be empty")
	}
	if len(args.Options) > 100 {
		return args, errors.New("question supports at most 100 choices")
	}
	seen := map[string]bool{}
	for i, option := range args.Options {
		if strings.TrimSpace(option.Label) == "" || seen[option.Label] || len(option.Label) > 8000 || len(option.Description) > 16000 {
			return args, fmt.Errorf("option %d label must be nonempty and unique", i+1)
		}
		seen[option.Label] = true
	}
	if len(args.Options) == 0 && args.AllowCustomResponse != nil && !*args.AllowCustomResponse {
		return args, errors.New("question has no allowed answers")
	}
	return args, nil
}

func askUser(ctx context.Context, host extension.Host, args askUserQuestionArgs) (extension.ToolResult, error) {
	labels := make([]string, len(args.Options))
	displays := make([]string, len(args.Options))
	for i, option := range args.Options {
		labels[i] = option.Label
		displays[i] = optionDisplay(option, i)
	}
	allowCustom := len(args.Options) == 0
	if args.AllowCustomResponse != nil {
		allowCustom = *args.AllowCustomResponse
	}

	answer, wasCustom, structured, err := chooseAnswer(ctx, host, args, displays, allowCustom)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
			return canceledResult(args.Question, labels, wasCustom), nil
		}
		return extension.ToolResult{}, err
	}
	if len(answer) > 8000 {
		return extension.ToolResult{}, errors.New("answer exceeds 8000 bytes; please use a shorter response")
	}
	prefix := "User answered: "
	if structured {
		if wasCustom {
			prefix = "User typed a custom answer: "
		} else {
			prefix = "User selected an option: "
		}
	}
	return extension.ToolResult{
		Content: prefix + answer,
		Details: map[string]any{
			"question": args.Question, "options": labels, "answer": answer,
			"wasCustom": wasCustom, "cancelled": false,
		},
	}, nil
}

func canceledResult(question string, labels []string, wasCustom bool) extension.ToolResult {
	return extension.ToolResult{
		Content: "User cancelled the question.",
		Details: map[string]any{
			"question": question, "options": labels, "answer": nil,
			"wasCustom": wasCustom, "cancelled": true,
		},
	}
}

func chooseAnswer(ctx context.Context, host extension.Host, args askUserQuestionArgs, displays []string, allowCustom bool) (string, bool, bool, error) {
	if structured, ok := host.(extension.QuestionHost); ok {
		options := make([]extension.QuestionOption, len(args.Options))
		for i, option := range args.Options {
			options[i] = extension.QuestionOption{Label: option.Label, Description: option.Description}
		}
		response, err := structured.Question(ctx, extension.Question{Prompt: args.Question, Options: options, AllowCustomResponse: allowCustom, Placeholder: args.Placeholder})
		if !errors.Is(err, extension.ErrStructuredQuestionUnsupported) {
			return response.Value, response.WasCustom, true, err
		}
	}
	if len(args.Options) == 0 {
		answer, err := host.Input(ctx, args.Question, args.Placeholder)
		return answer, true, false, err
	}

	// Host.Select returns display text, not an index. Disambiguate collisions,
	// including a real option with the same text as the custom-entry UI row.
	seen := map[string]bool{}
	collision := false
	for _, display := range displays {
		if seen[display] || allowCustom && display == "Type a custom response" {
			collision = true
		}
		seen[display] = true
	}
	if collision {
		displays = append([]string(nil), displays...)
		for i := range displays {
			displays[i] = fmt.Sprintf("[%d] %s", i+1, displays[i])
		}
	}
	choices := append([]string(nil), displays...)
	if allowCustom {
		choices = append(choices, "Type a custom response")
	}
	selected, err := host.Select(ctx, args.Question, choices)
	if err != nil {
		return "", false, false, err
	}
	for i, display := range displays {
		if selected == display {
			return args.Options[i].Label, false, false, nil
		}
	}
	if !allowCustom || selected != choices[len(choices)-1] {
		return "", false, false, fmt.Errorf("UI returned unknown selection %q", selected)
	}
	answer, err := host.Input(ctx, args.Question, args.Placeholder)
	return answer, true, false, err
}

func optionDisplay(option questionOption, _ int) string {
	display := option.Label
	if option.Description != "" {
		display += " — " + option.Description
	}
	return display
}
