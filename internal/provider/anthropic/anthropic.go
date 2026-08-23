// Package anthropic implements the Anthropic Messages streaming API.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/trobrock/notch/internal/model"
)

const (
	defaultBaseURL        = "https://api.anthropic.com"
	defaultVersion        = "2023-06-01"
	oauthBeta             = "claude-code-20250219,oauth-2025-04-20"
	oauthUserAgent        = "claude-cli/2.1.75"
	claudeCodeSystemBlock = "You are Claude Code, Anthropic's official CLI for Claude."
)

// Config configures an Anthropic provider.
type Config struct {
	APIKey           string
	OAuthToken       string
	OAuthMode        bool
	BaseURL          string
	HTTPClient       *http.Client
	AnthropicVersion string
}

type provider struct {
	apiKey     string
	oauthToken string
	oauthMode  bool
	baseURL    string
	version    string
	httpClient *http.Client
}

// New returns a provider backed by Anthropic's native Messages API.
func New(cfg Config) model.Provider {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	version := cfg.AnthropicVersion
	if version == "" {
		version = defaultVersion
	}
	return &provider{
		apiKey: cfg.APIKey, oauthToken: cfg.OAuthToken, oauthMode: cfg.OAuthMode,
		baseURL: baseURL, version: version, httpClient: client,
	}
}

type wireRequest struct {
	Model     string                 `json:"model"`
	System    any                    `json:"system,omitempty"`
	Messages  []wireMessage          `json:"messages"`
	Tools     []model.ToolDefinition `json:"tools,omitempty"`
	MaxTokens int                    `json:"max_tokens"`
	Stream    bool                   `json:"stream"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

func makeRequest(req model.Request) wireRequest {
	return makeRequestForMode(req, false)
}

func makeRequestForMode(req model.Request, oauthMode bool) wireRequest {
	messages := make([]wireMessage, 0, len(req.Messages))
	for _, message := range req.Messages {
		content := make([]any, 0, len(message.Content))
		for _, block := range message.Content {
			switch block.Type {
			case "text":
				content = append(content, map[string]any{"type": "text", "text": block.Text})
			case "tool_use", "function_call":
				input := block.Arguments
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				name := block.Name
				if oauthMode {
					name = canonicalToolName(name)
				}
				content = append(content, struct {
					Type  string          `json:"type"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				}{"tool_use", block.ID, name, input})
			case "tool_result", "function_call_output":
				content = append(content, struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id"`
					Content   string `json:"content"`
					IsError   bool   `json:"is_error,omitempty"`
				}{"tool_result", block.ToolUseID, block.Text, block.IsError})
			}
		}
		messages = append(messages, wireMessage{Role: message.Role, Content: content})
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	tools := req.Tools
	if oauthMode && len(req.Tools) != 0 {
		tools = append([]model.ToolDefinition(nil), req.Tools...)
		for i := range tools {
			tools[i].Name = canonicalToolName(tools[i].Name)
		}
	}
	var system any
	if oauthMode {
		blocks := []map[string]string{{"type": "text", "text": claudeCodeSystemBlock}}
		if req.SystemPrompt != "" {
			blocks = append(blocks, map[string]string{"type": "text", "text": req.SystemPrompt})
		}
		system = blocks
	} else if req.SystemPrompt != "" {
		system = req.SystemPrompt
	}
	return wireRequest{
		Model: req.Model, System: system, Messages: messages,
		Tools: tools, MaxTokens: maxTokens, Stream: true,
	}
}

func canonicalToolName(name string) string {
	switch strings.ToLower(name) {
	case "read":
		return "Read"
	case "write":
		return "Write"
	case "edit":
		return "Edit"
	case "bash":
		return "Bash"
	case "grep":
		return "Grep"
	case "find", "glob":
		return "Glob"
	default:
		return name
	}
}

// responseToolNames maps names sent to Claude back to the exact registered
// spelling expected by the local tool registry.
func responseToolNames(tools []model.ToolDefinition, oauthMode bool) map[string]string {
	if !oauthMode {
		return nil
	}
	names := make(map[string]string, len(tools)*2)
	for _, tool := range tools {
		names[strings.ToLower(tool.Name)] = tool.Name
		names[strings.ToLower(canonicalToolName(tool.Name))] = tool.Name
	}
	return names
}

type streamBlock struct {
	block       model.Block
	arguments   bytes.Buffer
	sawArgDelta bool
}

