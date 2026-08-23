package anthropic

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

func TestStreamTextToolUseAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "secret" {
			t.Errorf("request path/header = %s/%q", r.URL.Path, r.Header.Get("x-api-key"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["stream"] != true || request["system"] != "be useful" {
			t.Errorf("unexpected request: %#v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive\n\n")
		events := []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool-1","name":"weather","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`,
		}
		for _, data := range events {
			fmt.Fprintf(w, "event: ignored-by-type\ndata: %s\n\n", data)
		}
	}))
	defer server.Close()

	provider := New(Config{BaseURL: server.URL, APIKey: "secret", HTTPClient: server.Client()})
	var streamed []model.StreamEvent
	response, err := provider.Stream(context.Background(), model.Request{
		Model: "claude-test", SystemPrompt: "be useful", Messages: []model.Message{model.TextMessage("user", "hi")},
	}, func(event model.StreamEvent) { streamed = append(streamed, event) })
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "tool_use" || response.InputTokens != 12 || response.OutputTokens != 9 {
		t.Errorf("unexpected metadata: %#v", response)
	}
	if len(response.Content) != 2 || response.Content[0].Text != "hello world" {
		t.Fatalf("unexpected content: %#v", response.Content)
	}
	tool := response.Content[1]
	if tool.Type != "tool_use" || tool.ID != "tool-1" || tool.Name != "weather" || string(tool.Arguments) != `{"city":"Paris"}` {
		t.Errorf("unexpected tool block: %#v", tool)
	}
	if len(streamed) != 4 || streamed[2].Type != "input_json_delta" {
		t.Errorf("unexpected stream events: %#v", streamed)
	}
}

func TestStreamOAuthHeadersBodyAndToolNameRoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Errorf("x-api-key must be omitted in OAuth mode, got %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != oauthBeta {
			t.Errorf("anthropic-beta = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != oauthUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		if got := r.Header.Get("x-app"); got != "cli" {
			t.Errorf("x-app = %q", got)
		}

		var request struct {
			System   []map[string]string    `json:"system"`
			Tools    []model.ToolDefinition `json:"tools"`
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.System) != 2 || request.System[0]["type"] != "text" || request.System[0]["text"] != claudeCodeSystemBlock || request.System[1]["text"] != "project instructions" {
			t.Errorf("unexpected OAuth system blocks: %#v", request.System)
		}
		if len(request.Tools) != 2 || request.Tools[0].Name != "Read" || request.Tools[1].Name != "Glob" {
			t.Errorf("unexpected canonical tool definitions: %#v", request.Tools)
		}
		if len(request.Messages) != 1 || len(request.Messages[0].Content) != 1 || request.Messages[0].Content[0].Name != "Read" {
			t.Errorf("unexpected canonical history: %#v", request.Messages)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"gLoB\",\"input\":{\"pattern\":\"*.go\"}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	response, err := New(Config{
		APIKey: "must-not-be-sent", OAuthToken: "oauth-secret", OAuthMode: true,
		BaseURL: server.URL, HTTPClient: server.Client(),
	}).Stream(context.Background(), model.Request{
		SystemPrompt: "project instructions",
		Tools:        []model.ToolDefinition{{Name: "read"}, {Name: "find"}},
		Messages: []model.Message{{Role: "assistant", Content: []model.Block{{
			Type: "tool_use", ID: "old-tool", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`),
		}}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 1 || response.Content[0].Name != "find" {
		t.Fatalf("streamed tool name was not mapped to registered name: %#v", response.Content)
	}
}

func TestStreamAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}

`)
	}))
	defer server.Close()

	_, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client()}).Stream(context.Background(), model.Request{}, nil)
	if err == nil || !strings.Contains(err.Error(), "overloaded_error: busy") {
		t.Fatalf("unexpected error: %v", err)
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
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stream did not return after cancellation")
	}
}
