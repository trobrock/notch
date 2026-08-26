// Package openrouter implements OpenRouter's OpenAI-compatible Chat Completions API.
package openrouter

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

const defaultBaseURL = "https://openrouter.ai/api/v1"

// Config configures an OpenRouter provider.
type Config struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
	AppName    string
	Referer    string
}

type provider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	appName    string
	referer    string
}

// New returns a provider backed by OpenRouter's Chat Completions API.
func New(cfg Config) model.Provider {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &provider{
		apiKey: cfg.APIKey, baseURL: baseURL, httpClient: client,
		appName: cfg.AppName, referer: cfg.Referer,
	}
}

type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Reasoning     *wireReasoning `json:"reasoning,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions streamOptions  `json:"stream_options"`
}

type wireReasoning struct {
	Effort string `json:"effort"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireCallFunction `json:"function"`
}

type wireCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
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
	return &wireReasoning{Effort: level}
}

func makeRequest(req model.Request) wireRequest {
	messages := make([]wireMessage, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, wireMessage{Role: "system", Content: req.SystemPrompt})
	}
	for _, message := range req.Messages {
		if message.Role == "assistant" {
			out := wireMessage{Role: "assistant"}
			hasOutput := false
			for _, block := range message.Content {
				switch block.Type {
				case "text":
					hasOutput = true
					out.Content += block.Text
				case "tool_use", "function_call":
					hasOutput = true
					arguments := string(block.Arguments)
					if arguments == "" {
						arguments = "{}"
					}
					out.ToolCalls = append(out.ToolCalls, wireToolCall{
						ID: block.ID, Type: "function",
						Function: wireCallFunction{Name: block.Name, Arguments: arguments},
					})
				case "tool_result", "function_call_output":
					// Tool outputs cannot be represented inside an assistant message.
					if hasOutput {
						messages = append(messages, out)
						out = wireMessage{Role: "assistant"}
						hasOutput = false
					}
					messages = append(messages, wireMessage{Role: "tool", Content: block.Text, ToolCallID: block.ToolUseID})
				}
			}
			if hasOutput || len(message.Content) == 0 {
				messages = append(messages, out)
			}
			continue
		}

		// Tool results are their own role=tool messages. Flush text around them so
		// the order of mixed generic blocks is retained.
		var text strings.Builder
		hasText := false
		flushText := func() {
			if !hasText {
				return
			}
			messages = append(messages, wireMessage{Role: message.Role, Content: text.String()})
			text.Reset()
			hasText = false
		}
		for _, block := range message.Content {
			switch block.Type {
			case "text":
				hasText = true
				text.WriteString(block.Text)
			case "tool_result", "function_call_output":
				flushText()
				messages = append(messages, wireMessage{Role: "tool", Content: block.Text, ToolCallID: block.ToolUseID})
			}
		}
		flushText()
		if len(message.Content) == 0 {
			messages = append(messages, wireMessage{Role: message.Role})
		}
	}

	tools := make([]wireTool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		tools = append(tools, wireTool{Type: "function", Function: wireToolFunction{
			Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema,
		}})
	}
	return wireRequest{
		Model: req.Model, Messages: messages, Tools: tools, MaxTokens: req.MaxTokens,
		Reasoning: reasoningForLevel(req.ReasoningLevel), Stream: true, StreamOptions: streamOptions{IncludeUsage: true},
	}
}

type streamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamEnvelope struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string           `json:"content"`
			Reasoning        string           `json:"reasoning"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []streamToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *wireError `json:"error"`
}

type wireError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Type    string          `json:"type"`
}

type accumulatedCall struct {
	id        strings.Builder
	name      strings.Builder
	arguments strings.Builder
}

func (p *provider) ListModels(ctx context.Context) ([]model.ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("openrouter: create model-list request: %w", err)
	}
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	if p.appName != "" {
		httpReq.Header.Set("X-Title", p.appName)
	}
	if p.referer != "" {
		httpReq.Header.Set("HTTP-Referer", p.referer)
	}
	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openrouter: list models: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, httpStatusError(httpResp)
	}
	var envelope struct {
		Data []struct {
			ID                  string   `json:"id"`
			Name                string   `json:"name"`
			ContextLength       int      `json:"context_length"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(httpResp.Body, 32<<20)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("openrouter: decode model list: %w", err)
	}
	models := make([]model.ModelInfo, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		reasoning := false
		for _, parameter := range item.SupportedParameters {
			if parameter == "reasoning" || parameter == "include_reasoning" {
				reasoning = true
				break
			}
		}
		models = append(models, model.ModelInfo{ID: item.ID, Name: item.Name, ContextWindow: item.ContextLength, Reasoning: reasoning})
	}
	return models, nil
}

