package model

import (
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
	IsError   bool            `json:"is_error,omitempty"`
}

type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

func TextMessage(role, text string) Message {
	return Message{Role: role, Content: []Block{{Type: "text", Text: text}}}
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type Request struct {
	Model        string           `json:"model"`
	SystemPrompt string           `json:"system_prompt"`
	Messages     []Message        `json:"messages"`
	Tools        []ToolDefinition `json:"tools,omitempty"`
	MaxTokens    int              `json:"max_tokens,omitempty"`
}

type StreamEvent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type Response struct {
	Content      []Block `json:"content"`
	StopReason   string  `json:"stop_reason"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
}

type Provider interface {
	Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (Response, error)
}
