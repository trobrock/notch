package mcpoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/oauth"
)

func TestLoginDiscoversRegistersAndExchangesWithPKCE(t *testing.T) {
	var registered map[string]any
	var exchanged url.Values
	var authorizationQuery url.Values
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			writeJSON(t, w, map[string]any{
				"resource":              "https://" + r.Host + "/mcp",
				"authorization_servers": []string{"https://" + r.Host},
				"scopes_supported":      []string{"read", "write"},
			})
		case "/.well-known/oauth-authorization-server":
			origin := "https://" + r.Host
			writeJSON(t, w, map[string]any{
				"issuer": origin, "authorization_endpoint": origin + "/authorize", "token_endpoint": origin + "/token",
				"registration_endpoint": origin + "/register", "response_types_supported": []string{"code"},
				"grant_types_supported":            []string{"authorization_code", "refresh_token"},
				"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"},
			})
		case "/register":
			if err := json.NewDecoder(r.Body).Decode(&registered); err != nil {
				t.Error(err)
			}
			writeJSON(t, w, map[string]any{"client_id": "notch-client", "token_endpoint_auth_method": "none"})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			exchanged = r.Form
			writeJSON(t, w, map[string]any{
				"access_token": "access", "refresh_token": "refresh", "token_type": "Bearer", "expires_in": 3600, "scope": "read write",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	oauthClient := oauth.NewClient()
	oauthClient.Browser = func(target string) error {
		authorize, err := url.Parse(target)
		if err != nil {
			return err
		}
		authorizationQuery = authorize.Query()
		callback, err := url.Parse(authorize.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		query := callback.Query()
		query.Set("code", "authorization-code")
		query.Set("state", authorize.Query().Get("state"))
		query.Set("iss", server.URL)
		callback.RawQuery = query.Encode()
		response, err := http.Get(strings.Replace(callback.String(), "localhost", "127.0.0.1", 1))
		if err == nil {
			response.Body.Close()
		}
		return err
	}
	client := &Client{HTTPClient: server.Client(), OAuth: oauthClient, Now: func() time.Time { return time.Unix(100, 0) }}
	credential, err := client.Login(context.Background(), server.URL+"/mcp", "", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if credential.ServerURL != server.URL+"/mcp" || credential.Resource != server.URL+"/mcp" || credential.AuthorizationServer != server.URL || credential.ClientID != "notch-client" || credential.AccessToken != "access" || credential.RefreshToken != "refresh" || credential.ExpiresAt != time.Unix(3700, 0).UnixMilli() {
		t.Fatalf("credential = %#v", credential)
	}
	redirects, _ := registered["redirect_uris"].([]any)
	if len(redirects) != 1 || !strings.HasPrefix(redirects[0].(string), "http://localhost:") {
		t.Fatalf("registration = %#v", registered)
	}
	if authorizationQuery.Get("resource") != server.URL+"/mcp" || authorizationQuery.Get("scope") != "read write" || authorizationQuery.Get("code_challenge_method") != "S256" || authorizationQuery.Get("code_challenge") == "" {
		t.Fatalf("authorization query = %#v", authorizationQuery)
	}
	if exchanged.Get("resource") != server.URL+"/mcp" || exchanged.Get("code") != "authorization-code" || exchanged.Get("code_verifier") == "" {
		t.Fatalf("exchange = %#v", exchanged)
	}
}

func TestRefreshPreservesRotatingFields(t *testing.T) {
	var form url.Values
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form = r.Form
		writeJSON(t, w, map[string]any{"access_token": "next", "token_type": "Bearer", "expires_in": 60})
	}))
	defer server.Close()
	client := &Client{HTTPClient: server.Client(), Now: func() time.Time { return time.Unix(10, 0) }}
	credential := Credential{
		ServerURL: "https://resource.test/mcp", Resource: "https://resource.test/mcp", AuthorizationServer: server.URL, TokenEndpoint: server.URL,
		ClientID: "client", TokenAuthMethod: "none", AccessToken: "old", RefreshToken: "refresh", Scope: "read",
	}
	refreshed, err := client.Refresh(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "next" || refreshed.RefreshToken != "refresh" || refreshed.Scope != "read" || refreshed.ExpiresAt != time.Unix(70, 0).UnixMilli() {
		t.Fatalf("refreshed = %#v", refreshed)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("resource") != credential.Resource || form.Get("scope") != "read" {
		t.Fatalf("refresh form = %#v", form)
	}
}

func TestStoreBindsCredentialToResourceAndUsesPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials", "mcp-auth.json")
	store := NewStore(path)
	credential := testCredential("https://example.test/mcp")
	if err := store.Put("example", credential); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %04o", info.Mode().Perm())
	}
	if _, ok, err := store.Get("example", credential.ServerURL); err != nil || !ok {
		t.Fatalf("Get ok=%v err=%v", ok, err)
	}
	if _, _, err := store.Get("example", "https://evil.test/mcp"); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestStoreRejectsUnsafeExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-auth.json")
	data := `{"version":1,"credentials":{}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := NewStore(path).GetAny("example")
	if err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("error = %v", err)
	}
}

func TestProtectedResourceMetadataURLs(t *testing.T) {
	resource, _ := url.Parse("https://example.test/a/b?x=1")
	got := protectedResourceMetadataURLs(resource)
	want := []string{"https://example.test/.well-known/oauth-protected-resource/a/b", "https://example.test/.well-known/oauth-protected-resource"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("URLs = %#v", got)
	}
}

func testCredential(resource string) Credential {
	return Credential{
		ServerURL: resource, Resource: resource, AuthorizationServer: "https://issuer.test", TokenEndpoint: "https://issuer.test/token",
		ClientID: "client", TokenAuthMethod: "none", AccessToken: "access", RefreshToken: "refresh", TokenType: "Bearer",
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}
