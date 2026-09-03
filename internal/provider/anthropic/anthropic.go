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
	"strconv"
	"strings"
	"time"

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

	// Authorize supplies the OAuth bearer token for each request, superseding
	// OAuthToken. It is called with an empty string before a request and with
	// a rejected token after HTTP 401, which asks it to refresh. Providers
	// configured with Authorize retry an unauthorized request once.
	Authorize func(ctx context.Context, stale string) (string, error)
}

type provider struct {
	apiKey             string
	oauthToken         string
	authorize          func(ctx context.Context, stale string) (string, error)
	oauthMode          bool
	baseURL            string
	version            string
	apiPricingEligible bool
	httpClient         *http.Client
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
		apiKey: cfg.APIKey, oauthToken: cfg.OAuthToken, authorize: cfg.Authorize, oauthMode: cfg.OAuthMode,
		baseURL: baseURL, version: version,
		apiPricingEligible: strings.TrimSpace(cfg.BaseURL) == "" || strings.EqualFold(baseURL, defaultBaseURL),
		httpClient:         client,
	}
}

type wireRequest struct {
	Model        string            `json:"model"`
	System       any               `json:"system,omitempty"`
	Messages     []wireMessage     `json:"messages"`
	Tools        []wireTool        `json:"tools,omitempty"`
	MaxTokens    int               `json:"max_tokens"`
	Thinking     *wireThinking     `json:"thinking,omitempty"`
	OutputConfig *wireOutputConfig `json:"output_config,omitempty"`
	Stream       bool              `json:"stream"`
}

type wireCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type wireSystemBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireTool struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	InputSchema  map[string]any    `json:"input_schema"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type wireOutputConfig struct {
	Effort string `json:"effort"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

