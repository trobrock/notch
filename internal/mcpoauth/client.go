// Package mcpoauth implements OAuth 2.1 authorization for remote MCP servers.
package mcpoauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/trobrock/notch/internal/oauth"
)

const maxMetadataSize = 1 << 20

// Credential contains the OAuth client registration and tokens bound to one
// MCP protected resource and authorization server.
type Credential struct {
	ServerURL           string `json:"server_url"`
	Resource            string `json:"resource"`
	AuthorizationServer string `json:"authorization_server"`
	TokenEndpoint       string `json:"token_endpoint"`
	ClientID            string `json:"client_id"`
	ClientSecret        string `json:"client_secret,omitempty"`
	TokenAuthMethod     string `json:"token_endpoint_auth_method,omitempty"`
	AccessToken         string `json:"access_token"`
	RefreshToken        string `json:"refresh_token,omitempty"`
	ExpiresAt           int64  `json:"expires_at,omitempty"`
	Scope               string `json:"scope,omitempty"`
	TokenType           string `json:"token_type,omitempty"`
}

// Client performs MCP OAuth discovery, dynamic client registration, PKCE
// authorization, token exchange, and refresh.
type Client struct {
	HTTPClient *http.Client
	OAuth      *oauth.Client
	Now        func() time.Time
}

// NewClient returns a production MCP OAuth client.
func NewClient() *Client {
	return &Client{HTTPClient: http.DefaultClient, OAuth: oauth.NewClient(), Now: time.Now}
}

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
	BearerMethods        []string `json:"bearer_methods_supported"`
}

type authorizationServerMetadata struct {
	Issuer                 string   `json:"issuer"`
	AuthorizationEndpoint  string   `json:"authorization_endpoint"`
	TokenEndpoint          string   `json:"token_endpoint"`
	RegistrationEndpoint   string   `json:"registration_endpoint"`
	ScopesSupported        []string `json:"scopes_supported"`
	ResponseTypesSupported []string `json:"response_types_supported"`
	GrantTypesSupported    []string `json:"grant_types_supported"`
	CodeChallengeMethods   []string `json:"code_challenge_methods_supported"`
	TokenAuthMethods       []string `json:"token_endpoint_auth_methods_supported"`
}