func (p *provider) Stream(ctx context.Context, req model.Request, onEvent func(model.StreamEvent)) (model.Response, error) {
	if !validReasoningLevel(req.ReasoningLevel) {
		return model.Response{}, fmt.Errorf("openrouter: invalid reasoning level %q", req.ReasoningLevel)
	}
	body, err := json.Marshal(makeRequest(req))
	if err != nil {
		return model.Response{}, fmt.Errorf("openrouter: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return model.Response{}, fmt.Errorf("openrouter: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	if p.appName != "" {
		httpReq.Header.Set("X-Title", p.appName)
	}
	if p.referer != "" {
		httpReq.Header.Set("HTTP-Referer", p.referer)
	}

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return model.Response{}, ctxErr
		}
		return model.Response{}, fmt.Errorf("openrouter: send request: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return model.Response{}, httpStatusError(httpResp)
	}

	var result model.Response
	var thinking, text strings.Builder
	calls := make(map[int]*accumulatedCall)
	err = readSSE(httpResp.Body, func(event, data string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if data == "[DONE]" {
			return true, nil
		}
		var envelope streamEnvelope
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			return false, fmt.Errorf("openrouter: decode SSE event %q: %w", event, err)
		}
		if envelope.Error != nil {
			return false, apiError(envelope.Error)
		}
		if envelope.Usage.PromptTokens != 0 {
			result.InputTokens = envelope.Usage.PromptTokens
		}
		if envelope.Usage.CompletionTokens != 0 {
			result.OutputTokens = envelope.Usage.CompletionTokens
		}
		for _, choice := range envelope.Choices {
			// Chat Completions defaults to one choice. Ignore any unsolicited
			// alternatives rather than combining independent answers.
			if choice.Index != 0 {
				continue
			}
			reasoning := choice.Delta.Reasoning
			if reasoning == "" {
				reasoning = choice.Delta.ReasoningContent
			}
			if reasoning != "" {
				thinking.WriteString(reasoning)
				if onEvent != nil {
					onEvent(model.StreamEvent{Type: "thinking_delta", Text: reasoning})
				}
			}
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				if onEvent != nil {
					onEvent(model.StreamEvent{Type: "text_delta", Text: choice.Delta.Content})
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := calls[delta.Index]
				if call == nil {
					call = &accumulatedCall{}
					calls[delta.Index] = call
				}
				call.id.WriteString(delta.ID)
				call.name.WriteString(delta.Function.Name)
				call.arguments.WriteString(delta.Function.Arguments)
				if delta.Function.Arguments != "" && onEvent != nil {
					onEvent(model.StreamEvent{Type: "input_json_delta", Text: delta.Function.Arguments})
				}
			}
			if choice.FinishReason != nil {
				result.StopReason = mapFinishReason(*choice.FinishReason)
			}
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

	if thinking.Len() != 0 {
		result.Content = append(result.Content, model.Block{Type: "thinking", Text: thinking.String()})
	}
	if text.Len() != 0 {
		result.Content = append(result.Content, model.Block{Type: "text", Text: text.String()})
	}
	indices := make([]int, 0, len(calls))
	for index := range calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		call := calls[index]
		arguments := call.arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		result.Content = append(result.Content, model.Block{
			Type: "tool_use", ID: call.id.String(), Name: call.name.String(),
			Arguments: json.RawMessage(arguments),
		})
	}
	if result.StopReason == "" && len(calls) != 0 {
		result.StopReason = "tool_use"
	}
	return result, nil
}

func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return reason
	}
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
		return fmt.Errorf("openrouter: read SSE stream: %w", err)
	}
	stop, err := dispatch()
	if err != nil {
		return err
	}
	if stop {
		return nil
	}
	return errors.New("openrouter: SSE stream ended before [DONE]")
}

func httpStatusError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	message := strings.TrimSpace(string(body))
	var envelope struct {
		Error *wireError `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil && envelope.Error.Message != "" {
		message = formatAPIError(envelope.Error)
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	code := ""
	if envelope.Error != nil {
		code = strings.Trim(strings.TrimSpace(string(envelope.Error.Code)), `"`)
		if code == "" || code == "null" {
			code = envelope.Error.Type
		}
	}
	return &model.ProviderError{
		Message:    fmt.Sprintf("openrouter: HTTP %s: %s", response.Status, message),
		StatusCode: response.StatusCode, Code: code,
		RetryAfter: model.ParseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	}
}

func apiError(err *wireError) error {
	message := formatAPIError(err)
	if message == "" {
		message = "streaming API error"
	}
	code := ""
	if err != nil {
		code = strings.Trim(strings.TrimSpace(string(err.Code)), `"`)
		if code == "" || code == "null" {
			code = err.Type
		}
	}
	return &model.ProviderError{Message: "openrouter: " + message, Code: code}
}

func formatAPIError(err *wireError) string {
	if err == nil {
		return ""
	}
	kind := strings.TrimSpace(err.Type)
	if code := strings.Trim(strings.TrimSpace(string(err.Code)), `"`); code != "" && code != "null" {
		kind = code
	}
	if kind != "" && err.Message != "" {
		return kind + ": " + err.Message
	}
	if err.Message != "" {
		return err.Message
	}
	return kind
}
