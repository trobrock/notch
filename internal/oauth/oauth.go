// Package oauth implements interactive OAuth login for Notch providers.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/trobrock/notch/internal/credentials"
)

const (
	OpenAICodex         = "openai-codex"
	AnthropicClaudeCode = "anthropic-claude-code"
	OpenRouter          = "openrouter"

	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	anthropicClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

// Client holds the transport and endpoints used for OAuth. Endpoint fields are
// exported primarily so tests and private installations can point at a local
// compatible server. Empty fields receive the production defaults in Login or
// Refresh.
type Client struct {
	HTTPClient    *http.Client
	Browser       func(string) error
	CallbackInput io.Reader

	CodexAuthorizeURL     string
	CodexTokenURL         string
	CodexRedirectURL      string
	AnthropicAuthorizeURL string
	AnthropicTokenURL     string
	AnthropicRedirectURL  string
	OpenRouterAuthURL     string
	OpenRouterTokenURL    string
}

// NewClient returns a client configured for the production providers.
func NewClient() *Client {
	return &Client{
		HTTPClient:            http.DefaultClient,
		Browser:               openBrowser,
		CodexAuthorizeURL:     "https://auth.openai.com/oauth/authorize",
		CodexTokenURL:         "https://auth.openai.com/oauth/token",
		CodexRedirectURL:      "http://localhost:1455/auth/callback",
		AnthropicAuthorizeURL: "https://claude.ai/oauth/authorize",
		AnthropicTokenURL:     "https://platform.claude.com/v1/oauth/token",
		AnthropicRedirectURL:  "http://localhost:53692/callback",
		OpenRouterAuthURL:     "https://openrouter.ai/auth",
		OpenRouterTokenURL:    "https://openrouter.ai/api/v1/auth/keys",
	}
}

// DefaultClient is used by the package-level Login and Refresh functions.
var DefaultClient = NewClient()

// Login performs a browser/loopback PKCE login. The authorization URL is
// always written to out, even if launching a browser fails.
func Login(ctx context.Context, provider string, out io.Writer) (credentials.Credential, error) {
	return DefaultClient.Login(ctx, provider, out)
}

// Refresh refreshes an OAuth credential. OpenRouter credentials are permanent
// and are returned unchanged.
func Refresh(ctx context.Context, provider string, credential credentials.Credential) (credentials.Credential, error) {
	return DefaultClient.Refresh(ctx, provider, credential)
}

// Login performs login with c's configured endpoints.
func (c *Client) Login(ctx context.Context, provider string, out io.Writer) (credentials.Credential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = os.Stdout
	}
	switch provider {
	case OpenAICodex:
		return c.loginCodex(ctx, out)
	case AnthropicClaudeCode:
		return c.loginAnthropic(ctx, out)
	case OpenRouter:
		return c.loginOpenRouter(ctx, out)
	default:
		return credentials.Credential{}, fmt.Errorf("unsupported OAuth provider %q", provider)
	}
}

// Refresh refreshes credential with c's configured token endpoint.
func (c *Client) Refresh(ctx context.Context, provider string, credential credentials.Credential) (credentials.Credential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if provider == OpenRouter {
		return credential, nil
	}
	if credential.Refresh == "" {
		return credentials.Credential{}, fmt.Errorf("refresh %s credential: refresh token is empty", provider)
	}
	var token tokenResponse
	var err error
	switch provider {
	case OpenAICodex:
		values := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {credential.Refresh},
			"client_id":     {codexClientID},
		}
		err = c.postForm(ctx, c.value(c.CodexTokenURL, "https://auth.openai.com/oauth/token"), values, &token)
	case AnthropicClaudeCode:
		body := map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": credential.Refresh,
			"client_id":     anthropicClientID,
		}
		err = c.postJSON(ctx, c.value(c.AnthropicTokenURL, "https://platform.claude.com/v1/oauth/token"), body, &token)
	default:
		return credentials.Credential{}, fmt.Errorf("unsupported OAuth provider %q", provider)
	}
	if err != nil {
		return credentials.Credential{}, fmt.Errorf("refresh %s credential: %w", provider, err)
	}
	result, err := credentialFromToken(provider, token)
	if err != nil {
		return credentials.Credential{}, err
	}
	if result.Refresh == "" {
		result.Refresh = credential.Refresh
	}
	if result.AccountID == "" {
		result.AccountID = credential.AccountID
	}
	return result, nil
}

func (c *Client) loginCodex(ctx context.Context, out io.Writer) (credentials.Credential, error) {
	verifier, challenge, err := makePKCE()
	if err != nil {
		return credentials.Credential{}, err
	}
	state, err := randomURLString(24)
	if err != nil {
		return credentials.Credential{}, err
	}
	redirect := c.value(c.CodexRedirectURL, "http://localhost:1455/auth/callback")
	code, actualRedirect, err := c.browserCode(ctx, redirect, state, out, func(callback string) (string, error) {
		u, err := url.Parse(c.value(c.CodexAuthorizeURL, "https://auth.openai.com/oauth/authorize"))
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Set("client_id", codexClientID)
		q.Set("response_type", "code")
		q.Set("redirect_uri", callback)
		q.Set("scope", "openid profile email offline_access")
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		q.Set("state", state)
		q.Set("id_token_add_organizations", "true")
		q.Set("codex_cli_simplified_flow", "true")
		q.Set("originator", "notch")
		u.RawQuery = q.Encode()
		return u.String(), nil
	})
	if err != nil {
		return credentials.Credential{}, fmt.Errorf("openai-codex login: %w", err)
	}
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexClientID},
		"code":          {code},
		"redirect_uri":  {actualRedirect},
		"code_verifier": {verifier},
	}
	var token tokenResponse
	if err := c.postForm(ctx, c.value(c.CodexTokenURL, "https://auth.openai.com/oauth/token"), values, &token); err != nil {
		return credentials.Credential{}, fmt.Errorf("openai-codex token exchange: %w", err)
	}
	return credentialFromToken(OpenAICodex, token)
}

