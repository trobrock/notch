package extension

import (
	"context"

	"github.com/trobrock/notch/internal/model"
)

type toolResultsContextKey struct{}

// WithContextToolResults records successful tool results in the exact context
// sent to the model. It copies only string references, not message payloads, and
// exposes no mutable history. Call after context filtering and hooks.
func WithContextToolResults(ctx context.Context, messages []model.Message) context.Context {
	names := make(map[string]string)
	results := make(map[string][]string)
	for _, message := range messages {
		for _, block := range message.Content {
			if message.Role == "assistant" && (block.Type == "tool_use" || block.Type == "function_call") {
				names[block.ID] = block.Name
			}
			if (block.Type == "tool_result" || block.Type == "function_call_output") && !block.IsError {
				if name := names[block.ToolUseID]; name != "" {
					results[name] = append(results[name], block.Text)
				}
			}
		}
	}
	return context.WithValue(ctx, toolResultsContextKey{}, results)
}

// HasContextToolResult reports whether an identical successful result from the
// named tool was in the context seen by the model issuing the current call.
// Absent context (e.g. direct extension invocation) always returns false.
func HasContextToolResult(ctx context.Context, name, content string) bool {
	results, _ := ctx.Value(toolResultsContextKey{}).(map[string][]string)
	for _, text := range results[name] {
		if text == content {
			return true
		}
	}
	return false
}
