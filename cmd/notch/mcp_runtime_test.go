package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/mcp"
	"github.com/trobrock/notch/internal/mcpoauth"
)

func TestMCPRuntimeLoginStoresCredentialAndReconnects(t *testing.T) {
	const serverURL = "https://mcp.example.test/mcp"
	config := mcp.Config{MCPServers: map[string]mcp.ServerConfig{
		"demo": {URL: serverURL, Auth: "oauth", OAuth: &mcp.OAuthConfig{Scope: "read write"}},
	}}
	store := mcpoauth.NewStore(filepath.Join(t.TempDir(), "mcp-auth.json"))
	runtime := newMCPRuntime(config, extension.NewRegistry(), store, &mcpoauth.Authorizer{Store: store}, nil)

	var gotResource, gotScope string
	runtime.login = func(_ context.Context, resource, scope string, out io.Writer) (mcpoauth.Credential, error) {
		gotResource, gotScope = resource, scope
		_, _ = io.WriteString(out, "Open this URL in your browser")
		return validTestMCPCredential(serverURL), nil
	}
	connects := 0
	runtime.connect = func(_ context.Context, _ mcp.Config, _ *extension.Registry, _ ...*mcpoauth.Authorizer) (*mcp.Manager, error) {
		connects++
		return nil, nil
	}

	var output bytes.Buffer
	text, err := runtime.command(&output).Execute(context.Background(), "login demo")
	if err != nil {
		t.Fatal(err)
	}
	if gotResource != serverURL || gotScope != "read write" {
		t.Fatalf("login resource=%q scope=%q", gotResource, gotScope)
	}
	if connects != 1 {
		t.Fatalf("connect calls = %d, want 1", connects)
	}
	if !strings.Contains(output.String(), "Open this URL") {
		t.Fatalf("login output = %q", output.String())
	}
	if text != "Logged in to MCP server demo and reloaded MCP tools." {
		t.Fatalf("command result = %q", text)
	}
	credential, ok, err := store.Get("demo", serverURL)
	if err != nil || !ok || credential.AccessToken != "access" {
		t.Fatalf("stored credential = %+v, ok=%v, err=%v", credential, ok, err)
	}
}

func TestMCPRuntimeLoginValidatesCommandBeforeOpeningBrowser(t *testing.T) {
	config := mcp.Config{MCPServers: map[string]mcp.ServerConfig{
		"local": {Command: "server"},
		"plain": {URL: "https://mcp.example.test/mcp"},
	}}
	runtime := newMCPRuntime(config, extension.NewRegistry(), mcpoauth.NewStore(filepath.Join(t.TempDir(), "auth.json")), nil, nil)
	runtime.login = func(context.Context, string, string, io.Writer) (mcpoauth.Credential, error) {
		t.Fatal("login called for invalid command")
		return mcpoauth.Credential{}, nil
	}
	command := runtime.command(io.Discard)
	for _, args := range []string{"", "status", "login missing", "login local", "login plain"} {
		if _, err := command.Execute(context.Background(), args); err == nil {
			t.Fatalf("%q succeeded", args)
		}
	}
}

func TestMCPRuntimeReportsReloadFailureAfterSuccessfulLogin(t *testing.T) {
	const serverURL = "https://mcp.example.test/mcp"
	config := mcp.Config{MCPServers: map[string]mcp.ServerConfig{"demo": {URL: serverURL, Auth: "oauth"}}}
	store := mcpoauth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	runtime := newMCPRuntime(config, extension.NewRegistry(), store, &mcpoauth.Authorizer{Store: store}, nil)
	runtime.login = func(context.Context, string, string, io.Writer) (mcpoauth.Credential, error) {
		return validTestMCPCredential(serverURL), nil
	}
	runtime.connect = func(context.Context, mcp.Config, *extension.Registry, ...*mcpoauth.Authorizer) (*mcp.Manager, error) {
		return nil, errors.New("handshake failed")
	}

	text, err := runtime.command(io.Discard).Execute(context.Background(), "login demo")
	if text != "Logged in to MCP server demo." || err == nil || !strings.Contains(err.Error(), "reload MCP tools") {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if _, ok, getErr := store.Get("demo", serverURL); getErr != nil || !ok {
		t.Fatalf("credential was not retained after reload failure: ok=%v err=%v", ok, getErr)
	}
}

func TestMCPLoginHintNamesOAuthServer(t *testing.T) {
	config := mcp.Config{MCPServers: map[string]mcp.ServerConfig{
		"firecrawl": {URL: "https://mcp.firecrawl.dev/v2/mcp-oauth", Auth: "oauth"},
		"plain":     {URL: "https://example.test/mcp"},
	}}
	err := errors.New(`MCP server "firecrawl": initialize: authorize MCP HTTP request: refresh MCP OAuth token: server returned 400 Bad Request`)
	if got := mcpLoginHint(err, config); got != "Run /mcp login firecrawl to reauthenticate and reload MCP tools without restarting." {
		t.Fatalf("hint = %q", got)
	}
	if got := mcpLoginHint(errors.New(`MCP server "firecrawl": protocol mismatch`), config); got != "" {
		t.Fatalf("non-auth hint = %q", got)
	}
	if got := mcpLoginHint(errors.New(`MCP server "plain": failed`), config); got != "" {
		t.Fatalf("non-OAuth hint = %q", got)
	}
}

func validTestMCPCredential(serverURL string) mcpoauth.Credential {
	return mcpoauth.Credential{
		ServerURL: serverURL, Resource: serverURL, AuthorizationServer: "https://auth.example.test",
		TokenEndpoint: "https://auth.example.test/token", ClientID: "client", TokenAuthMethod: "none",
		AccessToken: "access", RefreshToken: "refresh", Scope: "read write", TokenType: "Bearer",
	}
}