func (c *Client) loginAnthropic(ctx context.Context, out io.Writer) (credentials.Credential, error) {
	verifier, challenge, err := makePKCE()
	if err != nil {
		return credentials.Credential{}, err
	}
	redirect := c.value(c.AnthropicRedirectURL, "http://localhost:53692/callback")
	code, actualRedirect, err := c.browserCode(ctx, redirect, verifier, out, func(callback string) (string, error) {
		u, err := url.Parse(c.value(c.AnthropicAuthorizeURL, "https://claude.ai/oauth/authorize"))
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Set("code", "true")
		q.Set("client_id", anthropicClientID)
		q.Set("response_type", "code")
		q.Set("redirect_uri", callback)
		q.Set("scope", "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload")
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		q.Set("state", verifier)
		u.RawQuery = q.Encode()
		return u.String(), nil
	})
	if err != nil {
		return credentials.Credential{}, fmt.Errorf("anthropic login: %w", err)
	}
	body := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     anthropicClientID,
		"code":          code,
		"redirect_uri":  actualRedirect,
		"code_verifier": verifier,
		"state":         verifier,
	}
	var token tokenResponse
	if err := c.postJSON(ctx, c.value(c.AnthropicTokenURL, "https://platform.claude.com/v1/oauth/token"), body, &token); err != nil {
		return credentials.Credential{}, fmt.Errorf("anthropic token exchange: %w", err)
	}
	return credentialFromToken(AnthropicClaudeCode, token)
}

func (c *Client) loginOpenRouter(ctx context.Context, out io.Writer) (credentials.Credential, error) {
	verifier, challenge, err := makePKCE()
	if err != nil {
		return credentials.Credential{}, err
	}
	code, _, err := c.browserCode(ctx, "http://localhost:0/callback", "", out, func(callback string) (string, error) {
		u, err := url.Parse(c.value(c.OpenRouterAuthURL, "https://openrouter.ai/auth"))
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Set("callback_url", callback)
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		u.RawQuery = q.Encode()
		return u.String(), nil
	})
	if err != nil {
		return credentials.Credential{}, fmt.Errorf("openrouter login: %w", err)
	}
	body := map[string]string{
		"code":                  code,
		"code_verifier":         verifier,
		"code_challenge_method": "S256",
	}
	var response struct {
		Key string `json:"key"`
	}
	if err := c.postJSON(ctx, c.value(c.OpenRouterTokenURL, "https://openrouter.ai/api/v1/auth/keys"), body, &response); err != nil {
		return credentials.Credential{}, fmt.Errorf("openrouter token exchange: %w", err)
	}
	if response.Key == "" {
		return credentials.Credential{}, errors.New("openrouter token exchange: response did not contain a key")
	}
	return credentials.Credential{Type: "api_key", Access: response.Key}, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func credentialFromToken(provider string, token tokenResponse) (credentials.Credential, error) {
	if token.AccessToken == "" {
		return credentials.Credential{}, fmt.Errorf("%s token response did not contain an access token", provider)
	}
	result := credentials.Credential{
		Type:    "oauth",
		Access:  token.AccessToken,
		Refresh: token.RefreshToken,
	}
	if token.ExpiresIn > 0 {
		result.Expires = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UnixMilli()
	}
	if provider == OpenAICodex {
		result.AccountID = jwtAccountID(token.IDToken)
		if result.AccountID == "" {
			result.AccountID = jwtAccountID(token.AccessToken)
		}
	}
	return result, nil
}

const (
	accountIDClaim      = "https://api.openai.com/auth.chatgpt_account_id"
	openAIAuthClaim     = "https://api.openai.com/auth"
	chatGPTAccountField = "chatgpt_account_id"
)

func jwtAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	// OpenAI currently groups private claims below its auth namespace. Accept
	// the flattened spelling as well because older token issuers used it.
	if namespace, ok := claims[openAIAuthClaim].(map[string]any); ok {
		if value, ok := namespace[chatGPTAccountField].(string); ok {
			return value
		}
	}
	value, _ := claims[accountIDClaim].(string)
	return value
}

func makePKCE() (verifier, challenge string, err error) {
	verifier, err = randomURLString(32)
	if err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func randomURLString(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// browserCode starts a dedicated loopback server, emits and opens the URL, and
// waits for a valid callback. redirect may contain port zero for an ephemeral
// port.
func (c *Client) browserCode(ctx context.Context, redirect, expectedState string, out io.Writer, authorizationURL func(string) (string, error)) (string, string, error) {
	callback, err := c.Authorize(ctx, redirect, expectedState, out, authorizationURL)
	if err != nil {
		return "", "", err
	}
	return callback.Code, callback.RedirectURL, nil
}

func (c *Client) postForm(ctx context.Context, endpoint string, values url.Values, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req, dst)
}

func (c *Client) postJSON(ctx context.Context, endpoint string, body any, dst any) error {
	var encoded strings.Builder
	if err := json.NewEncoder(&encoded).Encode(body); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(encoded.String()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, dst)
}

func (c *Client) do(req *http.Request, dst any) error {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("token endpoint returned %s", response.Status)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	return nil
}

func (c *Client) value(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("cmd", "/c", "start", "", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}
