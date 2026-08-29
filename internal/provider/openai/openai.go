// Package openai implements the OpenAI Responses streaming API.
package openai

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
	"time"

	"github.com/trobrock/notch/internal/model"
)

const defaultBaseURL = "https://api.openai.com"

// Config configures an OpenAI provider.
type Config struct {
	APIKey           string
	BaseURL          string
	Endpoint         string
	Headers          map[string]string
	CodexMode        bool
	OfficialEndpoint bool
	HTTPClient       *http.Client
}

type provider struct {
	apiKey            string
	baseURL           string
	endpoint          string
	headers           map[string]string
	codexMode         bool
	promptCacheFields bool
	httpClient        *http.Client
}

// New returns a provider backed by OpenAI's native Responses API.
func New(cfg Config) model.Provider {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "/v1/responses"
		if cfg.CodexMode {
			endpoint = "/codex/responses"
		}
	}
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	headers := make(map[string]string, len(cfg.Headers))
	for name, value := range cfg.Headers {
		headers[name] = value
	}
	return &provider{
		apiKey: cfg.APIKey, baseURL: baseURL, endpoint: endpoint, headers: headers,
		codexMode:         cfg.CodexMode,
		promptCacheFields: cfg.OfficialEndpoint || strings.TrimSpace(cfg.BaseURL) == "" || strings.EqualFold(baseURL, defaultBaseURL),
		httpClient:        client,
	}
}

type wireRequest struct {
	Model                string                  `json:"model"`
	Instructions         string                  `json:"instructions,omitempty"`
	Input                []any                   `json:"input"`
	Tools                []wireTool              `json:"tools,omitempty"`
	MaxOutputTokens      int                     `json:"max_output_tokens,omitempty"`
	Reasoning            *wireReasoning          `json:"reasoning,omitempty"`
	PromptCacheKey       string                  `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string                  `json:"prompt_cache_retention,omitempty"`
	PromptCacheOptions   *wirePromptCacheOptions `json:"prompt_cache_options,omitempty"`
	Stream               bool                    `json:"stream"`
}

type codexWireRequest struct {
	Model             string         `json:"model"`
	Store             bool           `json:"store"`
	Stream            bool           `json:"stream"`
	Instructions      string         `json:"instructions"`
	Input             []any          `json:"input"`
	Tools             []wireTool     `json:"tools"`
	Text              wireText       `json:"text"`
	Include           []string       `json:"include"`
	ToolChoice        string         `json:"tool_choice"`
	ParallelToolCalls bool           `json:"parallel_tool_calls"`
	Reasoning         *wireReasoning `json:"reasoning,omitempty"`
	PromptCacheKey    string         `json:"prompt_cache_key,omitempty"`
}

type wirePromptCacheOptions struct {
	Mode string `json:"mode"`
}

type wireReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
}

type wireText struct {
	Verbosity string `json:"verbosity"`
}

type wireTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

func validReasoningLevel(level string) bool {
	switch level {
	case "", "off", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func reasoningForLevel(level string) *wireReasoning {
	if level == "" || level == "off" || !validReasoningLevel(level) {
		return nil
	}
	return &wireReasoning{Effort: level, Summary: "auto"}
}

func replayReasoningItem(signature string) (json.RawMessage, bool) {
	raw := json.RawMessage(signature)
	var item struct {
		Type             string `json:"type"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if json.Unmarshal(raw, &item) != nil || item.Type != "reasoning" || item.EncryptedContent == "" {
		return nil, false
	}
	return append(json.RawMessage(nil), raw...), true
}

func openAICacheKey(retention, key string) string {
	if retention == "none" {
		return ""
	}
	runes := []rune(key)
	if len(runes) > 64 {
		runes = runes[:64]
	}
	return string(runes)
}

func supportsLongPromptCache(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	return strings.HasPrefix(id, "gpt-5")
}

func supportsExplicitPromptCache(modelID string) bool {
	return strings.Contains(strings.ToLower(modelID), "gpt-5.6")
}

