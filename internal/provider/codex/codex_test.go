package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/trobrock/notch/internal/model"
)

func TestStreamConfiguresCodexRequestAndParsesToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/codex/responses" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		wantHeaders := map[string]string{
			"Authorization":      "Bearer access-token",
			"ChatGPT-Account-ID": "account-id",
			"OpenAI-Beta":        "responses=experimental",
			"originator":         "notch",
			"User-Agent":         "notch",
		}
		for name, want := range wantHeaders {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"store", "stream", "instructions", "input", "tools", "text", "include", "tool_choice", "parallel_tool_calls"} {
			if _, ok := body[name]; !ok {
				t.Errorf("request body does not contain %q: %#v", name, body)
			}
		}
		if body["store"] != false || body["stream"] != true || body["tool_choice"] != "auto" || body["parallel_tool_calls"] != true {
			t.Errorf("Codex options = %#v", body)
		}
		if _, ok := body["max_output_tokens"]; ok {
			t.Errorf("max_output_tokens must be omitted: %#v", body)
		}
		if text, ok := body["text"].(map[string]any); !ok || text["verbosity"] != "low" {
			t.Errorf("text = %#v", body["text"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_item.added\ndata: {\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n")
		fmt.Fprint(w, "event: response.function_call_arguments.delta\ndata: {\"output_index\":0,\"delta\":\"{\\\"id\\\":1}\"}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		AccessToken: "access-token", AccountID: "account-id",
		BaseURL: server.URL, HTTPClient: server.Client(),
	})
	var events []model.StreamEvent
	response, err := provider.Stream(context.Background(), model.Request{
		Model: "codex-test", MaxTokens: 999,
		Tools: []model.ToolDefinition{{Name: "lookup", InputSchema: map[string]any{"type": "object"}}},
	}, func(event model.StreamEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "tool_use" || response.InputTokens != 4 || response.OutputTokens != 2 {
		t.Errorf("response = %#v", response)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "tool_use" || response.Content[0].ID != "call-1" || response.Content[0].Name != "lookup" || string(response.Content[0].Arguments) != `{"id":1}` {
		t.Errorf("tool call = %#v", response.Content)
	}
	if len(events) != 1 || events[0].Type != "input_json_delta" || events[0].Text != `{"id":1}` {
		t.Errorf("events = %#v", events)
	}
}