func (p *provider) Stream(ctx context.Context, req model.Request, onEvent func(model.StreamEvent)) (model.Response, error) {
	body, err := json.Marshal(makeRequestForMode(req, p.oauthMode))
	if err != nil {
		return model.Response{}, fmt.Errorf("anthropic: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return model.Response{}, fmt.Errorf("anthropic: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", p.version)
	if p.oauthMode {
		if p.oauthToken != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.oauthToken)
		}
		httpReq.Header.Set("anthropic-beta", oauthBeta)
		httpReq.Header.Set("User-Agent", oauthUserAgent)
		httpReq.Header.Set("x-app", "cli")
	} else if p.apiKey != "" {
		httpReq.Header.Set("x-api-key", p.apiKey)
	}

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Response{}, ctxErr
		}
		return model.Response{}, fmt.Errorf("anthropic: send request: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return model.Response{}, httpStatusError("anthropic", httpResp)
	}

	var result model.Response
	blocks := make(map[int]*streamBlock)
	toolNames := responseToolNames(req.Tools, p.oauthMode)
	err = readSSE(httpResp.Body, func(event, data string) (bool, error) {
		if data == "[DONE]" {
			return true, nil
		}
		var envelope struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			return false, fmt.Errorf("anthropic: decode SSE event %q: %w", event, err)
		}
		kind := envelope.Type
		if kind == "" {
			kind = event
		}
		switch kind {
		case "message_start":
			result.InputTokens = envelope.Message.Usage.InputTokens
			result.OutputTokens = envelope.Message.Usage.OutputTokens
		case "content_block_start":
			name := envelope.ContentBlock.Name
			if original, ok := toolNames[strings.ToLower(name)]; ok {
				name = original
			}
			b := &streamBlock{block: model.Block{Type: envelope.ContentBlock.Type, Text: envelope.ContentBlock.Text, ID: envelope.ContentBlock.ID, Name: name}}
			if len(envelope.ContentBlock.Input) > 0 && string(envelope.ContentBlock.Input) != "{}" {
				b.arguments.Write(envelope.ContentBlock.Input)
			}
			blocks[envelope.Index] = b
			if envelope.ContentBlock.Text != "" && onEvent != nil {
				onEvent(model.StreamEvent{Type: "text_delta", Text: envelope.ContentBlock.Text})
			}
		case "content_block_delta":
			b := blocks[envelope.Index]
			if b == nil {
				b = &streamBlock{}
				blocks[envelope.Index] = b
			}
			switch envelope.Delta.Type {
			case "text_delta":
				if b.block.Type == "" {
					b.block.Type = "text"
				}
				b.block.Text += envelope.Delta.Text
				if onEvent != nil {
					onEvent(model.StreamEvent{Type: "text_delta", Text: envelope.Delta.Text})
				}
			case "input_json_delta":
				if !b.sawArgDelta {
					b.arguments.Reset()
					b.sawArgDelta = true
				}
				b.arguments.WriteString(envelope.Delta.PartialJSON)
				if onEvent != nil {
					onEvent(model.StreamEvent{Type: "input_json_delta", Text: envelope.Delta.PartialJSON})
				}
			}
		case "message_delta":
			result.StopReason = envelope.Delta.StopReason
			if envelope.Usage.InputTokens != 0 {
				result.InputTokens = envelope.Usage.InputTokens
			}
			result.OutputTokens = envelope.Usage.OutputTokens
		case "message_stop":
			return true, nil
		case "error":
			return false, apiError("anthropic", envelope.Error.Type, envelope.Error.Message)
		}
		return false, nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Response{}, ctxErr
		}
		return model.Response{}, err
	}

	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		b := blocks[index]
		if b.arguments.Len() != 0 {
			b.block.Arguments = json.RawMessage(append([]byte(nil), b.arguments.Bytes()...))
		}
		result.Content = append(result.Content, b.block)
	}
	return result, nil
}

func readSSE(r io.Reader, handle func(event, data string) (bool, error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 32*1024), 16*1024*1024)
	var event string
	var data []string
	dispatch := func() (bool, error) {
		if len(data) == 0 {
			event = ""
			return false, nil
		}
		stop, err := handle(event, strings.Join(data, "\n"))
		event, data = "", nil
		return stop, err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			stop, err := dispatch()
			if err != nil || stop {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("anthropic: read SSE stream: %w", err)
	}
	_, err := dispatch()
	return err
}

func httpStatusError(service string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	message := strings.TrimSpace(string(body))
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
		if envelope.Error.Type != "" {
			message = envelope.Error.Type + ": " + message
		} else if envelope.Error.Code != "" {
			message = envelope.Error.Code + ": " + message
		}
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("%s: HTTP %s: %s", service, response.Status, message)
}

func apiError(service, kind, message string) error {
	if message == "" {
		message = "streaming API error"
	}
	if kind != "" {
		return fmt.Errorf("%s: %s: %s", service, kind, message)
	}
	return errors.New(service + ": " + message)
}