func makeRequest(req model.Request) wireRequest {
	input := make([]any, 0, len(req.Messages))
	for _, message := range req.Messages {
		var textContent []any
		flushText := func() {
			if len(textContent) == 0 {
				return
			}
			input = append(input, struct {
				Role    string `json:"role"`
				Content []any  `json:"content"`
			}{message.Role, textContent})
			textContent = nil
		}
		for _, block := range message.Content {
			switch block.Type {
			case "thinking":
				if item, ok := replayReasoningItem(block.Signature); ok {
					flushText()
					input = append(input, item)
				}
			case "text":
				contentType := "input_text"
				if message.Role == "assistant" {
					contentType = "output_text"
				}
				textContent = append(textContent, map[string]any{"type": contentType, "text": block.Text})
			case "tool_use", "function_call":
				flushText()
				arguments := string(block.Arguments)
				if arguments == "" {
					arguments = "{}"
				}
				input = append(input, struct {
					Type      string `json:"type"`
					ID        string `json:"id,omitempty"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{"function_call", block.Signature, block.ID, block.Name, arguments})
			case "tool_result", "function_call_output":
				flushText()
				input = append(input, struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Output string `json:"output"`
				}{"function_call_output", block.ToolUseID, block.Text})
			}
		}
		flushText()
	}
	tools := make([]wireTool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		tools = append(tools, wireTool{
			Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema,
		})
	}
	cacheKey := openAICacheKey(req.CacheRetention, req.CacheKey)
	cacheRetention := ""
	if req.CacheRetention == "long" && supportsLongPromptCache(req.Model) {
		cacheRetention = "24h"
	}
	var cacheOptions *wirePromptCacheOptions
	if req.CacheRetention == "none" && supportsExplicitPromptCache(req.Model) {
		cacheOptions = &wirePromptCacheOptions{Mode: "explicit"}
	}
	return wireRequest{
		Model: req.Model, Instructions: req.SystemPrompt, Input: input, Tools: tools,
		MaxOutputTokens: req.MaxTokens, Reasoning: reasoningForLevel(req.ReasoningLevel),
		PromptCacheKey: cacheKey, PromptCacheRetention: cacheRetention, PromptCacheOptions: cacheOptions,
		Stream: true,
	}
}

type outputSlot struct {
	kind     string
	parts    map[int]*model.Block
	call     model.Block
	thinking model.Block
	args     bytes.Buffer
}

type outputItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	Output           string `json:"output"`
	EncryptedContent string `json:"encrypted_content"`
	raw              json.RawMessage
	Content          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

func (i *outputItem) UnmarshalJSON(data []byte) error {
	type wireOutputItem outputItem
	var item wireOutputItem
	if err := json.Unmarshal(data, &item); err != nil {
		return err
	}
	*i = outputItem(item)
	i.raw = append(i.raw[:0], data...)
	return nil
}

type responseData struct {
	Status            string       `json:"status"`
	Output            []outputItem `json:"output"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *provider) ListModels(ctx context.Context) ([]model.ModelInfo, error) {
	if p.codexMode {
		return nil, errors.New("openai-codex: model listing is unavailable; using bundled registry")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("openai: create model-list request: %w", err)
	}
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for name, value := range p.headers {
		httpReq.Header.Set(name, value)
	}
	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: list models: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, httpStatusError(httpResp)
	}
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(httpResp.Body, 16<<20)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("openai: decode model list: %w", err)
	}
	models := make([]model.ModelInfo, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		lower := strings.ToLower(id)
		reasoning := strings.Contains(lower, "gpt-5") || strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") || strings.HasPrefix(lower, "o4")
		models = append(models, model.ModelInfo{ID: id, Name: id, Reasoning: reasoning})
	}
	return models, nil
}

func (p *provider) Stream(ctx context.Context, req model.Request, onEvent func(model.StreamEvent)) (model.Response, error) {
	if !validReasoningLevel(req.ReasoningLevel) {
		return model.Response{}, fmt.Errorf("openai: invalid reasoning level %q", req.ReasoningLevel)
	}
	wireReq := makeRequest(req)
	if !p.promptCacheFields {
		wireReq.PromptCacheKey = ""
		wireReq.PromptCacheRetention = ""
		wireReq.PromptCacheOptions = nil
	}
	var requestBody any = wireReq
	if p.codexMode {
		requestBody = codexWireRequest{
			Model: wireReq.Model, Store: false, Stream: true,
			Instructions: wireReq.Instructions, Input: wireReq.Input, Tools: wireReq.Tools,
			Text: wireText{Verbosity: "low"}, Include: []string{"reasoning.encrypted_content"},
			ToolChoice: "auto", ParallelToolCalls: true, Reasoning: wireReq.Reasoning,
			PromptCacheKey: wireReq.PromptCacheKey,
		}
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return model.Response{}, fmt.Errorf("openai: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+p.endpoint, bytes.NewReader(body))
	if err != nil {
		return model.Response{}, fmt.Errorf("openai: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for name, value := range p.headers {
		httpReq.Header.Set(name, value)
	}

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Response{}, ctxErr
		}
		return model.Response{}, fmt.Errorf("openai: send request: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return model.Response{}, httpStatusError(httpResp)
	}

	var result model.Response
	slots := make(map[int]*outputSlot)
	err = readSSE(httpResp.Body, func(event, data string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if data == "[DONE]" {
			return true, nil
		}
		var envelope struct {
			Type         string          `json:"type"`
			OutputIndex  int             `json:"output_index"`
			ContentIndex int             `json:"content_index"`
			Delta        string          `json:"delta"`
			Text         string          `json:"text"`
			Arguments    string          `json:"arguments"`
			Item         outputItem      `json:"item"`
			Part         json.RawMessage `json:"part"`
			Response     responseData    `json:"response"`
			Code         string          `json:"code"`
			Message      string          `json:"message"`
			Error        struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			return false, fmt.Errorf("openai: decode SSE event %q: %w", event, err)
		}
		kind := envelope.Type
		if kind == "" {
			kind = event
		}
		switch kind {
		case "response.output_item.added":
			mergeOutputItem(slots, envelope.OutputIndex, envelope.Item, false)
		case "response.content_part.added":
			var part struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(envelope.Part, &part); err != nil {
				return false, fmt.Errorf("openai: decode content part: %w", err)
			}
			if part.Type == "output_text" {
				block := textPart(slots, envelope.OutputIndex, envelope.ContentIndex)
				block.Text = part.Text
				if part.Text != "" && onEvent != nil {
					onEvent(model.StreamEvent{Type: "text_delta", Text: part.Text})
				}
			}
		case "response.output_text.delta":
			block := textPart(slots, envelope.OutputIndex, envelope.ContentIndex)
			block.Text += envelope.Delta
			if onEvent != nil {
				onEvent(model.StreamEvent{Type: "text_delta", Text: envelope.Delta})
			}
		case "response.output_text.done":
			block := textPart(slots, envelope.OutputIndex, envelope.ContentIndex)
			if block.Text == "" {
				block.Text = envelope.Text
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			block := reasoningPart(slots, envelope.OutputIndex)
			block.Text += envelope.Delta
			if envelope.Delta != "" && onEvent != nil {
				onEvent(model.StreamEvent{Type: "thinking_delta", Text: envelope.Delta})
			}
		case "response.reasoning_summary_part.done":
			block := reasoningPart(slots, envelope.OutputIndex)
			if block.Text != "" && !strings.HasSuffix(block.Text, "\n\n") {
				block.Text += "\n\n"
				if onEvent != nil {
					onEvent(model.StreamEvent{Type: "thinking_delta", Text: "\n\n"})
				}
			}
		case "response.reasoning_summary_text.done":
			block := reasoningPart(slots, envelope.OutputIndex)
			if block.Text == "" {
				block.Text = envelope.Text
			}
		case "response.function_call_arguments.delta":
			slot := functionSlot(slots, envelope.OutputIndex)
			slot.args.WriteString(envelope.Delta)
			if onEvent != nil {
				onEvent(model.StreamEvent{Type: "input_json_delta", Text: envelope.Delta})
			}
		case "response.function_call_arguments.done":
			slot := functionSlot(slots, envelope.OutputIndex)
			if slot.args.Len() == 0 {
				slot.args.WriteString(envelope.Arguments)
			}
		case "response.output_item.done":
			mergeOutputItem(slots, envelope.OutputIndex, envelope.Item, true)
		case "response.completed":
			applyResponse(&result, slots, envelope.Response)
			if hasFunctionCall(slots) {
				result.StopReason = "tool_use"
			} else if result.StopReason == "" {
				result.StopReason = "end_turn"
			}
			return true, nil
		case "response.incomplete":
			applyResponse(&result, slots, envelope.Response)
			if envelope.Response.IncompleteDetails.Reason == "max_output_tokens" {
				result.StopReason = "max_tokens"
			} else {
				result.StopReason = envelope.Response.IncompleteDetails.Reason
			}
			return true, nil
		case "response.failed":
			message := envelope.Response.Error.Message
			code := envelope.Response.Error.Code
			return false, apiError(code, message)
		case "error":
			code, message := envelope.Code, envelope.Message
			if message == "" {
				code, message = envelope.Error.Code, envelope.Error.Message
			}
			return false, apiError(code, message)
		}
		return false, nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Response{}, ctxErr
		}
		return model.Response{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return model.Response{}, ctxErr
	}

	result.Content = flattenSlots(slots)
	result.APIPricingEligible = p.promptCacheFields
	return result, nil
}

func textPart(slots map[int]*outputSlot, outputIndex, contentIndex int) *model.Block {
	slot := slots[outputIndex]
	if slot == nil {
		slot = &outputSlot{}
		slots[outputIndex] = slot
	}
	slot.kind = "message"
	if slot.parts == nil {
		slot.parts = make(map[int]*model.Block)
	}
	block := slot.parts[contentIndex]
	if block == nil {
		block = &model.Block{Type: "text"}
		slot.parts[contentIndex] = block
	}
	return block
}

func reasoningPart(slots map[int]*outputSlot, outputIndex int) *model.Block {
	slot := slots[outputIndex]
	if slot == nil {
		slot = &outputSlot{}
		slots[outputIndex] = slot
	}
	slot.kind = "reasoning"
	slot.thinking.Type = "thinking"
	return &slot.thinking
}

func functionSlot(slots map[int]*outputSlot, outputIndex int) *outputSlot {
	slot := slots[outputIndex]
	if slot == nil {
		slot = &outputSlot{}
		slots[outputIndex] = slot
	}
	slot.kind = "function_call"
	slot.call.Type = "tool_use"
	return slot
}

func mergeOutputItem(slots map[int]*outputSlot, index int, item outputItem, done bool) {
	switch item.Type {
	case "reasoning":
		block := reasoningPart(slots, index)
		if done {
			if item.EncryptedContent != "" && len(item.raw) != 0 {
				block.Signature = string(item.raw)
			}
			var parts []string
			for _, summary := range item.Summary {
				if summary.Text != "" {
					parts = append(parts, summary.Text)
				}
			}
			if len(parts) == 0 {
				for _, content := range item.Content {
					if content.Text != "" {
						parts = append(parts, content.Text)
					}
				}
			}
			if len(parts) != 0 {
				block.Text = strings.Join(parts, "\n\n")
			}
		}
	case "message":
		for contentIndex, content := range item.Content {
			if content.Type == "output_text" {
				block := textPart(slots, index, contentIndex)
				if done && block.Text == "" {
					block.Text = content.Text
				}
			}
		}
	case "function_call":
		slot := functionSlot(slots, index)
		slot.call.ID = item.CallID
		slot.call.Name = item.Name
		slot.call.Signature = item.ID
		if done && item.Arguments != "" {
			slot.args.Reset()
			slot.args.WriteString(item.Arguments)
		}
	case "function_call_output":
		slot := slots[index]
		if slot == nil {
			slot = &outputSlot{}
			slots[index] = slot
		}
		slot.kind = "function_call_output"
		slot.call = model.Block{Type: "tool_result", ToolUseID: item.CallID, Text: item.Output}
	}
}

func applyResponse(result *model.Response, slots map[int]*outputSlot, response responseData) {
	result.CacheReadTokens = response.Usage.InputTokensDetails.CachedTokens
	result.CacheWriteTokens = response.Usage.InputTokensDetails.CacheWriteTokens
	result.InputTokens = response.Usage.InputTokens - result.CacheReadTokens - result.CacheWriteTokens
	if result.InputTokens < 0 {
		result.InputTokens = 0
	}
	result.OutputTokens = response.Usage.OutputTokens
	result.ReasoningTokens = response.Usage.OutputTokensDetails.ReasoningTokens
	for index, item := range response.Output {
		mergeOutputItem(slots, index, item, true)
	}
}

func hasFunctionCall(slots map[int]*outputSlot) bool {
	for _, slot := range slots {
		if slot.kind == "function_call" {
			return true
		}
	}
	return false
}

func flattenSlots(slots map[int]*outputSlot) []model.Block {
	indices := make([]int, 0, len(slots))
	for index := range slots {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	var content []model.Block
	for _, index := range indices {
		slot := slots[index]
		switch slot.kind {
		case "reasoning":
			if slot.thinking.Text != "" {
				content = append(content, slot.thinking)
			}
		case "message":
			partIndices := make([]int, 0, len(slot.parts))
			for partIndex := range slot.parts {
				partIndices = append(partIndices, partIndex)
			}
			sort.Ints(partIndices)
			for _, partIndex := range partIndices {
				content = append(content, *slot.parts[partIndex])
			}
		case "function_call":
			if slot.args.Len() != 0 {
				slot.call.Arguments = json.RawMessage(append([]byte(nil), slot.args.Bytes()...))
			}
			content = append(content, slot.call)
		case "function_call_output":
			content = append(content, slot.call)
		}
	}
	return content
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
		return fmt.Errorf("openai: read SSE stream: %w", err)
	}
	stop, err := dispatch()
	if err != nil {
		return err
	}
	if stop {
		return nil
	}
	return errors.New("openai: SSE stream ended before a completion event")
}

func httpStatusError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	message := strings.TrimSpace(string(body))
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
		if envelope.Error.Code != "" {
			message = envelope.Error.Code + ": " + message
		}
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return &model.ProviderError{
		Message:    fmt.Sprintf("openai: HTTP %s: %s", response.Status, message),
		StatusCode: response.StatusCode, Code: envelope.Error.Code,
		RetryAfter: model.ParseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	}
}

func apiError(code, message string) error {
	if message == "" {
		message = "streaming API error"
	}
	text := "openai: " + message
	if code != "" {
		text = fmt.Sprintf("openai: %s: %s", code, message)
	}
	return &model.ProviderError{Message: text, Code: code}
}
