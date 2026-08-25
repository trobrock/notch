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
