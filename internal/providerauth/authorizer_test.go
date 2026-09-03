package providerauth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/credentials"
)

func newStore(t *testing.T, credential credentials.Credential) *credentials.Store {
	t.Helper()
	store := credentials.New(filepath.Join(t.TempDir(), "auth.json"))
	if credential.Access != "" {
		if err := store.Put("openai-codex", credential); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestTokenReturnsStoredTokenWithoutRefresh(t *testing.T) {
	store := newStore(t, credentials.Credential{
		Type: "oauth", Access: "access", Refresh: "refresh",
		Expires: time.Now().Add(time.Hour).UnixMilli(),
	})
	authorizer := New(store, "openai-codex", "", func(context.Context, string, credentials.Credential) (credentials.Credential, error) {
		t.Fatal("unexpected refresh")
		return credentials.Credential{}, nil
	})
	token, err := authorizer.Token(context.Background(), "")
	if err != nil || token != "access" {
		t.Fatalf("token = %q, %v", token, err)
	}
}

func TestTokenRefreshesNearExpiryAndPersists(t *testing.T) {
	store := newStore(t, credentials.Credential{
		Type: "oauth", Access: "old", Refresh: "refresh",
		Expires: time.Now().Add(time.Minute).UnixMilli(), AccountID: "acct",
	})
	authorizer := New(store, "openai-codex", "", func(_ context.Context, _ string, credential credentials.Credential) (credentials.Credential, error) {
		credential.Access = "new"
		credential.Expires = time.Now().Add(time.Hour).UnixMilli()
		return credential, nil
	})
	token, err := authorizer.Token(context.Background(), "")
	if err != nil || token != "new" {
		t.Fatalf("token = %q, %v", token, err)
	}
	stored, ok, err := store.Get("openai-codex")
	if err != nil || !ok || stored.Access != "new" || stored.AccountID != "acct" {
		t.Fatalf("stored = %+v, %v, %v", stored, ok, err)
	}
}

func TestTokenRefreshesRejectedTokenDespiteFutureExpiry(t *testing.T) {
	store := newStore(t, credentials.Credential{
		Type: "oauth", Access: "rejected", Refresh: "refresh",
		Expires: time.Now().Add(time.Hour).UnixMilli(),
	})
	var refreshes int
	authorizer := New(store, "openai-codex", "", func(_ context.Context, _ string, credential credentials.Credential) (credentials.Credential, error) {
		refreshes++
		credential.Access = "new"
		return credential, nil
	})
	token, err := authorizer.Token(context.Background(), "rejected")
	if err != nil || token != "new" || refreshes != 1 {
		t.Fatalf("token = %q, refreshes = %d, %v", token, refreshes, err)
	}
}

func TestTokenAdoptsTokenRefreshedByAnotherProcess(t *testing.T) {
	store := newStore(t, credentials.Credential{
		Type: "oauth", Access: "written-by-other-process", Refresh: "refresh",
		Expires: time.Now().Add(time.Hour).UnixMilli(),
	})
	authorizer := New(store, "openai-codex", "", func(context.Context, string, credentials.Credential) (credentials.Credential, error) {
		t.Fatal("unexpected refresh")
		return credentials.Credential{}, nil
	})
	token, err := authorizer.Token(context.Background(), "stale-in-memory-token")
	if err != nil || token != "written-by-other-process" {
		t.Fatalf("token = %q, %v", token, err)
	}
}

func TestTokenReportsMissingAndUnrefreshableCredentials(t *testing.T) {
	missing := New(newStore(t, credentials.Credential{}), "openai-codex", "", nil)
	if _, err := missing.Token(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "notch login openai-codex") {
		t.Fatalf("err = %v", err)
	}

	store := newStore(t, credentials.Credential{Type: "oauth", Access: "expired", Expires: 1})
	authorizer := New(store, "openai-codex", "", func(context.Context, string, credentials.Credential) (credentials.Credential, error) {
		t.Fatal("unexpected refresh without a refresh token")
		return credentials.Credential{}, nil
	})
	if _, err := authorizer.Token(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "login expired") {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenWrapsRefreshFailure(t *testing.T) {
	store := newStore(t, credentials.Credential{Type: "oauth", Access: "old", Refresh: "refresh", Expires: 1})
	authorizer := New(store, "openai-codex", "", func(context.Context, string, credentials.Credential) (credentials.Credential, error) {
		return credentials.Credential{}, errors.New("network down")
	})
	_, err := authorizer.Token(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "refresh openai-codex login: network down") {
		t.Fatalf("err = %v", err)
	}
	stored, _, _ := store.Get("openai-codex")
	if stored.Access != "old" {
		t.Fatalf("stored credential was modified: %+v", stored)
	}
}

func TestCredentialUsesLegacyProviderKey(t *testing.T) {
	store := credentials.New(filepath.Join(t.TempDir(), "auth.json"))
	if err := store.Put(credentials.LegacyAnthropicProvider, credentials.Credential{Type: "oauth", Access: "legacy"}); err != nil {
		t.Fatal(err)
	}
	authorizer := New(store, credentials.AnthropicClaudeCodeProvider, credentials.LegacyAnthropicProvider, nil)
	credential, err := authorizer.Credential(context.Background())
	if err != nil || credential.Access != "legacy" {
		t.Fatalf("credential = %+v, %v", credential, err)
	}
}
