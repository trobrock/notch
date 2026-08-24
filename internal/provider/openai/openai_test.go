package openai

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
			body, err := json.Marshal(makeRequest(model.Request{Model: "gpt-test", ReasoningLevel: tc.level}))
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
			} else if !present || reasoning["effort"] != tc.wantEffort || reasoning["summary"] != "auto" {
				t.Fatalf("reasoning = %#v, want effort %q with summary", request["reasoning"], tc.wantEffort)
			}
			if _, present := request["temperature"]; present {
				t.Fatalf("temperature must be omitted: %s", body)
			}
		})
	}
}

func TestListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request = %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"data":[{"id":"gpt-5"},{"id":"text-embedding-3-small"}]}`)
	}))
	defer server.Close()
	provider := New(Config{APIKey: "secret", BaseURL: server.URL, HTTPClient: server.Client()})
	models, err := provider.(model.ModelLister).ListModels(context.Background())
	if err != nil || len(models) != 2 || models[0].ID != "gpt-5" || !models[0].Reasoning {
		t.Fatalf("models = %#v, %v", models, err)
	}
	codex := New(Config{CodexMode: true})
	if _, err := codex.(model.ModelLister).ListModels(context.Background()); err == nil {
		t.Fatal("codex listing unexpectedly succeeded")
	}
}

func TestStreamTextFunctionCallAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}

		var request struct {
			Model           string           `json:"model"`
			Instructions    string           `json:"instructions"`
			Input           []map[string]any `json:"input"`
			Tools           []map[string]any `json:"tools"`
			MaxOutputTokens int              `json:"max_output_tokens"`
			Stream          bool             `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "gpt-test" || request.Instructions != "be useful" || !request.Stream || request.MaxOutputTokens != 123 {
			t.Errorf("unexpected request metadata: %#v", request)
		}
		if len(request.Input) != 4 {
			t.Fatalf("input = %#v", request.Input)
		}
		if request.Input[0]["role"] != "user" || request.Input[1]["type"] != "function_call" || request.Input[1]["call_id"] != "call-old" {
			t.Errorf("input conversion = %#v", request.Input)
		}
		if request.Input[2]["type"] != "function_call_output" || request.Input[2]["call_id"] != "call-old" || request.Input[2]["output"] != "sunny" {
			t.Errorf("tool output conversion = %#v", request.Input[2])
		}
		if len(request.Tools) != 1 || request.Tools[0]["type"] != "function" || request.Tools[0]["name"] != "weather" {
			t.Errorf("tools = %#v", request.Tools)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg-1","content":[]}}`,
			`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello "}`,
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"world"}`,
			`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"hello world"}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"weather","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"city\":"}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"Paris\"}"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"weather","arguments":"{\"city\":\"Paris\"}"}}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":17,"output_tokens":8}}}`,
		}
		for _, data := range events {
			fmt.Fprintf(w, "event: ignored-by-json-type\ndata: %s\n\n", data)
		}
	}))
	defer server.Close()

	provider := New(Config{BaseURL: server.URL + "/", APIKey: "secret", HTTPClient: server.Client()})
	var streamed []model.StreamEvent
	response, err := provider.Stream(context.Background(), model.Request{
		Model: "gpt-test", SystemPrompt: "be useful", MaxTokens: 123,
		Messages: []model.Message{
			model.TextMessage("user", "hi"),
			{Role: "assistant", Content: []model.Block{{Type: "tool_use", ID: "call-old", Name: "weather", Arguments: json.RawMessage(`{"city":"Rome"}`)}}},
			{Role: "user", Content: []model.Block{{Type: "tool_result", ToolUseID: "call-old", Text: "sunny"}, {Type: "text", Text: "and Paris?"}}},
		},
		Tools: []model.ToolDefinition{{Name: "weather", Description: "get weather", InputSchema: map[string]any{"type": "object"}}},
	}, func(event model.StreamEvent) { streamed = append(streamed, event) })
	if err != nil {
		t.Fatal(err)
	}
	if response.InputTokens != 17 || response.OutputTokens != 8 || response.StopReason != "tool_use" {
		t.Errorf("response metadata = %#v", response)
	}
	if len(response.Content) != 2 || response.Content[0].Type != "text" || response.Content[0].Text != "hello world" {
		t.Fatalf("content = %#v", response.Content)
	}
	call := response.Content[1]
	if call.Type != "tool_use" || call.ID != "call-1" || call.Name != "weather" || string(call.Arguments) != `{"city":"Paris"}` {
		t.Errorf("function call = %#v", call)
	}
	wantEvents := []model.StreamEvent{
		{Type: "text_delta", Text: "hello "}, {Type: "text_delta", Text: "world"},
		{Type: "input_json_delta", Text: `{"city":`}, {Type: "input_json_delta", Text: `"Paris"}`},
	}
	if fmt.Sprint(streamed) != fmt.Sprint(wantEvents) {
		t.Errorf("stream events = %#v, want %#v", streamed, wantEvents)
	}
}

func TestStreamReasoningSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, data := range []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"r-1","summary":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"Checked "}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"the files."}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r-1","summary":[{"type":"summary_text","text":"Checked the files."}]}}`,
			`{"type":"response.completed","response":{"status":"completed"}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer server.Close()
	var events []model.StreamEvent
	response, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client()}).Stream(context.Background(), model.Request{ReasoningLevel: "medium"}, func(event model.StreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "thinking" || response.Content[0].Text != "Checked the files." {
		t.Fatalf("content = %#v", response.Content)
	}
	if got := fmt.Sprint(events); got != fmt.Sprint([]model.StreamEvent{{Type: "thinking_delta", Text: "Checked "}, {Type: "thinking_delta", Text: "the files."}}) {
		t.Fatalf("events = %#v", events)
	}
}

func TestStreamIncompleteAndErrors(t *testing.T) {
	t.Run("incomplete", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":2,\"output_tokens\":3},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"partial\"}]}]}}\n\n")
		}))
		defer server.Close()
		response, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client()}).Stream(context.Background(), model.Request{}, nil)
		if err != nil || response.StopReason != "max_tokens" || len(response.Content) != 1 || response.Content[0].Text != "partial" {
			t.Fatalf("response = %#v, err = %v", response, err)
		}
	})

	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"stream": {func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit\",\"message\":\"slow down\"}}\n\n")
		}, "rate_limit: slow down"},
		"http": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"bad_request","message":"invalid input"}}`)
		}, "bad_request: invalid input"},
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
