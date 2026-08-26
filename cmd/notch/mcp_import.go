package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/trobrock/notch/internal/mcp"
	"github.com/trobrock/notch/internal/mcpoauth"
)

type piMCPAuthEntry struct {
	Tokens struct {
		AccessToken  string  `json:"accessToken"`
		RefreshToken string  `json:"refreshToken"`
		ExpiresAt    float64 `json:"expiresAt"`
		Scope        string  `json:"scope"`
		Issuer       string  `json:"issuer"`
	} `json:"tokens"`
	ClientInfo struct {
		ClientID             string `json:"clientId"`
		ClientSecret         string `json:"clientSecret"`
		TokenAuthMethod      string `json:"tokenEndpointAuthMethod"`
		ClientIDSnake        string `json:"client_id"`
		ClientSecretSnake    string `json:"client_secret"`
		TokenAuthMethodSnake string `json:"token_endpoint_auth_method"`
		Issuer               string `json:"issuer"`
	} `json:"clientInfo"`
	ServerURL string `json:"serverUrl"`
}

func importPiMCP(ctx context.Context, path string, notchConfig mcp.Config, store *mcpoauth.Store) error {
	piConfig, err := mcp.LoadConfig(path)
	if err != nil {
		return fmt.Errorf("load Pi MCP config: %w", err)
	}
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return errors.New("import Pi MCP OAuth: secret-tool is required to read the Linux credential store")
	}
	imported := 0
	for name, piServer := range piConfig.MCPServers {
		if !piServer.OAuthEnabled() || piServer.URL == "" {
			continue
		}
		notchServer, ok := notchConfig.MCPServers[name]
		if !ok || !notchServer.OAuthEnabled() || notchServer.URL != piServer.URL {
			return fmt.Errorf("Pi OAuth server %q is not configured in Notch with the same URL", name)
		}
		payload, ok, err := readPiMCPKeyringEntry(name)
		if err != nil {
			return fmt.Errorf("read Pi MCP OAuth credential %q: %w", name, err)
		}
		if !ok {
			continue
		}
		var entry piMCPAuthEntry
		if err := json.Unmarshal([]byte(payload), &entry); err != nil {
			return fmt.Errorf("parse Pi MCP OAuth credential %q: %w", name, err)
		}
		if entry.ServerURL != "" && entry.ServerURL != piServer.URL {
			return fmt.Errorf("Pi MCP OAuth credential %q is bound to a different URL", name)
		}
		clientID := entry.ClientInfo.ClientID
		if clientID == "" {
			clientID = entry.ClientInfo.ClientIDSnake
		}
		clientSecret := entry.ClientInfo.ClientSecret
		if clientSecret == "" {
			clientSecret = entry.ClientInfo.ClientSecretSnake
		}
		authMethod := entry.ClientInfo.TokenAuthMethod
		if authMethod == "" {
			authMethod = entry.ClientInfo.TokenAuthMethodSnake
		}
		if authMethod == "" {
			authMethod = "none"
		}
		issuer := entry.Tokens.Issuer
		if issuer == "" {
			issuer = entry.ClientInfo.Issuer
		}
		discovery, discoverErr := mcpoauth.NewClient().Discover(ctx, piServer.URL)
		if discoverErr != nil {
			return fmt.Errorf("discover OAuth metadata for Pi MCP credential %q: %w", name, discoverErr)
		}
		if issuer != "" && strings.TrimSuffix(issuer, "/") != strings.TrimSuffix(discovery.AuthorizationServer, "/") {
			return fmt.Errorf("Pi MCP OAuth credential %q issuer does not match current discovery", name)
		}
		issuer = discovery.AuthorizationServer
		resourceIndicator := discovery.Resource
		tokenEndpoint := discovery.TokenEndpoint
		credential := mcpoauth.Credential{
			ServerURL: piServer.URL, Resource: resourceIndicator, AuthorizationServer: issuer, TokenEndpoint: tokenEndpoint,
			ClientID: clientID, ClientSecret: clientSecret, TokenAuthMethod: authMethod,
			AccessToken: entry.Tokens.AccessToken, RefreshToken: entry.Tokens.RefreshToken,
			ExpiresAt: piExpiresMillis(entry.Tokens.ExpiresAt), Scope: entry.Tokens.Scope, TokenType: "Bearer",
		}
		if err := store.Put(name, credential); err != nil {
			return fmt.Errorf("store imported MCP OAuth credential %q: %w", name, err)
		}
		imported++
	}
	fmt.Printf("imported %d Pi MCP OAuth credential(s) into %s\n", imported, store.Path())
	return nil
}

func readPiMCPKeyringEntry(name string) (string, bool, error) {
	digest := sha256.Sum256([]byte(name))
	account := "sha256-" + hex.EncodeToString(digest[:])
	command := exec.Command("secret-tool", "lookup", "service", "pi-mcp-adapter.oauth", "username", account)
	command.Env = os.Environ()
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	payload := strings.TrimSpace(string(output))
	if payload == "" {
		return "", false, nil
	}
	return payload, true, nil
}

func piExpiresMillis(value float64) int64 {
	expires := int64(value)
	if expires > 0 && expires < 100_000_000_000 {
		return expires * 1000
	}
	return expires
}
