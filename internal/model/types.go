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
	Signature string          `json:"signature,omitempty"`
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
