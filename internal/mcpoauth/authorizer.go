package mcpoauth

import (
	"context"
	"errors"
	"sync"
	"time"
)

const refreshLeadTime = 5 * time.Minute

// Authorizer resolves and refreshes stored OAuth bearer tokens for MCP HTTP
// requests. Refreshes are serialized within this process and through the
// credential store so separate Notch processes cannot rotate the same refresh
// token concurrently.
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

// Token returns a current access token. stale is the token rejected by the MCP
// server, or empty for an ordinary request. Reading the store again under the
// refresh lock lets this process adopt a token refreshed by another process.
func (a *Authorizer) Token(ctx context.Context, name, resource, stale string) (string, error) {
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
	if !needsRefresh(credential, stale) {
		return credential.AccessToken, nil
	}
	return a.Store.withRefreshLock(func() (string, error) {
		credential, ok, err := a.Store.Get(name, resource)
		if err != nil {
			return "", err
		}
		if !ok || credential.AccessToken == "" {
			return "", errors.New("MCP OAuth login required; run `notch mcp login " + name + "`")
		}
		if !needsRefresh(credential, stale) {
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
	})
}

func needsRefresh(credential Credential, stale string) bool {
	if stale != "" {
		return credential.AccessToken == stale
	}
	return credential.ExpiresAt > 0 && credential.ExpiresAt <= time.Now().Add(refreshLeadTime).UnixMilli()
}
