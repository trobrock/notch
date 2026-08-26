package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/credentials"
)

func TestCodexLoginPKCEAndAccountID(t *testing.T) {
	var exchanged url.Values
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		exchanged = r.Form
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access", "refresh_token": "refresh", "expires_in": 3600,
			"id_token": jwt(t, map[string]any{openAIAuthClaim: map[string]any{chatGPTAccountField: "account-123"}}),
		})
	}))
	defer tokenServer.Close()

	client := NewClient()
	client.CodexAuthorizeURL = "https://authorize.test/oauth"
	client.CodexTokenURL = tokenServer.URL
	client.CodexRedirectURL = "http://localhost:0/auth/callback"
	client.Browser = callbackBrowser(t, func(q url.Values) {
		for key, want := range map[string]string{
			"client_id": codexClientID, "scope": "openid profile email offline_access",
			"originator": "notch", "id_token_add_organizations": "true",
			"codex_cli_simplified_flow": "true", "code_challenge_method": "S256",
		} {
			if q.Get(key) != want {
				t.Errorf("authorize %s = %q, want %q", key, q.Get(key), want)
			}
		}
	})
	var output bytes.Buffer
	credential, err := client.Login(context.Background(), OpenAICodex, &output)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Type != "oauth" || credential.Access != "access" || credential.Refresh != "refresh" || credential.AccountID != "account-123" || credential.Expires <= time.Now().UnixMilli() {
		t.Fatalf("credential = %+v", credential)
	}
	if !strings.Contains(output.String(), "https://authorize.test/oauth") {
		t.Fatalf("authorization URL not printed: %q", output.String())
	}
	if exchanged.Get("code") != "test-code" || exchanged.Get("code_verifier") == "" || exchanged.Get("redirect_uri") == "" {
		t.Fatalf("exchange form = %#v", exchanged)
	}
}

func TestAnthropicRefreshUsesJSONAndPreservesRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" || body["client_id"] != anthropicClientID {
			t.Errorf("request = %#v", body)
		}
		fmt.Fprint(w, `{"access_token":"new-access","expires_in":60}`)
	}))
	defer server.Close()
	client := NewClient()
	client.AnthropicTokenURL = server.URL
	got, err := client.Refresh(context.Background(), AnthropicClaudeCode, credentials.Credential{Type: "oauth", Access: "old", Refresh: "old-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Access != "new-access" || got.Refresh != "old-refresh" || got.Type != "oauth" {
		t.Fatalf("refreshed credential = %+v", got)
	}
}

func TestOpenRouterLoginEphemeralCallback(t *testing.T) {
	var exchange map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&exchange); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"key":"sk-or-secret"}`)
	}))
	defer server.Close()
	client := NewClient()
	client.OpenRouterAuthURL = "https://openrouter.test/auth"
	client.OpenRouterTokenURL = server.URL
	client.Browser = callbackBrowser(t, func(q url.Values) {
		callback, err := url.Parse(q.Get("callback_url"))
		if err != nil || callback.Port() == "" || callback.Port() == "0" {
			t.Errorf("callback URL = %q, err=%v", q.Get("callback_url"), err)
		}
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
			t.Errorf("authorize query = %#v", q)
		}
	})
	got, err := client.Login(context.Background(), OpenRouter, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if got != (credentials.Credential{Type: "api_key", Access: "sk-or-secret"}) {
		t.Fatalf("credential = %+v", got)
	}
	if exchange["code"] != "test-code" || exchange["code_verifier"] == "" || exchange["code_challenge_method"] != "S256" {
		t.Fatalf("exchange = %#v", exchange)
	}
}

func TestCallbackIgnoresWrongStateAndWaitsForValidCallback(t *testing.T) {
	client := NewClient()
	client.CodexRedirectURL = "http://localhost:0/auth/callback"
	client.CodexAuthorizeURL = "https://authorize.test/"
	client.Browser = func(target string) error {
		u, _ := url.Parse(target)
		callback, _ := url.Parse(u.Query().Get("redirect_uri"))
		q := callback.Query()
		q.Set("code", "wrong-code")
		q.Set("state", "wrong")
		callback.RawQuery = q.Encode()
		response, err := http.Get(strings.Replace(callback.String(), "localhost", "127.0.0.1", 1))
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("wrong-state status = %d", response.StatusCode)
		}
		q.Set("code", "valid-code")
		q.Set("state", u.Query().Get("state"))
		callback.RawQuery = q.Encode()
		response, err = http.Get(strings.Replace(callback.String(), "localhost", "127.0.0.1", 1))
		if err == nil {
			response.Body.Close()
		}
		return err
	}
	_, err := client.Login(context.Background(), OpenAICodex, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("error = %v", err)
	}
}

func callbackBrowser(t *testing.T, check func(url.Values)) func(string) error {
	t.Helper()
	return func(target string) error {
		u, err := url.Parse(target)
		if err != nil {
			return err
		}
		check(u.Query())
		callbackText := u.Query().Get("redirect_uri")
		if callbackText == "" {
			callbackText = u.Query().Get("callback_url")
		}
		callback, err := url.Parse(callbackText)
		if err != nil {
			return err
		}
		q := callback.Query()
		q.Set("code", "test-code")
		if state := u.Query().Get("state"); state != "" {
			q.Set("state", state)
		}
		callback.RawQuery = q.Encode()
		response, err := http.Get(strings.Replace(callback.String(), "localhost", "127.0.0.1", 1))
		if err != nil {
			return err
		}
		response.Body.Close()
		return nil
	}
}

func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
