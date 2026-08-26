package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/trobrock/notch/internal/extension"
)

func TestHTTPServerHandshakeAndTool(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		mu.Lock()
		methods = append(methods, request.Method)
		mu.Unlock()
		if request.Method != "initialize" && r.Header.Get(sessionHeader) != "test-session" {
			t.Errorf("missing session header on %s", request.Method)
		}
		switch request.Method {
		case "initialize":
			w.Header().Set(sessionHeader, "test-session")
			writeRPCResult(t, w, request.ID, map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(t, w, request.ID, map[string]any{"tools": []any{map[string]any{
				"name": "echo", "description": "echo input", "inputSchema": map[string]any{"type": "object"},
			}}})
		case "tools/call":
			writeRPCResult(t, w, request.ID, map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "json", "json": map[string]any{"ok": true}},
			}})
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	registry := extension.NewRegistry()
	manager, err := ConnectConfigured(context.Background(), Config{MCPServers: map[string]ServerConfig{
		"demo": {URL: server.URL, Headers: map[string]string{"X-Test": "yes"}},
	}}, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	tool, ok := registry.Tool("mcp__demo__echo")
	if !ok {
		t.Fatal("namespaced tool was not registered")
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"value":1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "hello\n{\"ok\":true}" {
		t.Fatalf("unexpected result content %q", result.Content)
	}
	mu.Lock()
	gotMethods := strings.Join(methods, ",")
	mu.Unlock()
	if gotMethods != "initialize,notifications/initialized,tools/list,tools/call" {
		t.Fatalf("unexpected method sequence: %s", gotMethods)
	}
}

func writeRPCResult(t *testing.T, w http.ResponseWriter, id int64, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestManagerCloseUnregistersTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		switch request.Method {
		case "initialize":
			writeRPCResult(t, w, request.ID, map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(t, w, request.ID, map[string]any{"tools": []any{map[string]any{"name": "owned"}}})
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	registry := extension.NewRegistry()
	manager, err := ConnectConfigured(context.Background(), Config{MCPServers: map[string]ServerConfig{"one": {URL: server.URL}}}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Tool("mcp__one__owned"); !ok {
		t.Fatal("tool was not registered")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := registry.Tool("mcp__one__owned"); ok {
		t.Fatal("tool remained registered after manager Close")
	}
}

func TestSecondServerFailureLeavesNoRegistrations(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode first request: %v", err)
			return
		}
		switch request.Method {
		case "initialize":
			writeRPCResult(t, w, request.ID, map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(t, w, request.ID, map[string]any{"tools": []any{map[string]any{"name": "first"}}})
		}
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID int64 `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		http.Error(w, "server two failed", http.StatusInternalServerError)
	}))
	defer second.Close()

	registry := extension.NewRegistry()
	manager, err := ConnectConfigured(context.Background(), Config{MCPServers: map[string]ServerConfig{
		"one": {URL: first.URL},
		"two": {URL: second.URL},
	}}, registry)
	if err == nil || manager != nil {
		t.Fatalf("ConnectConfigured manager=%v err=%v", manager, err)
	}
	if _, ok := registry.Tool("mcp__one__first"); ok {
		t.Fatal("first server tool remained after second server failed")
	}
	if len(registry.Tools()) != 0 {
		t.Fatalf("failed startup left tools: %#v", registry.Tools())
	}
}

func TestHTTPOAuthRefreshesAndRetriesUnauthorized(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		count := len(tokens)
		mu.Unlock()
		if count == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		writeRPCResult(t, w, request.ID, map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}})
	}))
	defer server.Close()
	var calls []bool
	currentToken := "old-token"
	client := newHTTPClient(ServerConfig{URL: server.URL}, func(_ context.Context, refresh bool) (string, error) {
		calls = append(calls, refresh)
		if refresh {
			currentToken = "new-token"
		}
		return currentToken, nil
	})
	if err := initialize(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(calls) != "[false true false]" {
		t.Fatalf("authorization calls = %v", calls)
	}
	mu.Lock()
	gotTokens := strings.Join(tokens, ",")
	mu.Unlock()
	if gotTokens != "Bearer old-token,Bearer new-token,Bearer new-token" {
		t.Fatalf("tokens = %s", gotTokens)
	}
}

func TestReadSSEResponse(t *testing.T) {
	stream := strings.NewReader("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\ndata: \"result\":{\"answer\":42}}\n\n")
	response, err := readSSEResponse(stream, 7)
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Result) != `{"answer":42}` {
		t.Fatalf("unexpected result: %s", response.Result)
	}
}

func TestLoadConfigEnabled(t *testing.T) {
	path := t.TempDir() + "/mcp.json"
	data := `{"mcpServers":{"off":{"command":"nope","enabled":false},"default":{"url":"http://example.test"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPServers["off"].isEnabled() {
		t.Fatal("explicitly disabled server is enabled")
	}
	if !cfg.MCPServers["default"].isEnabled() {
		t.Fatal("server without enabled field is disabled")
	}
	if got := fmt.Sprint(cfg.MCPServers["default"].URL); got != "http://example.test" {
		t.Fatalf("URL = %q", got)
	}
}

func TestLoadConfigExpandsEnvironment(t *testing.T) {
	t.Setenv("MCP_TEST_TOKEN", "secret-value")
	t.Setenv("MCP_TEST_HOST", "example.test")
	path := t.TempDir() + "/mcp.json"
	data := `{"mcpServers":{"stdio":{"command":"server","env":{"TOKEN":"${MCP_TEST_TOKEN}","LABEL":"prefix-${MCP_TEST_HOST}-suffix","LITERAL":"$${MCP_TEST_TOKEN}"}},"http":{"url":"https://example.test","headers":{"Authorization":"Bearer ${MCP_TEST_TOKEN}"}}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCPServers["stdio"].Env["TOKEN"]; got != "secret-value" {
		t.Fatalf("TOKEN = %q", got)
	}
	if got := cfg.MCPServers["stdio"].Env["LABEL"]; got != "prefix-example.test-suffix" {
		t.Fatalf("LABEL = %q", got)
	}
	if got := cfg.MCPServers["stdio"].Env["LITERAL"]; got != "${MCP_TEST_TOKEN}" {
		t.Fatalf("LITERAL = %q", got)
	}
	if got := cfg.MCPServers["http"].Headers["Authorization"]; got != "Bearer secret-value" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestLoadConfigSkipsEnvironmentExpansionForDisabledServer(t *testing.T) {
	const name = "NOTCH_MCP_TEST_DISABLED_MISSING"
	_ = os.Unsetenv(name)
	path := t.TempDir() + "/mcp.json"
	data := `{"mcpServers":{"off":{"command":"server","enabled":false,"env":{"TOKEN":"${` + name + `}"}}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig rejected a disabled server: %v", err)
	}
}

func TestLoadConfigRejectsMissingEnvironment(t *testing.T) {
	const name = "NOTCH_MCP_TEST_MISSING"
	_ = os.Unsetenv(name)
	path := t.TempDir() + "/mcp.json"
	data := `{"mcpServers":{"grafana":{"command":"server","env":{"TOKEN":"${` + name + `}"}}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig succeeded with an unset environment variable")
	}
	if got := err.Error(); !strings.Contains(got, `server "grafana" env: "TOKEN": environment variable "`+name+`" is not set`) {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestExpandEnvironmentRejectsInvalidReferences(t *testing.T) {
	for _, value := range []string{"${}", "${1TOKEN}", "${TOKEN-NAME}", "${TOKEN"} {
		t.Run(value, func(t *testing.T) {
			if _, err := expandEnvironment(value, os.LookupEnv); err == nil {
				t.Fatalf("expandEnvironment(%q) succeeded", value)
			}
		})
	}
}
