package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/mcp"
	"github.com/trobrock/notch/internal/mcpoauth"
)

type mcpLoginFunc func(context.Context, string, string, io.Writer) (mcpoauth.Credential, error)
type mcpConnectFunc func(context.Context, mcp.Config, *extension.Registry, ...*mcpoauth.Authorizer) (*mcp.Manager, error)

// mcpRuntime owns the active MCP connections so an interactive login can
// replace expired credentials and reload tools without restarting Notch.
type mcpRuntime struct {
	mu          sync.Mutex
	config      mcp.Config
	registry    *extension.Registry
	store       *mcpoauth.Store
	authorizer  *mcpoauth.Authorizer
	manager     *mcp.Manager
	login       mcpLoginFunc
	connect     mcpConnectFunc
	applyPolicy func()
}

func newMCPRuntime(config mcp.Config, registry *extension.Registry, store *mcpoauth.Store, authorizer *mcpoauth.Authorizer, applyPolicy func()) *mcpRuntime {
	return &mcpRuntime{
		config: config, registry: registry, store: store, authorizer: authorizer,
		login: func(ctx context.Context, resource, scope string, out io.Writer) (mcpoauth.Credential, error) {
			return mcpoauth.NewClient().Login(ctx, resource, scope, out)
		},
		connect: mcp.ConnectConfigured, applyPolicy: applyPolicy,
	}
}

func (r *mcpRuntime) Connect(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var closeErr error
	if r.manager != nil {
		closeErr = r.manager.Close()
		r.manager = nil
	}
	manager, err := r.connect(ctx, r.config, r.registry, r.authorizer)
	if err != nil {
		return errors.Join(closeErr, err)
	}
	r.manager = manager
	if r.applyPolicy != nil {
		r.applyPolicy()
	}
	return closeErr
}

func (r *mcpRuntime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.manager == nil {
		return nil
	}
	err := r.manager.Close()
	r.manager = nil
	return err
}

func (r *mcpRuntime) command(out io.Writer) extension.Command {
	return extension.Command{
		Name: "mcp", Description: "log in to an MCP server and reload its tools", Source: "builtin:mcp",
		Execute: func(ctx context.Context, args string) (string, error) {
			fields := strings.Fields(args)
			if len(fields) != 2 || fields[0] != "login" {
				return "", errors.New("usage: /mcp login NAME")
			}
			name := fields[1]
			server, ok := r.config.MCPServers[name]
			if !ok {
				return "", fmt.Errorf("MCP server %q is not configured", name)
			}
			if server.URL == "" || server.Command != "" {
				return "", fmt.Errorf("MCP server %q is not a remote HTTP server", name)
			}
			if !server.OAuthEnabled() {
				return "", fmt.Errorf("MCP server %q does not enable OAuth", name)
			}
			scope := ""
			if server.OAuth != nil {
				scope = server.OAuth.Scope
			}
			loginCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			credential, err := r.login(loginCtx, server.URL, scope, out)
			if err != nil {
				return "", err
			}
			if err := r.store.Put(name, credential); err != nil {
				return "", err
			}
			if err := r.Connect(ctx); err != nil {
				return "Logged in to MCP server " + name + ".", fmt.Errorf("reload MCP tools: %w", err)
			}
			return "Logged in to MCP server " + name + " and reloaded MCP tools.", nil
		},
	}
}

type mcpNoticeWriter struct{ host extension.Host }

func (w mcpNoticeWriter) Write(p []byte) (int, error) {
	if text := strings.TrimSpace(string(p)); text != "" {
		w.host.Notify(text, "notice")
	}
	return len(p), nil
}

func mcpLoginHint(err error, config mcp.Config) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if !strings.Contains(message, "MCP OAuth login required") &&
		!strings.Contains(message, "refresh MCP OAuth token") {
		return ""
	}
	for _, name := range sortedMCPNames(config.MCPServers) {
		server := config.MCPServers[name]
		if server.OAuthEnabled() && strings.Contains(message, fmt.Sprintf("MCP server %q", name)) {
			return fmt.Sprintf("Run /mcp login %s to reauthenticate and reload MCP tools without restarting.", name)
		}
	}
	return ""
}
