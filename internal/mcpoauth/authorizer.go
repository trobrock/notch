package mcpoauth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Authorizer resolves and refreshes stored OAuth bearer tokens for MCP HTTP
// requests. Refreshes are serialized so concurrent tool calls cannot rotate the
// same refresh token more than once.
type Authorizer struct {
	Store  *Store
	Client *Client

	mu sync.Mutex
}

func (a *Authorizer) HasCredential(name, resource string) (bool, error) {
	if a == nil || a.Store == nil {
		return false, nil
	}
	_, ok, err := a.Store.Get(name, resource)
	return ok, err
}

func (a *Authorizer) Token(ctx context.Context, name, resource string, forceRefresh bool) (string, error) {
	if a == nil || a.Store == nil {
		return "", errors.New("MCP OAuth credential store is unavailable")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	credential, ok, err := a.Store.Get(name, resource)
	if err != nil {
		return "", err
	}
	if !ok || credential.AccessToken == "" {
		return "", errors.New("MCP OAuth login required; run `notch mcp login " + name + "`")
	}
	if !forceRefresh && (credential.ExpiresAt == 0 || credential.ExpiresAt > time.Now().Add(5*time.Minute).UnixMilli()) {
		return credential.AccessToken, nil
	}
	client := a.Client
	if client == nil {
		client = NewClient()
	}
	refreshed, err := client.Refresh(ctx, credential)
	if err != nil {
		return "", err
	}
	if err := a.Store.Put(name, refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}
