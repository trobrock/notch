package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const protocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) != 0 && !bytes.Equal(e.Data, []byte("null")) {
		return fmt.Sprintf("JSON-RPC error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

func validateResponse(response rpcResponse, expectedID int64) error {
	if response.JSONRPC != "2.0" {
		return fmt.Errorf("invalid JSON-RPC version %q", response.JSONRPC)
	}
	want := fmt.Sprintf("%d", expectedID)
	if got := strings.Trim(string(response.ID), `"`); got != want {
		return fmt.Errorf("unexpected JSON-RPC response id %q (want %s)", got, want)
	}
	if response.Error == nil && len(response.Result) == 0 {
		return fmt.Errorf("JSON-RPC response has neither result nor error")
	}
	return nil
}

type rpcClient interface {
	call(context.Context, string, any, any) error
	notify(context.Context, string, any) error
	close() error
}

type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
}

func initialize(ctx context.Context, c rpcClient) error {
	var result initializeResult
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "notch",
			"version": "1",
		},
	}, &result)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if h, ok := c.(interface{ setProtocolVersion(string) }); ok && result.ProtocolVersion != "" {
		h.setProtocolVersion(result.ProtocolVersion)
	}
	if err := c.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("send initialized notification: %w", err)
	}
	return nil
}

type listedTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type listToolsResult struct {
	Tools      []listedTool `json:"tools"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

func listTools(ctx context.Context, c rpcClient) ([]listedTool, error) {
	var all []listedTool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page listToolsResult
		if err := c.call(ctx, "tools/list", params, &page); err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			return all, nil
		}
		if page.NextCursor == cursor {
			return nil, fmt.Errorf("list tools: server repeated cursor %q", cursor)
		}
		cursor = page.NextCursor
	}
}

type callToolResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent any               `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

func renderToolResult(result callToolResult) (string, map[string]any, error) {
	parts := make([]string, 0, len(result.Content)+1)
	for _, raw := range result.Content {
		var block struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			JSON json.RawMessage `json:"json"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return "", nil, fmt.Errorf("decode tool content: %w", err)
		}
		switch {
		case block.Type == "text":
			parts = append(parts, block.Text)
		case block.Type == "json" && len(block.JSON) > 0:
			parts = append(parts, string(block.JSON))
		default:
			parts = append(parts, string(raw))
		}
	}
	var details map[string]any
	if result.StructuredContent != nil {
		details = map[string]any{"structuredContent": result.StructuredContent}
		if len(parts) == 0 {
			data, err := json.Marshal(result.StructuredContent)
			if err != nil {
				return "", nil, fmt.Errorf("encode structured tool content: %w", err)
			}
			parts = append(parts, string(data))
		}
	}
	return strings.Join(parts, "\n"), details, nil
}