func validReasoningLevel(level string) bool {
	switch level {
	case "", "off", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

// Adaptive thinking was introduced with Claude 4.6. Treat later 4.x
// versions and all 5.x model-family IDs as adaptive as well.
func supportsAdaptiveThinking(modelID string) bool {
	parts := strings.FieldsFunc(strings.ToLower(modelID), func(r rune) bool {
		return r == '-' || r == '.' || r == '/' || r == ':'
	})
	adaptiveVersion := func(majorPart, minorPart string) bool {
		major, err := strconv.Atoi(majorPart)
		if err != nil {
			return false
		}
		if major >= 5 {
			return true
		}
		minor, err := strconv.Atoi(minorPart)
		return err == nil && major == 4 && minor >= 6
	}
	for i, part := range parts {
		if part != "opus" && part != "sonnet" && part != "haiku" {
			continue
		}
		// Current IDs place the family before the version (opus-4-6), but
		// accepting version-first aliases makes model detection robust.
		if i+1 < len(parts) {
			minor := ""
			if i+2 < len(parts) {
				minor = parts[i+2]
			}
			if adaptiveVersion(parts[i+1], minor) {
				return true
			}
		}
		if i >= 2 {
			if adaptiveVersion(parts[i-2], parts[i-1]) {
				return true
			}
		} else if i >= 1 && adaptiveVersion(parts[i-1], "") {
			return true
		}
	}
	return false
}

func adaptiveEffort(level string) string {
	switch level {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "max"
	default:
		return ""
	}
}

func thinkingBudget(level string, maxTokens int) int {
	desired := map[string]int{
		"minimal": 1024,
		"low":     2048,
		"medium":  8192,
		"high":    16384,
		"xhigh":   32768,
	}[level]
	if desired == 0 || maxTokens <= 1 {
		return 0
	}

	// Reserve at least 1024 tokens for the visible answer where possible. For
	// unusually small limits, still keep the thinking budget strictly below the
	// API's max_tokens value.
	limit := maxTokens - 1024
	if limit < 1024 {
		limit = maxTokens - 1
	}
	if desired > limit {
		return limit
	}
	return desired
}

func isOpenAIReasoningSignature(signature string) bool {
	var item struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(signature), &item) == nil && item.Type == "reasoning"
}

func anthropicCacheControl(retention string) *wireCacheControl {
	if retention != "short" && retention != "long" {
		return nil
	}
	control := &wireCacheControl{Type: "ephemeral"}
	if retention == "long" {
		control.TTL = "1h"
	}
	return control
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
			case "thinking":
				// Anthropic requires its own opaque signature when replaying a
				// thinking block. OpenAI reasoning items are display-only here.
				if block.Signature != "" && !isOpenAIReasoningSignature(block.Signature) {
					content = append(content, struct {
						Type      string `json:"type"`
						Thinking  string `json:"thinking"`
						Signature string `json:"signature"`
					}{"thinking", block.Text, block.Signature})
				}
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
				content = append(content, map[string]any{"type": "tool_use", "id": block.ID, "name": name, "input": input})
			case "tool_result", "function_call_output":
				resultText := block.Text
				// Older sessions may contain an empty failed tool result. Anthropic
				// rejects that payload, so repair it while serializing and allow the
				// append-only session to remain resumable without manual editing.
				if block.IsError && strings.TrimSpace(resultText) == "" {
					resultText = "tool execution failed without an error message"
				}
				result := map[string]any{"type": "tool_result", "tool_use_id": block.ToolUseID, "content": resultText}
				if block.IsError {
					result["is_error"] = true
				}
				content = append(content, result)
			}
		}
		messages = append(messages, wireMessage{Role: message.Role, Content: content})
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	cacheControl := anthropicCacheControl(req.CacheRetention)
	tools := make([]wireTool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		name := tool.Name
		if oauthMode {
			name = canonicalToolName(name)
		}
		tools = append(tools, wireTool{Name: name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	if cacheControl != nil && len(tools) != 0 {
		tools[len(tools)-1].CacheControl = cacheControl
	}
	var system any
	if oauthMode {
		blocks := []wireSystemBlock{{Type: "text", Text: claudeCodeSystemBlock, CacheControl: cacheControl}}
		if req.SystemPrompt != "" {
			blocks = append(blocks, wireSystemBlock{Type: "text", Text: req.SystemPrompt, CacheControl: cacheControl})
		}
		system = blocks
	} else if req.SystemPrompt != "" {
		if cacheControl == nil {
			system = req.SystemPrompt
		} else {
			system = []wireSystemBlock{{Type: "text", Text: req.SystemPrompt, CacheControl: cacheControl}}
		}
	}
	if cacheControl != nil && len(messages) != 0 {
		last := &messages[len(messages)-1]
		if last.Role == "user" && len(last.Content) != 0 {
			if block, ok := last.Content[len(last.Content)-1].(map[string]any); ok {
				block["cache_control"] = cacheControl
			}
		}
	}
	wireReq := wireRequest{
		Model: req.Model, System: system, Messages: messages,
		Tools: tools, MaxTokens: maxTokens, Stream: true,
	}
	if req.ReasoningLevel == "" || req.ReasoningLevel == "off" || !validReasoningLevel(req.ReasoningLevel) {
		return wireReq
	}
	if supportsAdaptiveThinking(req.Model) {
		wireReq.Thinking = &wireThinking{Type: "adaptive"}
		wireReq.OutputConfig = &wireOutputConfig{Effort: adaptiveEffort(req.ReasoningLevel)}
	} else {
		wireReq.Thinking = &wireThinking{Type: "enabled", BudgetTokens: thinkingBudget(req.ReasoningLevel, maxTokens)}
	}
	return wireReq
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

// send performs an authorized request. When the provider refreshes its own
// OAuth credential and the service reports the token unauthorized, the token is
// refreshed and the request is retried exactly once, so a long-running session
// survives access-token expiry without a restart.
func (p *provider) send(ctx context.Context, build func(token string) (*http.Request, error)) (*http.Response, error) {
	token, err := p.token(ctx, "")
	if err != nil {
		return nil, err
	}
	httpReq, err := build(token)
	if err != nil {
		return nil, err
	}
	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode != http.StatusUnauthorized || p.authorize == nil {
		return httpResp, nil
	}

	unauthorized := httpStatusError("anthropic", httpResp)
	httpResp.Body.Close()
	refreshed, refreshErr := p.token(ctx, token)
	if refreshErr != nil {
		unauthorized.Message = fmt.Sprintf("%s (token refresh failed: %v)", unauthorized.Message, refreshErr)
		return nil, unauthorized
	}
	if refreshed == token {
		return nil, unauthorized
	}
	retry, err := build(refreshed)
	if err != nil {
		return nil, err
	}
	return p.httpClient.Do(retry)
}

// token returns the OAuth bearer token for a request. A non-empty stale token
// reports a token the service just rejected.
func (p *provider) token(ctx context.Context, stale string) (string, error) {
	if p.authorize == nil {
		return p.oauthToken, nil
	}
	return p.authorize(ctx, stale)
}

// applyAuth sets the credential headers for an outgoing request.
func (p *provider) applyAuth(httpReq *http.Request, token string) {
	if p.oauthMode {
		if token != "" {
			httpReq.Header.Set("Authorization", "Bearer "+token)
		}
		httpReq.Header.Set("anthropic-beta", oauthBeta)
		httpReq.Header.Set("User-Agent", oauthUserAgent)
		httpReq.Header.Set("x-app", "cli")
		return
	}
	if p.apiKey != "" {
		httpReq.Header.Set("x-api-key", p.apiKey)
	}
}

func (p *provider) ListModels(ctx context.Context) ([]model.ModelInfo, error) {
	httpResp, err := p.send(ctx, func(token string) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v1/models?limit=1000", nil)
		if err != nil {
			return nil, fmt.Errorf("anthropic: create model-list request: %w", err)
		}
		httpReq.Header.Set("anthropic-version", p.version)
		p.applyAuth(httpReq, token)
		return httpReq, nil
	})
	if err != nil {
		var providerErr *model.ProviderError
		if errors.As(err, &providerErr) {
			return nil, err
		}
		return nil, fmt.Errorf("anthropic: list models: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, httpStatusError("anthropic", httpResp)
	}
	var envelope struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(httpResp.Body, 16<<20)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("anthropic: decode model list: %w", err)
	}
	models := make([]model.ModelInfo, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		models = append(models, model.ModelInfo{ID: item.ID, Name: item.DisplayName, Reasoning: true})
	}
	return models, nil
}

func (p *provider) Stream(ctx context.Context, req model.Request, onEvent func(model.StreamEvent)) (model.Response, error) {
	if !validReasoningLevel(req.ReasoningLevel) {
		return model.Response{}, fmt.Errorf("anthropic: invalid reasoning level %q", req.ReasoningLevel)
	}
	if !p.apiPricingEligible {
		req.CacheRetention = "none"
		req.CacheKey = ""
	}
	body, err := json.Marshal(makeRequestForMode(req, p.oauthMode))
	if err != nil {
		return model.Response{}, fmt.Errorf("anthropic: encode request: %w", err)
	}
	httpResp, err := p.send(ctx, func(token string) (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("anthropic: create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("anthropic-version", p.version)
		p.applyAuth(httpReq, token)
		return httpReq, nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Response{}, ctxErr
		}
		var providerErr *model.ProviderError
		if errors.As(err, &providerErr) {
			return model.Response{}, err
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
		var envelope struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					InputTokens         int `json:"input_tokens"`
					OutputTokens        int `json:"output_tokens"`
					CacheReadTokens     int `json:"cache_read_input_tokens"`
					CacheCreationTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				Thinking  string          `json:"thinking"`
				Signature string          `json:"signature"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens         int `json:"input_tokens"`
				OutputTokens        int `json:"output_tokens"`
				CacheReadTokens     int `json:"cache_read_input_tokens"`
				CacheCreationTokens int `json:"cache_creation_input_tokens"`
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
			result.CacheReadTokens = envelope.Message.Usage.CacheReadTokens
			result.CacheWriteTokens = envelope.Message.Usage.CacheCreationTokens
		case "content_block_start":
			name := envelope.ContentBlock.Name
			if original, ok := toolNames[strings.ToLower(name)]; ok {
				name = original
			}
			text := envelope.ContentBlock.Text
			if envelope.ContentBlock.Type == "thinking" {
				text = envelope.ContentBlock.Thinking
			}
			b := &streamBlock{block: model.Block{Type: envelope.ContentBlock.Type, Text: text, Signature: envelope.ContentBlock.Signature, ID: envelope.ContentBlock.ID, Name: name}}
			if len(envelope.ContentBlock.Input) > 0 && string(envelope.ContentBlock.Input) != "{}" {
				b.arguments.Write(envelope.ContentBlock.Input)
			}
			blocks[envelope.Index] = b
			if text != "" && onEvent != nil {
				eventType := "text_delta"
				if envelope.ContentBlock.Type == "thinking" {
					eventType = "thinking_delta"
				}
				onEvent(model.StreamEvent{Type: eventType, Text: text})
			}
		case "content_block_delta":
			b := blocks[envelope.Index]
			if b == nil {
				b = &streamBlock{}
				blocks[envelope.Index] = b
			}
			switch envelope.Delta.Type {
			case "thinking_delta":
				if b.block.Type == "" {
					b.block.Type = "thinking"
				}
				b.block.Text += envelope.Delta.Thinking
				if envelope.Delta.Thinking != "" && onEvent != nil {
					onEvent(model.StreamEvent{Type: "thinking_delta", Text: envelope.Delta.Thinking})
				}
			case "signature_delta":
				b.block.Signature += envelope.Delta.Signature
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
			if envelope.Usage.CacheReadTokens != 0 {
				result.CacheReadTokens = envelope.Usage.CacheReadTokens
			}
			if envelope.Usage.CacheCreationTokens != 0 {
				result.CacheWriteTokens = envelope.Usage.CacheCreationTokens
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
	result.APIPricingEligible = p.apiPricingEligible
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
	stop, err := dispatch()
	if err != nil {
		return err
	}
	if stop {
		return nil
	}
	return errors.New("anthropic: SSE stream ended before message_stop")
}

func httpStatusError(service string, response *http.Response) *model.ProviderError {
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
	return &model.ProviderError{
		Message:    fmt.Sprintf("%s: HTTP %s: %s", service, response.Status, message),
		StatusCode: response.StatusCode, Code: firstNonEmpty(envelope.Error.Type, envelope.Error.Code),
		RetryAfter: model.ParseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func apiError(service, kind, message string) error {
	if message == "" {
		message = "streaming API error"
	}
	text := service + ": " + message
	if kind != "" {
		text = fmt.Sprintf("%s: %s: %s", service, kind, message)
	}
	return &model.ProviderError{Message: text, Code: kind}
}