type clientRegistration struct {
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret"`
	TokenAuthMethod string `json:"token_endpoint_auth_method"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

type Discovery struct {
	Resource            string
	AuthorizationServer string
	TokenEndpoint       string
	Scopes              []string
}

// Discover resolves and validates the protected resource and authorization
// server metadata needed to bind or import an OAuth credential.
func (c *Client) Discover(ctx context.Context, resourceURL string) (Discovery, error) {
	resource, err := parseProtectedURL(resourceURL)
	if err != nil {
		return Discovery{}, err
	}
	resourceMetadata, err := c.discoverProtectedResource(ctx, resource)
	if err != nil {
		return Discovery{}, err
	}
	if err := validateResourceBinding(resource, resourceMetadata.Resource); err != nil {
		return Discovery{}, err
	}
	if len(resourceMetadata.AuthorizationServers) == 0 {
		return Discovery{}, errors.New("MCP protected resource metadata has no authorization server")
	}
	authorizationServer, err := parseHTTPSURL(resourceMetadata.AuthorizationServers[0], "authorization server")
	if err != nil {
		return Discovery{}, err
	}
	metadata, err := c.discoverAuthorizationServer(ctx, authorizationServer)
	if err != nil {
		return Discovery{}, err
	}
	if !sameIssuer(authorizationServer.String(), metadata.Issuer) {
		return Discovery{}, fmt.Errorf("OAuth authorization server issuer mismatch: discovered %q", metadata.Issuer)
	}
	if _, err := parseHTTPSURL(metadata.TokenEndpoint, "token endpoint"); err != nil {
		return Discovery{}, err
	}
	return Discovery{
		Resource: resourceMetadata.Resource, AuthorizationServer: authorizationServer.String(),
		TokenEndpoint: metadata.TokenEndpoint, Scopes: append([]string(nil), resourceMetadata.ScopesSupported...),
	}, nil
}

// Login authorizes access to resourceURL. requestedScope overrides discovery;
// an empty value requests the protected resource's advertised scopes.
func (c *Client) Login(ctx context.Context, resourceURL, requestedScope string, out io.Writer) (Credential, error) {
	resource, err := parseProtectedURL(resourceURL)
	if err != nil {
		return Credential{}, err
	}
	resourceMetadata, err := c.discoverProtectedResource(ctx, resource)
	if err != nil {
		return Credential{}, err
	}
	if err := validateResourceBinding(resource, resourceMetadata.Resource); err != nil {
		return Credential{}, err
	}
	resourceIndicator, err := parseProtectedURL(resourceMetadata.Resource)
	if err != nil {
		return Credential{}, fmt.Errorf("invalid OAuth resource indicator: %w", err)
	}
	if len(resourceMetadata.AuthorizationServers) == 0 {
		return Credential{}, errors.New("MCP protected resource metadata has no authorization server")
	}
	authorizationServer, err := parseHTTPSURL(resourceMetadata.AuthorizationServers[0], "authorization server")
	if err != nil {
		return Credential{}, err
	}
	metadata, err := c.discoverAuthorizationServer(ctx, authorizationServer)
	if err != nil {
		return Credential{}, err
	}
	if !sameIssuer(authorizationServer.String(), metadata.Issuer) {
		return Credential{}, fmt.Errorf("OAuth authorization server issuer mismatch: discovered %q", metadata.Issuer)
	}
	if metadata.AuthorizationEndpoint == "" || metadata.TokenEndpoint == "" || metadata.RegistrationEndpoint == "" {
		return Credential{}, errors.New("OAuth authorization server does not advertise authorization, token, and registration endpoints")
	}
	if !contains(metadata.ResponseTypesSupported, "code") || !contains(metadata.CodeChallengeMethods, "S256") {
		return Credential{}, errors.New("OAuth authorization server does not support authorization code with S256 PKCE")
	}
	if len(metadata.GrantTypesSupported) != 0 && !contains(metadata.GrantTypesSupported, "authorization_code") {
		return Credential{}, errors.New("OAuth authorization server does not support authorization_code")
	}
	if _, err := parseHTTPSURL(metadata.AuthorizationEndpoint, "authorization endpoint"); err != nil {
		return Credential{}, err
	}
	if _, err := parseHTTPSURL(metadata.TokenEndpoint, "token endpoint"); err != nil {
		return Credential{}, err
	}
	if _, err := parseHTTPSURL(metadata.RegistrationEndpoint, "registration endpoint"); err != nil {
		return Credential{}, err
	}
	scope := strings.TrimSpace(requestedScope)
	if scope == "" {
		scope = strings.Join(resourceMetadata.ScopesSupported, " ")
	}
	verifier, challenge, err := oauth.MakePKCE()
	if err != nil {
		return Credential{}, err
	}
	state, err := oauth.RandomURLString(32)
	if err != nil {
		return Credential{}, fmt.Errorf("generate OAuth state: %w", err)
	}
	oauthClient := c.OAuth
	if oauthClient == nil {
		oauthClient = oauth.NewClient()
	}
	var registration clientRegistration
	callback, err := oauthClient.Authorize(ctx, "http://localhost:0/oauth/callback", state, out, func(redirectURI string) (string, error) {
		registration, err = c.register(ctx, metadata, redirectURI, scope)
		if err != nil {
			return "", err
		}
		authorize, err := url.Parse(metadata.AuthorizationEndpoint)
		if err != nil {
			return "", err
		}
		query := authorize.Query()
		query.Set("response_type", "code")
		query.Set("client_id", registration.ClientID)
		query.Set("redirect_uri", redirectURI)
		query.Set("code_challenge", challenge)
		query.Set("code_challenge_method", "S256")
		query.Set("state", state)
		query.Set("resource", resourceIndicator.String())
		if scope != "" {
			query.Set("scope", scope)
		}
		authorize.RawQuery = query.Encode()
		return authorize.String(), nil
	})
	if err != nil {
		return Credential{}, fmt.Errorf("MCP OAuth authorization: %w", err)
	}
	if issuer := callback.Query.Get("iss"); issuer != "" && !sameIssuer(authorizationServer.String(), issuer) {
		return Credential{}, errors.New("OAuth authorization response issuer mismatch")
	}
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {callback.Code},
		"redirect_uri":  {callback.RedirectURL},
		"code_verifier": {verifier},
		"client_id":     {registration.ClientID},
		"resource":      {resourceIndicator.String()},
	}
	var token tokenResponse
	if err := c.tokenRequest(ctx, metadata.TokenEndpoint, registration, values, &token); err != nil {
		return Credential{}, fmt.Errorf("MCP OAuth token exchange: %w", err)
	}
	return c.credential(resource.String(), resourceIndicator.String(), authorizationServer.String(), metadata.TokenEndpoint, registration, scope, token)
}

func (c *Client) register(ctx context.Context, metadata authorizationServerMetadata, redirectURI, scope string) (clientRegistration, error) {
	method := "none"
	if len(metadata.TokenAuthMethods) != 0 && !contains(metadata.TokenAuthMethods, method) {
		return clientRegistration{}, errors.New("OAuth dynamic registration does not support public clients")
	}
	body := map[string]any{
		"client_name":                "Notch",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
	}
	if scope != "" {
		body["scope"] = scope
	}
	var registration clientRegistration
	if err := c.requestJSON(ctx, http.MethodPost, metadata.RegistrationEndpoint, body, &registration); err != nil {
		return clientRegistration{}, fmt.Errorf("register OAuth client: %w", err)
	}
	if registration.ClientID == "" {
		return clientRegistration{}, errors.New("OAuth client registration response has no client_id")
	}
	if registration.TokenAuthMethod == "" {
		registration.TokenAuthMethod = method
	}
	switch registration.TokenAuthMethod {
	case "none", "client_secret_basic", "client_secret_post":
	default:
		return clientRegistration{}, fmt.Errorf("unsupported OAuth token endpoint authentication method %q", registration.TokenAuthMethod)
	}
	return registration, nil
}

// Refresh exchanges credential's refresh token for a current access token.
func (c *Client) Refresh(ctx context.Context, credential Credential) (Credential, error) {
	if err := validateCredential(credential); err != nil {
		return Credential{}, err
	}
	if credential.RefreshToken == "" {
		return Credential{}, errors.New("MCP OAuth credential has no refresh token; run `notch mcp login` again")
	}
	registration := clientRegistration{
		ClientID: credential.ClientID, ClientSecret: credential.ClientSecret, TokenAuthMethod: credential.TokenAuthMethod,
	}
	values := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {credential.RefreshToken},
		"client_id":     {credential.ClientID},
		"resource":      {credential.Resource},
	}
	if credential.Scope != "" {
		values.Set("scope", credential.Scope)
	}
	var token tokenResponse
	if err := c.tokenRequest(ctx, credential.TokenEndpoint, registration, values, &token); err != nil {
		return Credential{}, fmt.Errorf("refresh MCP OAuth token: %w", err)
	}
	if token.RefreshToken == "" {
		token.RefreshToken = credential.RefreshToken
	}
	if token.Scope == "" {
		token.Scope = credential.Scope
	}
	return c.credential(credential.ServerURL, credential.Resource, credential.AuthorizationServer, credential.TokenEndpoint, registration, credential.Scope, token)
}

func (c *Client) credential(serverURL, resource, authorizationServer, tokenEndpoint string, registration clientRegistration, requestedScope string, token tokenResponse) (Credential, error) {
	if token.AccessToken == "" {
		return Credential{}, errors.New("OAuth token response has no access_token")
	}
	if token.TokenType != "" && !strings.EqualFold(token.TokenType, "Bearer") {
		return Credential{}, fmt.Errorf("unsupported OAuth token type %q", token.TokenType)
	}
	scope := token.Scope
	if scope == "" {
		scope = requestedScope
	}
	credential := Credential{
		ServerURL: serverURL, Resource: resource, AuthorizationServer: authorizationServer, TokenEndpoint: tokenEndpoint,
		ClientID: registration.ClientID, ClientSecret: registration.ClientSecret, TokenAuthMethod: registration.TokenAuthMethod,
		AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, Scope: scope, TokenType: "Bearer",
	}
	if token.ExpiresIn > 0 {
		credential.ExpiresAt = c.now().Add(time.Duration(token.ExpiresIn) * time.Second).UnixMilli()
	}
	return credential, nil
}

func (c *Client) tokenRequest(ctx context.Context, endpoint string, registration clientRegistration, values url.Values, dst *tokenResponse) error {
	if _, err := parseHTTPSURL(endpoint, "token endpoint"); err != nil {
		return err
	}
	values = cloneValues(values)
	method := registration.TokenAuthMethod
	if method == "" {
		method = "none"
	}
	if method == "client_secret_post" {
		values.Set("client_secret", registration.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if method == "client_secret_basic" {
		req.SetBasicAuth(registration.ClientID, registration.ClientSecret)
	}
	return c.doJSON(req, dst)
}

func (c *Client) discoverProtectedResource(ctx context.Context, resource *url.URL) (protectedResourceMetadata, error) {
	urls := protectedResourceMetadataURLs(resource)
	var lastErr error
	for _, endpoint := range urls {
		var metadata protectedResourceMetadata
		if err := c.requestJSON(ctx, http.MethodGet, endpoint, nil, &metadata); err != nil {
			lastErr = err
			continue
		}
		return metadata, nil
	}
	return protectedResourceMetadata{}, fmt.Errorf("discover MCP protected resource metadata: %w", lastErr)
}

func (c *Client) discoverAuthorizationServer(ctx context.Context, issuer *url.URL) (authorizationServerMetadata, error) {
	var metadata authorizationServerMetadata
	endpoint := authorizationServerMetadataURL(issuer)
	if err := c.requestJSON(ctx, http.MethodGet, endpoint, nil, &metadata); err != nil {
		return authorizationServerMetadata{}, fmt.Errorf("discover OAuth authorization server metadata: %w", err)
	}
	return metadata, nil
}

func (c *Client) requestJSON(ctx context.Context, method, endpoint string, body any, dst any) error {
	var reader io.Reader
	if body != nil {
		var encoded strings.Builder
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return err
		}
		reader = strings.NewReader(encoded.String())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req, dst)
}

func (c *Client) doJSON(req *http.Request, dst any) error {
	if _, err := parseHTTPSURL(req.URL.String(), "OAuth endpoint"); err != nil {
		return err
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	client = noRedirectClient(client)
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.Request == nil || response.Request.URL == nil || response.Request.URL.Scheme != "https" {
		return errors.New("OAuth request redirected to a non-HTTPS endpoint")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxMetadataSize))
		return fmt.Errorf("server returned %s", response.Status)
	}
	limited := &io.LimitedReader{R: response.Body, N: maxMetadataSize + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode JSON response: %w", err)
	}
	if limited.N <= 0 {
		return errors.New("OAuth JSON response exceeds 1 MiB")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("OAuth JSON response contains multiple values")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("finish OAuth JSON response: %w", err)
	}
	return nil
}

func noRedirectClient(client *http.Client) *http.Client {
	cloned := *client
	cloned.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &cloned
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func protectedResourceMetadataURLs(resource *url.URL) []string {
	origin := &url.URL{Scheme: resource.Scheme, Host: resource.Host}
	wellKnown := *origin
	wellKnown.Path = "/.well-known/oauth-protected-resource"
	if resource.EscapedPath() != "" && resource.EscapedPath() != "/" {
		wellKnown.RawPath = strings.TrimSuffix(wellKnown.EscapedPath(), "/") + resource.EscapedPath()
		wellKnown.Path = strings.TrimSuffix(wellKnown.Path, "/") + resource.Path
	}
	urls := []string{wellKnown.String()}
	root := *origin
	root.Path = "/.well-known/oauth-protected-resource"
	if root.String() != urls[0] {
		urls = append(urls, root.String())
	}
	return urls
}

func authorizationServerMetadataURL(issuer *url.URL) string {
	endpoint := *issuer
	issuerPath := strings.TrimSuffix(issuer.EscapedPath(), "/")
	endpoint.Path = path.Join("/.well-known/oauth-authorization-server", issuer.Path)
	endpoint.RawPath = ""
	if issuerPath == "" {
		endpoint.Path = "/.well-known/oauth-authorization-server"
	}
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	return endpoint.String()
}

func parseProtectedURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid MCP OAuth resource URL %q", raw)
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return nil, errors.New("MCP OAuth resource URL must use HTTPS (except loopback testing)")
	}
	return parsed, nil
}

func parseHTTPSURL(raw, name string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid HTTPS %s %q", name, raw)
	}
	return parsed, nil
}

func validateResourceBinding(configured *url.URL, advertised string) error {
	if advertised == "" {
		return errors.New("MCP protected resource metadata has no resource identifier")
	}
	parsed, err := parseProtectedURL(advertised)
	if err != nil {
		return fmt.Errorf("invalid protected resource metadata: %w", err)
	}
	if parsed.Scheme != configured.Scheme || !strings.EqualFold(parsed.Host, configured.Host) {
		return errors.New("MCP protected resource metadata is not bound to the configured server origin")
	}
	configuredPath := strings.TrimSuffix(configured.EscapedPath(), "/") + "/"
	advertisedPath := strings.TrimSuffix(parsed.EscapedPath(), "/") + "/"
	if !strings.HasPrefix(configuredPath, advertisedPath) && !strings.HasPrefix(advertisedPath, configuredPath) {
		return errors.New("MCP protected resource metadata path does not match the configured server")
	}
	return nil
}

func validateCredential(credential Credential) error {
	if credential.ServerURL == "" || credential.Resource == "" || credential.AuthorizationServer == "" || credential.TokenEndpoint == "" || credential.ClientID == "" || credential.AccessToken == "" {
		return errors.New("MCP OAuth credential is incomplete; run `notch mcp login` again")
	}
	if _, err := parseProtectedURL(credential.ServerURL); err != nil {
		return err
	}
	if _, err := parseProtectedURL(credential.Resource); err != nil {
		return err
	}
	if _, err := parseHTTPSURL(credential.AuthorizationServer, "authorization server"); err != nil {
		return err
	}
	if _, err := parseHTTPSURL(credential.TokenEndpoint, "token endpoint"); err != nil {
		return err
	}
	resourceURL, err := url.Parse(credential.Resource)
	if err != nil {
		return err
	}
	authorizationURL, err := url.Parse(credential.AuthorizationServer)
	if err != nil {
		return err
	}
	if !strings.EqualFold(resourceURL.Scheme, "https") || !strings.EqualFold(authorizationURL.Scheme, "https") {
		return errors.New("MCP OAuth resource and authorization server must use HTTPS")
	}
	switch credential.TokenAuthMethod {
	case "", "none":
	case "client_secret_basic", "client_secret_post":
		if credential.ClientSecret == "" {
			return fmt.Errorf("MCP OAuth credential uses %s without a client secret", credential.TokenAuthMethod)
		}
	default:
		return fmt.Errorf("unsupported OAuth token endpoint authentication method %q", credential.TokenAuthMethod)
	}
	return nil
}

func sameIssuer(expected, actual string) bool {
	a, errA := url.Parse(expected)
	b, errB := url.Parse(actual)
	if errA != nil || errB != nil {
		return false
	}
	a.Fragment, b.Fragment = "", ""
	a.RawQuery, b.RawQuery = "", ""
	a.Path = strings.TrimSuffix(a.Path, "/")
	b.Path = strings.TrimSuffix(b.Path, "/")
	return subtle.ConstantTimeCompare([]byte(a.String()), []byte(b.String())) == 1
}

func isLoopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, value := range values {
		cloned[key] = append([]string(nil), value...)
	}
	return cloned
}

func credentialKey(resource string) string {
	digest := sha256.Sum256([]byte(resource))
	return hex.EncodeToString(digest[:])
}
