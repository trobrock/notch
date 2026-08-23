package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

// Manager owns the connections and processes backing configured MCP servers.
type Manager struct {
	mu      sync.Mutex
	clients []rpcClient
	closed  bool
}

// ConnectConfigured connects enabled servers, performs the MCP handshake, and
// registers each advertised tool in registry. Tool names use the conventional
// mcp__<server>__<tool> namespace.
func ConnectConfigured(ctx context.Context, cfg Config, registry *extension.Registry) (*Manager, error) {
	if registry == nil {
		return nil, errors.New("MCP registry is nil")
	}
	manager := &Manager{}
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		server := cfg.MCPServers[name]
		if !server.isEnabled() {
			continue
		}
		if name == "" {
			_ = manager.Close()
			return nil, errors.New("MCP server name is empty")
		}
		client, err := connectServer(ctx, server)
		if err != nil {
			_ = manager.Close()
			return nil, fmt.Errorf("connect MCP server %q: %w", name, err)
		}
		manager.clients = append(manager.clients, client)
		if err := initialize(ctx, client); err != nil {
			_ = manager.Close()
			return nil, fmt.Errorf("MCP server %q: %w", name, err)
		}
		tools, err := listTools(ctx, client)
		if err != nil {
			_ = manager.Close()
			return nil, fmt.Errorf("MCP server %q: %w", name, err)
		}
		for _, remote := range tools {
			if remote.Name == "" {
				_ = manager.Close()
				return nil, fmt.Errorf("MCP server %q returned a tool with no name", name)
			}
			inputSchema := remote.InputSchema
			if inputSchema == nil {
				inputSchema = map[string]any{"type": "object"}
			}
			clientForTool := client
			remoteName := remote.Name
			tool := extension.Tool{
				Definition: model.ToolDefinition{
					Name:        namespacedToolName(name, remote.Name),
					Description: remote.Description,
					InputSchema: inputSchema,
				},
				Source: "mcp:" + name,
				Execute: func(ctx context.Context, args json.RawMessage, _ func(string)) (extension.ToolResult, error) {
					if len(args) == 0 {
						args = json.RawMessage(`{}`)
					}
					var result callToolResult
					if err := clientForTool.call(ctx, "tools/call", struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					}{remoteName, args}, &result); err != nil {
						return extension.ToolResult{}, fmt.Errorf("MCP tool %s: %w", remoteName, err)
					}
					content, details, err := renderToolResult(result)
					if err != nil {
						return extension.ToolResult{}, err
					}
					return extension.ToolResult{Content: content, IsError: result.IsError, Details: details}, nil
				},
			}
			if err := registry.RegisterTool(tool); err != nil {
				_ = manager.Close()
				return nil, fmt.Errorf("register MCP server %q tool %q: %w", name, remote.Name, err)
			}
		}
	}
	return manager, nil
}

func connectServer(ctx context.Context, cfg ServerConfig) (rpcClient, error) {
	switch {
	case cfg.Command != "" && cfg.URL != "":
		return nil, errors.New("configuration has both command and url")
	case cfg.Command != "":
		return newStdioClient(ctx, cfg)
	case cfg.URL != "":
		return newHTTPClient(cfg), nil
	default:
		return nil, errors.New("configuration requires command or url")
	}
}

func namespacedToolName(server, tool string) string {
	return "mcp__" + server + "__" + tool
}

// Close terminates all server processes and HTTP sessions. It is idempotent.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	clients := append([]rpcClient(nil), m.clients...)
	m.clients = nil
	m.mu.Unlock()

	var errs []error
	for i := len(clients) - 1; i >= 0; i-- {
		if err := clients[i].close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
