// Package providerauth resolves and refreshes stored provider OAuth
// credentials for the lifetime of a Notch process.
package providerauth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/trobrock/notch/internal/credentials"
)

// refreshLeadTime is how long before expiry a credential is refreshed
// pre-emptively.
const refreshLeadTime = 5 * time.Minute

// Refresher exchanges a credential's refresh token for a current credential.
type Refresher func(ctx context.Context, provider string, credential credentials.Credential) (credentials.Credential, error)

// Authorizer serves access tokens for one provider. Tokens are read from the
// store on every call so a refresh performed by another Notch process is picked
// up without a restart, and refreshes are serialized so concurrent requests
// cannot rotate the same refresh token more than once.
type Authorizer struct {
	store          *credentials.Store
	provider       string
	legacyProvider string
	refresh        Refresher

	mu sync.Mutex
}

// New returns an authorizer for provider. legacyProvider may be empty; when
// set, a credential stored under that key is migrated on first read. A nil
// refresh function disables refreshing.
func New(store *credentials.Store, provider, legacyProvider string, refresh Refresher) *Authorizer {
	return &Authorizer{store: store, provider: provider, legacyProvider: legacyProvider, refresh: refresh}
}

// Credential returns a currently valid credential, refreshing it when it is
// expired or close to expiry.
func (a *Authorizer) Credential(ctx context.Context) (credentials.Credential, error) {
	return a.resolve(ctx, "")
}

// Token returns a currently valid access token. A non-empty stale token
// reports a token the caller just saw rejected, which forces a refresh unless
// the stored token has meanwhile changed.
func (a *Authorizer) Token(ctx context.Context, stale string) (string, error) {
	credential, err := a.resolve(ctx, stale)
	if err != nil {
		return "", err
	}
	return credential.Access, nil
}

func (a *Authorizer) resolve(ctx context.Context, stale string) (credentials.Credential, error) {
	if a == nil || a.store == nil {
		return credentials.Credential{}, fmt.Errorf("no %s credential store", a.providerName())
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	credential, err := a.load()
	if err != nil {
		return credentials.Credential{}, err
	}
	if !a.needsRefresh(credential, stale) {
		return credential, nil
	}
	if a.refresh == nil || credential.Refresh == "" {
		return credentials.Credential{}, fmt.Errorf("%s login expired; run: notch login %s", a.provider, a.provider)
	}
	refreshed, err := a.refresh(ctx, a.provider, credential)
	if err != nil {
		return credentials.Credential{}, fmt.Errorf("refresh %s login: %w", a.provider, err)
	}
	if err := a.store.Put(a.provider, refreshed); err != nil {
		return credentials.Credential{}, err
	}
	return refreshed, nil
}

func (a *Authorizer) load() (credentials.Credential, error) {
	var (
		credential credentials.Credential
		ok         bool
		err        error
	)
	if a.legacyProvider != "" {
		credential, ok, err = a.store.GetWithLegacyFallback(a.provider, a.legacyProvider)
	} else {
		credential, ok, err = a.store.Get(a.provider)
	}
	if err != nil {
		return credentials.Credential{}, err
	}
	if !ok || credential.Access == "" {
		return credentials.Credential{}, fmt.Errorf("no %s credential; run: notch login %s", a.provider, a.provider)
	}
	return credential, nil
}

// needsRefresh reports whether credential must be exchanged before use. A
// rejected token is refreshed regardless of its recorded expiry, unless the
// store already holds a different token.
func (a *Authorizer) needsRefresh(credential credentials.Credential, stale string) bool {
	if stale != "" {
		return credential.Access == stale
	}
	return credential.Expires > 0 && credential.Expires <= time.Now().Add(refreshLeadTime).UnixMilli()
}

func (a *Authorizer) providerName() string {
	if a == nil || a.provider == "" {
		return "provider"
	}
	return a.provider
}
