package model

import (
	"bytes"
	"context"
	"encoding/json"
)

// Block is one typed part of a conversation message.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Signature string          `json:"signature,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

const ToolCallInputErrorMessage = "tool call input was truncated or invalid JSON; retry with smaller input or write the content to a file first"

func TextMessage(role, text string) Message {
	return Message{Role: role, Content: []Block{{Type: "text", Text: text}}}
}

// NormalizeToolCallArguments makes tool inputs safe to store and encode.
// Empty input means an empty object. Invalid or non-object JSON is replaced and
// marked as an error so the agent can return a recovery result to the model.
func NormalizeToolCallArguments(blocks []Block) int {
	invalid := 0
	for i := range blocks {
		block := &blocks[i]
		if block.Type != "tool_use" && block.Type != "function_call" {
			continue
		}
		if len(block.Arguments) == 0 {
			block.Arguments = json.RawMessage(`{}`)
			continue
		}
		trimmed := bytes.TrimSpace(block.Arguments)
		if !json.Valid(trimmed) || len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
			block.Arguments = json.RawMessage(`{}`)
			block.IsError = true
			invalid++
		}
	}
	return invalid
}

// CleanMessages returns an independent copy with safe tool-call arguments.
func CleanMessages(messages []Message) ([]Message, int) {
	if messages == nil {
		return nil, 0
	}
	cleaned := make([]Message, len(messages))
	invalid := 0
	for i, message := range messages {
		cleaned[i].Role = message.Role
		if message.Content == nil {
			continue
		}
		cleaned[i].Content = make([]Block, len(message.Content))
		copy(cleaned[i].Content, message.Content)
		for j := range cleaned[i].Content {
			cleaned[i].Content[j].Arguments = append(json.RawMessage(nil), message.Content[j].Arguments...)
		}
		invalid += NormalizeToolCallArguments(cleaned[i].Content)
	}
	return cleaned, invalid
}

// CleanAndRepairMessages returns an independent copy with safe arguments and
// error results for invalid tool-call batches.
func CleanAndRepairMessages(messages []Message, errorText string) ([]Message, int, int) {
	cleaned, invalid := CleanMessages(messages)
	repaired, inserted := repairToolCallResults(cleaned, errorText)
	return repaired, invalid, inserted
}

// RepairToolCallResults adds error results for an invalid tool call and any
// unresolved sibling calls in the same assistant response.
func RepairToolCallResults(messages []Message, errorText string) ([]Message, int) {
	cleaned, _ := CleanMessages(messages)
	return repairToolCallResults(cleaned, errorText)
}

func repairToolCallResults(repaired []Message, errorText string) ([]Message, int) {
	inserted := 0
	for i := 0; i < len(repaired); i++ {
		hasInvalidCall := false
		for _, block := range repaired[i].Content {
			if (block.Type == "tool_use" || block.Type == "function_call") && block.IsError {
				hasInvalidCall = true
				break
			}
		}
		if !hasInvalidCall {
			continue
		}
		var missing []Block
		for _, block := range repaired[i].Content {
			if block.Type != "tool_use" && block.Type != "function_call" {
				continue
			}
			found := false
			if i+1 < len(repaired) && repaired[i+1].Role == "user" {
				for _, result := range repaired[i+1].Content {
					if (result.Type == "tool_result" || result.Type == "function_call_output") && result.ToolUseID == block.ID {
						found = true
						break
					}
				}
			}
			if !found {
				missing = append(missing, Block{Type: "tool_result", ToolUseID: block.ID, Text: errorText, IsError: true})
			}
		}
		if len(missing) == 0 {
			continue
		}
		inserted += len(missing)
		if i+1 < len(repaired) && repaired[i+1].Role == "user" {
			repaired[i+1].Content = append(missing, repaired[i+1].Content...)
			continue
		}
		recovery := Message{Role: "user", Content: missing}
		repaired = append(repaired, Message{})
		copy(repaired[i+2:], repaired[i+1:])
		repaired[i+1] = recovery
		i++
	}
	return repaired, inserted
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type Request struct {
	Model          string           `json:"model"`
	SystemPrompt   string           `json:"system_prompt"`
	Messages       []Message        `json:"messages"`
	Tools          []ToolDefinition `json:"tools,omitempty"`
	MaxTokens      int              `json:"max_tokens,omitempty"`
	ReasoningLevel string           `json:"reasoning_level,omitempty"`
	CacheRetention string           `json:"cache_retention,omitempty"`
	CacheKey       string           `json:"cache_key,omitempty"`
}

type StreamEvent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type Response struct {
	Content            []Block  `json:"content"`
	StopReason         string   `json:"stop_reason"`
	InputTokens        int      `json:"input_tokens,omitempty"`
	OutputTokens       int      `json:"output_tokens,omitempty"`
	CacheReadTokens    int      `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens   int      `json:"cache_write_tokens,omitempty"`
	ReasoningTokens    int      `json:"reasoning_tokens,omitempty"`
	CostUSD            *float64 `json:"cost_usd,omitempty"`
	APIPricingEligible bool     `json:"-"`
}

func (r Response) TotalInputTokens() int {
	return r.InputTokens + r.CacheReadTokens + r.CacheWriteTokens
}

func (r Response) TotalTokens() int {
	return r.TotalInputTokens() + r.OutputTokens
}

type Provider interface {
	Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (Response, error)
}

// ModelInfo is provider-discovered metadata used by the runtime model registry.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
}

// ModelLister is optionally implemented by providers with a model-list API.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// DiscoverableProvider is the common capability set implemented by Notch's
// built-in providers. Keeping model discovery as a separate capability lets
// future providers support generation even when their API has no catalog.
type DiscoverableProvider interface {
	Provider
	ModelLister
}
