package mcpoauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenAdoptsTokenRefreshedByAnotherProcess(t *testing.T) {
	resource := "https://resource.test/mcp"
	store := NewStore(filepath.Join(t.TempDir(), "mcp-auth.json"))
	credential := testCredential(resource)
	credential.AccessToken = "old-access"
	credential.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	if err := store.Put("example", credential); err != nil {
		t.Fatal(err)
	}

	credential.AccessToken = "new-access"
	credential.RefreshToken = "new-refresh"
	if err := store.Put("example", credential); err != nil {
		t.Fatal(err)
	}

	authorizer := &Authorizer{Store: NewStore(store.Path()), Client: &Client{
		HTTPClient: http.DefaultClient,
	}}
	token, err := authorizer.Token(context.Background(), "example", resource, "old-access")
	if err != nil {
		t.Fatal(err)
	}
	if token != "new-access" {
		t.Fatalf("token = %q", token)
	}
}

func TestTokenSerializesRefreshAcrossStores(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access", "refresh_token": "new-refresh", "token_type": "Bearer", "expires_in": 3600,
		}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	resource := "https://resource.test/mcp"
	path := filepath.Join(t.TempDir(), "mcp-auth.json")
	credential := testCredential(resource)
	credential.TokenEndpoint = server.URL
	credential.AccessToken = "old-access"
	credential.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	if err := NewStore(path).Put("example", credential); err != nil {
		t.Fatal(err)
	}

	authorizers := []*Authorizer{
		{Store: NewStore(path), Client: &Client{HTTPClient: server.Client(), Now: time.Now}},
		{Store: NewStore(path), Client: &Client{HTTPClient: server.Client(), Now: time.Now}},
	}
	start := make(chan struct{})
	results := make(chan string, len(authorizers))
	errs := make(chan error, len(authorizers))
	var group sync.WaitGroup
	for _, authorizer := range authorizers {
		group.Add(1)
		go func(authorizer *Authorizer) {
			defer group.Done()
			<-start
			token, err := authorizer.Token(context.Background(), "example", resource, "")
			results <- token
			errs <- err
		}(authorizer)
	}
	close(start)
	group.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for token := range results {
		if token != "new-access" {
			t.Fatalf("token = %q", token)
		}
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh requests = %d, want 1", got)
	}
}
