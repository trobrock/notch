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

func TestRequestReplaysEncryptedReasoningAndFunctionItemID(t *testing.T) {
	reasoning := `{"type":"reasoning","id":"rs-1","encrypted_content":"cipher","summary":[]}`
	body, err := json.Marshal(makeRequest(model.Request{Messages: []model.Message{
		{Role: "assistant", Content: []model.Block{
			{Type: "thinking", Text: "prior summary", Signature: reasoning},
			{Type: "tool_use", ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"id":1}`), Signature: "fc-1"},
		}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", ToolUseID: "call-1", Text: "found"}}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 3 {
		t.Fatalf("input = %#v", request.Input)
	}
	if item := request.Input[0]; item["type"] != "reasoning" || item["id"] != "rs-1" || item["encrypted_content"] != "cipher" {
		t.Errorf("reasoning replay = %#v", item)
	}
	if item := request.Input[1]; item["type"] != "function_call" || item["id"] != "fc-1" || item["call_id"] != "call-1" {
		t.Errorf("function replay = %#v", item)
	}
	if item := request.Input[2]; item["type"] != "function_call_output" || item["call_id"] != "call-1" {
		t.Errorf("tool result replay = %#v", item)
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
	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/models" || r.URL.Query().Get("client_version") != "0.0.0" {
			t.Errorf("request URL = %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer codex-secret" || r.Header.Get("ChatGPT-Account-ID") != "account-id" {
			t.Errorf("auth=%q account=%q", r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"))
		}
		fmt.Fprint(w, `{"models":[{"slug":"gpt-visible","display_name":"GPT Visible","visibility":"list","context_window":272000,"supported_reasoning_levels":[{"effort":"medium"}]},{"slug":"gpt-max-context","display_name":"","visibility":"list","max_context_window":128000,"default_reasoning_level":"low"},{"slug":"gpt-hidden","display_name":"GPT Hidden","visibility":"hide","context_window":272000}]}`)
	}))
	defer codexServer.Close()
	codex := New(Config{
		APIKey: "codex-secret", BaseURL: codexServer.URL, HTTPClient: codexServer.Client(),
		CodexMode: true, Headers: map[string]string{"ChatGPT-Account-ID": "account-id"},
	})
	models, err = codex.(model.ModelLister).ListModels(context.Background())
	if err != nil || len(models) != 2 {
		t.Fatalf("codex models = %#v, %v", models, err)
	}
	if models[0].ID != "gpt-visible" || models[0].Name != "GPT Visible" || models[0].ContextWindow != 272000 || !models[0].Reasoning {
		t.Errorf("visible model = %#v", models[0])
	}
	if models[1].ID != "gpt-max-context" || models[1].Name != "gpt-max-context" || models[1].ContextWindow != 128000 || !models[1].Reasoning {
		t.Errorf("max-context model = %#v", models[1])
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
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":17,"output_tokens":8,"input_tokens_details":{"cached_tokens":6,"cache_write_tokens":2},"output_tokens_details":{"reasoning_tokens":4}}}}`,
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
	if response.InputTokens != 9 || response.CacheReadTokens != 6 || response.CacheWriteTokens != 2 || response.OutputTokens != 8 || response.ReasoningTokens != 4 || response.TotalTokens() != 25 || response.StopReason != "tool_use" {
		t.Errorf("response metadata = %#v", response)
	}
	if len(response.Content) != 2 || response.Content[0].Type != "text" || response.Content[0].Text != "hello world" {
		t.Fatalf("content = %#v", response.Content)
	}
	call := response.Content[1]
	if call.Type != "tool_use" || call.ID != "call-1" || call.Signature != "fc-1" || call.Name != "weather" || string(call.Arguments) != `{"city":"Paris"}` {
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
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r-1","encrypted_content":"cipher","summary":[{"type":"summary_text","text":"Checked the files."}]}}`,
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
	if item, ok := replayReasoningItem(response.Content[0].Signature); !ok || !strings.Contains(string(item), `"encrypted_content":"cipher"`) {
		t.Fatalf("reasoning signature = %q", response.Content[0].Signature)
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

func TestStreamAbruptEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n")
	}))
	defer server.Close()

	_, err := New(Config{BaseURL: server.URL, HTTPClient: server.Client()}).Stream(context.Background(), model.Request{}, nil)
	if err == nil || !strings.Contains(err.Error(), "ended before a completion event") {
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

func TestMakeRequestConfiguresPromptCaching(t *testing.T) {
	key := strings.Repeat("k", 80)
	wire := makeRequest(model.Request{Model: "gpt-5.4", CacheRetention: "long", CacheKey: key})
	if len([]rune(wire.PromptCacheKey)) != 64 || wire.PromptCacheRetention != "24h" || wire.PromptCacheOptions != nil {
		t.Fatalf("long cache config = %#v", wire)
	}
	uncached := makeRequest(model.Request{Model: "gpt-5.6-sol", CacheRetention: "none", CacheKey: key})
	if uncached.PromptCacheKey != "" || uncached.PromptCacheRetention != "" || uncached.PromptCacheOptions == nil || uncached.PromptCacheOptions.Mode != "explicit" {
		t.Fatalf("disabled cache config = %#v", uncached)
	}
}

func TestCodexRequestOmitsUnsupportedPromptCacheOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if _, present := request["prompt_cache_options"]; present {
			t.Errorf("prompt_cache_options must be omitted from Codex requests: %#v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	provider := New(Config{
		BaseURL: server.URL, Endpoint: "/codex/responses", CodexMode: true,
		OfficialEndpoint: true, HTTPClient: server.Client(),
	})
	if _, err := provider.Stream(context.Background(), model.Request{
		Model: "gpt-5.6-sol", CacheRetention: "none",
	}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCustomEndpointDisablesHostedCacheFieldsAndPricing(t *testing.T) {
	custom := New(Config{BaseURL: "https://local.example"}).(*provider)
	if custom.promptCacheFields {
		t.Fatal("custom endpoint unexpectedly enables hosted cache fields")
	}
	official := New(Config{}).(*provider)
	if !official.promptCacheFields {
		t.Fatal("official endpoint disabled hosted cache fields")
	}
	wire := makeRequest(model.Request{Model: "other-model", CacheRetention: "long", CacheKey: "key"})
	if wire.PromptCacheRetention != "" {
		t.Fatalf("unsupported model retention = %q", wire.PromptCacheRetention)
	}
}

func TestStreamRefreshesExpiredTokenAndRetriesOnce(t *testing.T) {
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":"token_expired","message":"Provided authentication token is expired."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	var stales []string
	provider := New(Config{
		BaseURL: server.URL, HTTPClient: server.Client(),
		Authorize: func(_ context.Context, stale string) (string, error) {
			stales = append(stales, stale)
			if stale == "" {
				return "expired", nil
			}
			return "fresh", nil
		},
	})
	if _, err := provider.Stream(context.Background(), model.Request{ReasoningLevel: "medium"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(authorizations) != 2 || authorizations[0] != "Bearer expired" || authorizations[1] != "Bearer fresh" {
		t.Fatalf("authorization headers = %#v", authorizations)
	}
	if len(stales) != 2 || stales[0] != "" || stales[1] != "expired" {
		t.Fatalf("authorize calls = %#v", stales)
	}
}

func TestStreamUnauthorizedReturnsProviderErrorWhenRefreshFails(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"token_expired","message":"Provided authentication token is expired."}}`)
	}))
	defer server.Close()

	provider := New(Config{
		BaseURL: server.URL, HTTPClient: server.Client(),
		Authorize: func(_ context.Context, stale string) (string, error) {
			if stale == "" {
				return "expired", nil
			}
			return "", errors.New("refresh openai-codex login: no network")
		},
	})
	_, err := provider.Stream(context.Background(), model.Request{ReasoningLevel: "medium"}, nil)
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(providerErr.Message, "token refresh failed") {
		t.Fatalf("message = %q", providerErr.Message)
	}
	if requests != 1 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestStreamUnauthorizedWithoutAuthorizeDoesNotRetry(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"invalid_api_key","message":"bad key"}}`)
	}))
	defer server.Close()

	provider := New(Config{APIKey: "static", BaseURL: server.URL, HTTPClient: server.Client()})
	if _, err := provider.Stream(context.Background(), model.Request{ReasoningLevel: "medium"}, nil); err == nil {
		t.Fatal("unauthorized stream unexpectedly succeeded")
	}
	if requests != 1 {
		t.Fatalf("requests = %d", requests)
	}
}
