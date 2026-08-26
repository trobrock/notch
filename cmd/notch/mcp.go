package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/trobrock/notch/internal/config"
	"github.com/trobrock/notch/internal/mcp"
	"github.com/trobrock/notch/internal/mcpoauth"
)

const mcpUsage = `usage: notch mcp COMMAND

Commands:
  login [--scope SCOPE] NAME  authorize an OAuth-enabled HTTP server
  logout NAME                 remove a server's OAuth credential
  import-pi [PATH]            import OAuth logins from pi-mcp-adapter
  status                      show configured MCP servers and login state`

func runMCP(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(mcpUsage)
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// MCP authentication is global and never reads project configuration. This
	// keeps project files from selecting credential storage or remote endpoints
	// for a standalone login command.
	cfg, err := config.LoadGlobal(home)
	if err != nil {
		return err
	}
	mcpConfig, err := mcp.LoadConfig(cfg.MCPConfig)
	if err != nil {
		return err
	}
	store := mcpoauth.NewStore(cfg.MCPAuthFile)

	switch args[0] {
	case "login":
		flags := flag.NewFlagSet("notch mcp login", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		scope := flags.String("scope", "", "space-separated OAuth scopes (defaults to discovery)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: notch mcp login [--scope SCOPE] NAME")
		}
		name := flags.Arg(0)
		server, ok := mcpConfig.MCPServers[name]
		if !ok {
			return fmt.Errorf("MCP server %q is not configured in %s", name, cfg.MCPConfig)
		}
		if server.URL == "" || server.Command != "" {
			return fmt.Errorf("MCP server %q is not a remote HTTP server", name)
		}
		if !server.OAuthEnabled() {
			return fmt.Errorf("MCP server %q does not enable OAuth", name)
		}
		requestedScope := ""
		if server.OAuth != nil {
			requestedScope = server.OAuth.Scope
		}
		if *scope != "" {
			requestedScope = *scope
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		credential, err := mcpoauth.NewClient().Login(ctx, server.URL, requestedScope, os.Stderr)
		if err != nil {
			return err
		}
		if err := store.Put(name, credential); err != nil {
			return err
		}
		fmt.Printf("logged in to MCP server %s; credentials saved to %s\n", name, store.Path())
		return nil
	case "logout":
		if len(args) != 2 {
			return errors.New("usage: notch mcp logout NAME")
		}
		if err := store.Delete(args[1]); err != nil {
			return err
		}
		fmt.Println("removed MCP credential for", args[1])
		return nil
	case "import-pi":
		if len(args) > 2 {
			return errors.New("usage: notch mcp import-pi [PATH]")
		}
		path := filepath.Join(home, ".config", "mcp", "mcp.json")
		if len(args) == 2 {
			path = args[1]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return importPiMCP(ctx, path, mcpConfig, store)
	case "status":
		if len(args) != 1 {
			return errors.New("usage: notch mcp status")
		}
		for _, name := range sortedMCPNames(mcpConfig.MCPServers) {
			server := mcpConfig.MCPServers[name]
			transport := "stdio"
			status := "authentication not required"
			if server.URL != "" {
				transport = "http"
			}
			if server.OAuthEnabled() {
				status = "not logged in"
				credential, ok, getErr := store.GetAny(name)
				if getErr != nil {
					return getErr
				}
				if ok {
					if credential.ServerURL != server.URL {
						status = "credential URL mismatch; log in again"
					} else {
						status = "logged in"
						if credential.ExpiresAt > 0 {
							status += ", expires " + time.UnixMilli(credential.ExpiresAt).Format(time.RFC3339)
						}
					}
				}
			}
			fmt.Printf("%s\t%s\t%s\n", name, transport, status)
		}
		return nil
	default:
		return fmt.Errorf("unknown MCP command %q\n%s", args[0], mcpUsage)
	}
}

func sortedMCPNames(servers map[string]mcp.ServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
