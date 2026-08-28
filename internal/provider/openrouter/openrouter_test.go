package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/model"
)

func TestRequestBodyReasoningLevels(t *testing.T) {
	for _, tc := range []struct {
		level      string
		wantEffort string
	}{
		{level: "off"},
		{level: "medium", wantEffort: "medium"},
		{level: "xhigh", wantEffort: "xhigh"},
		{level: "invalid"},
	} {
		t.Run(tc.level, func(t *testing.T) {
			body, err := json.Marshal(makeRequest(model.Request{Model: "test/model", ReasoningLevel: tc.level}))
			if err != nil {
				t.Fatal(err)
			}
			var request map[string]any
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			reasoning, present := request["reasoning"].(map[string]any)
			if tc.wantEffort == "" {
				if present {
					t.Fatalf("reasoning must be omitted: %s", body)
				}
			} else if !present || reasoning["effort"] != tc.wantEffort {
				t.Fatalf("reasoning = %#v, want effort %q", request["reasoning"], tc.wantEffort)
			}
			if _, present := request["temperature"]; present {
				t.Fatalf("temperature must be omitted: %s", body)
			}
		})
	}
}

func TestListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request = %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"data":[{"id":"vendor/model","name":"Model","context_length":123456,"supported_parameters":["reasoning"]}]}`)
	}))
	defer server.Close()
	provider := New(Config{APIKey: "secret", BaseURL: server.URL, HTTPClient: server.Client()})
	models, err := provider.(model.ModelLister).ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "vendor/model" || models[0].ContextWindow != 123456 || !models[0].Reasoning {
		t.Fatalf("models = %#v, %v", models, err)
	}
}

func TestStreamRequestTextToolsAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/chat/completions" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		for header, want := range map[string]string{
			"Authorization": "Bearer secret", "Accept": "text/event-stream",
			"X-Title": "Notch tests", "HTTP-Referer": "https://example.test/app",
		} {
			if got := r.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		var request struct {
			Model         string `json:"model"`
			MaxTokens     int    `json:"max_tokens"`
			Stream        bool   `json:"stream"`
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name        string         `json:"name"`
					Description string         `json:"description"`
					Parameters  map[string]any `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "openai/test" || request.MaxTokens != 321 || !request.Stream || !request.StreamOptions.IncludeUsage {
			t.Errorf("request metadata = %#v", request)
		}
		if len(request.Messages) != 5 {
			t.Fatalf("messages = %#v", request.Messages)
		}
		if request.Messages[0].Role != "system" || request.Messages[0].Content != "be useful" ||
			request.Messages[1].Role != "user" || request.Messages[1].Content != "hello" {
			t.Errorf("system/user messages = %#v", request.Messages[:2])
		}
		assistant := request.Messages[2]
		if assistant.Role != "assistant" || assistant.Content != "checking" || len(assistant.ToolCalls) != 1 ||
			assistant.ToolCalls[0].ID != "old-call" || assistant.ToolCalls[0].Function.Name != "weather" ||
			assistant.ToolCalls[0].Function.Arguments != `{"city":"Rome"}` {
			t.Errorf("assistant message = %#v", assistant)
		}
		if request.Messages[3].Role != "tool" || request.Messages[3].ToolCallID != "old-call" || request.Messages[3].Content != "sunny" ||
			request.Messages[4].Role != "user" || request.Messages[4].Content != "now Paris" {
			t.Errorf("tool messages = %#v", request.Messages[3:])
		}
		if len(request.Tools) != 1 || request.Tools[0].Type != "function" || request.Tools[0].Function.Name != "weather" ||
			request.Tools[0].Function.Description != "look up weather" || request.Tools[0].Function.Parameters["type"] != "object" {
			t.Errorf("tools = %#v", request.Tools)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"gen","choices":[{"index":0,"delta":{"role":"assistant","content":"I'll "},"finish_reason":null}]}`,
			`{"id":"gen","choices":[{"index":0,"delta":{"content":"check.","tool_calls":[{"index":1,"id":"call-b","type":"function","function":{"name":"time","arguments":"{\"zone\":"}}]},"finish_reason":null}]}`,
			`{"id":"gen","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"weather","arguments":"{\"city\":"}},{"index":1,"function":{"arguments":"\"UTC\"}"}}]},"finish_reason":null}]}`,
			`{"id":"gen","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"id":"gen","choices":[],"usage":{"prompt_tokens":25,"completion_tokens":11,"total_tokens":36,"cost":0.0042,"prompt_tokens_details":{"cached_tokens":7,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":3}}}`,
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		APIKey: "secret", BaseURL: server.URL + "/api/v1/", HTTPClient: server.Client(),
		AppName: "Notch tests", Referer: "https://example.test/app",
	})
	var events []model.StreamEvent
	response, err := provider.Stream(context.Background(), model.Request{
		Model: "openai/test", SystemPrompt: "be useful", MaxTokens: 321,
		Messages: []model.Message{
			model.TextMessage("user", "hello"),
			{Role: "assistant", Content: []model.Block{
				{Type: "text", Text: "checking"},
				{Type: "tool_use", ID: "old-call", Name: "weather", Arguments: json.RawMessage(`{"city":"Rome"}`)},
			}},
			{Role: "user", Content: []model.Block{
				{Type: "tool_result", ToolUseID: "old-call", Text: "sunny"},
				{Type: "text", Text: "now Paris"},
			}},
		},
		Tools: []model.ToolDefinition{{Name: "weather", Description: "look up weather", InputSchema: map[string]any{"type": "object"}}},
	}, func(event model.StreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "tool_use" || response.InputTokens != 16 || response.CacheReadTokens != 7 || response.CacheWriteTokens != 2 || response.OutputTokens != 11 || response.ReasoningTokens != 3 || response.CostUSD == nil || *response.CostUSD != 0.0042 || response.TotalTokens() != 36 {
		t.Errorf("response metadata = %#v", response)
	}
	if len(response.Content) != 3 || response.Content[0].Type != "text" || response.Content[0].Text != "I'll check." {
		t.Fatalf("content = %#v", response.Content)
	}
	if got := response.Content[1]; got.Type != "tool_use" || got.ID != "call-a" || got.Name != "weather" || string(got.Arguments) != `{"city":"Paris"}` {
		t.Errorf("first call = %#v", got)
	}
	if got := response.Content[2]; got.ID != "call-b" || got.Name != "time" || string(got.Arguments) != `{"zone":"UTC"}` {
		t.Errorf("second call = %#v", got)
	}
	wantEvents := []model.StreamEvent{
		{Type: "text_delta", Text: "I'll "},
		{Type: "text_delta", Text: "check."},
		{Type: "input_json_delta", Text: `{"zone":`},
		{Type: "input_json_delta", Text: `{"city":`},
		{Type: "input_json_delta", Text: `"UTC"}`},
		{Type: "input_json_delta", Text: `"Paris"}`},
	}
	if fmt.Sprint(events) != fmt.Sprint(wantEvents) {
		t.Errorf("events = %#v, want %#v", events, wantEvents)
	}
}

func TestStreamReasoningSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"Checked \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"the files.\",\"content\":\"Done\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	var events []model.StreamEvent
	response, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client()}).Stream(context.Background(), model.Request{}, func(event model.StreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 2 || response.Content[0].Type != "thinking" || response.Content[0].Text != "Checked the files." || response.Content[1].Text != "Done" {
		t.Fatalf("content = %#v", response.Content)
	}
	if len(events) != 3 || events[0].Type != "thinking_delta" || events[1].Type != "thinking_delta" || events[2].Type != "text_delta" {
		t.Fatalf("events = %#v", events)
	}
}

func TestStreamFinishReasonsAndErrors(t *testing.T) {
	t.Run("length", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
		}))
		defer server.Close()
		response, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client()}).Stream(context.Background(), model.Request{}, nil)
		if err != nil || response.StopReason != "max_tokens" || len(response.Content) != 1 || response.Content[0].Text != "partial" {
			t.Fatalf("response = %#v, error = %v", response, err)
		}
	})

	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"stream": {func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "data: {\"error\":{\"code\":429,\"message\":\"rate limited\"}}\n\n")
		}, "429: rate limited"},
		"http": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"bad model"}}`)
		}, "400: bad model"},
		"invalid event": {func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "data: not-json\n\n")
		}, "decode SSE"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client()}).Stream(context.Background(), model.Request{}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestStreamAbruptEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer server.Close()

	_, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client()}).Stream(context.Background(), model.Request{}, nil)
	if err == nil || !strings.Contains(err.Error(), "ended before [DONE]") {
		t.Fatalf("error = %v, want incomplete stream error", err)
	}
}

func TestStreamCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": waiting\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client()}).Stream(ctx, model.Request{}, nil)
		done <- err
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream did not return after cancellation")
	}
}
