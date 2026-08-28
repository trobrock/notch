package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/extension"
)

// BenchmarkConnectConfiguredThreeServers models startup with several MCP
// servers whose handshakes each have a small independent delay.
func BenchmarkConnectConfiguredThreeServers(b *testing.B) {
	servers := make([]*httptest.Server, 0, 3)
	configured := make(map[string]ServerConfig, 3)
	for i := range 3 {
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
				b.Errorf("decode request: %v", err)
				return
			}
			switch request.Method {
			case "initialize":
				time.Sleep(10 * time.Millisecond)
				writeBenchmarkResult(w, request.ID, map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}})
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			case "tools/list":
				writeBenchmarkResult(w, request.ID, map[string]any{"tools": []any{}})
			default:
				http.Error(w, "unexpected method", http.StatusBadRequest)
			}
		}))
		servers = append(servers, server)
		configured[fmt.Sprintf("server-%d", i)] = ServerConfig{URL: server.URL}
	}
	defer func() {
		for _, server := range servers {
			server.Close()
		}
	}()

	cfg := Config{MCPServers: configured}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		manager, err := ConnectConfigured(context.Background(), cfg, extension.NewRegistry())
		if err != nil {
			b.Fatal(err)
		}
		if err := manager.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func writeBenchmarkResult(w http.ResponseWriter, id int64, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
