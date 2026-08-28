package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/mcpoauth"
	"github.com/trobrock/notch/internal/model"
)

type clientConnection struct {
	client rpcClient
	lease  *extension.Registration
}

// Manager owns the connections and processes backing configured MCP servers.
type Manager struct {
	mu          sync.Mutex
	connections []clientConnection
	closed      bool
}

// ConnectConfigured connects enabled servers, performs the MCP handshake, and
// registers each advertised tool in registry. Tool names use the conventional
// mcp__<server>__<tool> namespace.
func ConnectConfigured(ctx context.Context, cfg Config, registry *extension.Registry, authorizers ...*mcpoauth.Authorizer) (*Manager, error) {
	if registry == nil {
		return nil, errors.New("MCP registry is nil")
	}
	manager := &Manager{}
	var authorizer *mcpoauth.Authorizer
	if len(authorizers) != 0 {
		authorizer = authorizers[0]
	}
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	enabledNames := names[:0]
	for _, name := range names {
		if !cfg.MCPServers[name].isEnabled() {
			continue
		}
		if name == "" {
			return nil, errors.New("MCP server name is empty")
		}
		enabledNames = append(enabledNames, name)
	}

	// MCP handshakes are independent and often involve process startup or
	// network round trips. Prepare them concurrently, then register their tools
	// in sorted order so duplicate handling and hook order stay deterministic.
	type preparedServer struct {
		client          rpcClient
		tools           []listedTool
		connectionIndex int
		err             error
	}
	prepared := make([]preparedServer, len(enabledNames))
	var wg sync.WaitGroup
	for i, name := range enabledNames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server := cfg.MCPServers[name]
			client, err := connectServer(ctx, name, server, authorizer)
			if err != nil {
				prepared[i].err = fmt.Errorf("connect MCP server %q: %w", name, err)
				return
			}
			prepared[i].client = client
			if err := initialize(ctx, client); err != nil {
				prepared[i].err = fmt.Errorf("MCP server %q: %w", name, err)
				return
			}
			prepared[i].tools, err = listTools(ctx, client)
			if err != nil {
				prepared[i].err = fmt.Errorf("MCP server %q: %w", name, err)
			}
		}()
	}
	wg.Wait()

	manager.connections = make([]clientConnection, 0, len(prepared))
	for i := range prepared {
		if prepared[i].client != nil {
			prepared[i].connectionIndex = len(manager.connections)
			manager.connections = append(manager.connections, clientConnection{client: prepared[i].client})
		}
	}
	for i, server := range prepared {
		if server.err != nil {
			_ = manager.Close()
			return nil, server.err
		}
		name := enabledNames[i]
		serverTools := make([]extension.Tool, 0, len(server.tools))
		for _, remote := range server.tools {
			if remote.Name == "" {
				_ = manager.Close()
				return nil, fmt.Errorf("MCP server %q returned a tool with no name", name)
			}
			inputSchema := remote.InputSchema
			if inputSchema == nil {
				inputSchema = map[string]any{"type": "object"}
			}
			clientForTool := server.client
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
			serverTools = append(serverTools, tool)
		}
		lease, err := registry.RegisterBatch(extension.Batch{Tools: serverTools})
		if err != nil {
			_ = manager.Close()
			return nil, fmt.Errorf("register MCP server %q tools: %w", name, err)
		}
		manager.connections[server.connectionIndex].lease = lease
	}
	return manager, nil
}

func connectServer(ctx context.Context, name string, cfg ServerConfig, authorizer *mcpoauth.Authorizer) (rpcClient, error) {
	if cfg.Auth != "" && cfg.Auth != "oauth" {
		return nil, fmt.Errorf("MCP server %q has unsupported auth mode %q", name, cfg.Auth)
	}
	switch {
	case cfg.Command != "" && cfg.URL != "":
		return nil, errors.New("configuration has both command and url")
	case cfg.OAuthEnabled() && cfg.Command != "":
		return nil, errors.New("OAuth is only supported for HTTP servers")
	case cfg.Command != "":
		return newStdioClient(ctx, cfg)
	case cfg.URL != "":
		if !cfg.OAuthEnabled() {
			return newHTTPClient(cfg, nil), nil
		}
		if authorizer == nil {
			return nil, errors.New("MCP OAuth is configured but no credential store is available")
		}
		return newHTTPClient(cfg, func(ctx context.Context, forceRefresh bool) (string, error) {
			return authorizer.Token(ctx, name, cfg.URL, forceRefresh)
		}), nil
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
	connections := append([]clientConnection(nil), m.connections...)
	m.connections = nil
	m.mu.Unlock()

	var errs []error
	for i := len(connections) - 1; i >= 0; i-- {
		if err := connections[i].lease.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := connections[i].client.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
