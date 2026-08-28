package oauth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AuthorizationCallback is the validated result of a loopback OAuth callback.
// Query is retained so callers can validate optional authorization response
// parameters such as iss without parsing untrusted browser input themselves.
type AuthorizationCallback struct {
	Code        string
	RedirectURL string
	Query       url.Values
}

// Authorize starts a dedicated loopback callback server, prints and opens the
// authorization URL, and waits for a valid callback. Invalid or unrelated
// requests receive an error response but do not terminate the login attempt.
// redirect may use port zero to request an ephemeral port.
func (c *Client) Authorize(ctx context.Context, redirect, expectedState string, out io.Writer, authorizationURL func(string) (string, error)) (AuthorizationCallback, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}
	parsed, err := url.Parse(redirect)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return AuthorizationCallback{}, fmt.Errorf("invalid loopback redirect %q", redirect)
	}
	hostname := parsed.Hostname()
	if hostname != "localhost" && hostname != "127.0.0.1" && hostname != "::1" {
		return AuthorizationCallback{}, fmt.Errorf("redirect host %q is not loopback", hostname)
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return AuthorizationCallback{}, fmt.Errorf("listen for OAuth callback: %w", err)
	}
	defer listener.Close()
	actualPort := listener.Addr().(*net.TCPAddr).Port
	if parsed.Port() == "0" {
		parsed.Host = net.JoinHostPort("localhost", strconv.Itoa(actualPort))
	}
	actualRedirect := parsed.String()

	type callbackResult struct {
		callback AuthorizationCallback
		err      error
	}
	result := make(chan callbackResult, 1)
	handler := http.NewServeMux()
	handler.HandleFunc(parsed.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Authorization callback requires GET.", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if expectedState != "" && q.Get("state") != expectedState {
			http.Error(w, "Invalid OAuth state. Return to the original authorization tab.", http.StatusBadRequest)
			return
		}
		if oauthErr := q.Get("error"); oauthErr != "" {
			detail := q.Get("error_description")
			if detail == "" {
				detail = oauthErr
			}
			http.Error(w, "Authorization denied. You may close this window.", http.StatusBadRequest)
			select {
			case result <- callbackResult{err: fmt.Errorf("authorization denied: %s", detail)}:
			default:
			}
			return
		}
		if expectedState != "" && q.Get("state") != expectedState {
			http.Error(w, "Invalid OAuth state. Return to the original authorization tab.", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "Authorization callback did not contain a code.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Authorization complete. You may close this window.\n")
		select {
		case result <- callbackResult{callback: AuthorizationCallback{Code: code, RedirectURL: actualRedirect, Query: q}}:
		default:
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serveDone)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-serveDone
	}()

	authURL, err := authorizationURL(actualRedirect)
	if err != nil {
		return AuthorizationCallback{}, fmt.Errorf("build authorization URL: %w", err)
	}
	if _, err := fmt.Fprintf(out, "Open this URL in your browser:\n%s\n", authURL); err != nil {
		return AuthorizationCallback{}, fmt.Errorf("print authorization URL: %w", err)
	}
	if c.CallbackInput != nil {
		if _, err := fmt.Fprintln(out, "If the browser cannot reach this machine, paste the final redirect URL here and press Enter:"); err != nil {
			return AuthorizationCallback{}, fmt.Errorf("print OAuth redirect prompt: %w", err)
		}
		go func() {
			scanner := bufio.NewScanner(c.CallbackInput)
			for scanner.Scan() {
				raw := strings.TrimSpace(scanner.Text())
				if raw == "" {
					continue
				}
				pasted, parseErr := url.Parse(raw)
				if parseErr != nil || !sameRedirectURL(parsed, pasted) {
					_, _ = fmt.Fprintln(out, "That is not the expected OAuth redirect URL; try again:")
					continue
				}
				q := pasted.Query()
				if expectedState != "" && q.Get("state") != expectedState {
					_, _ = fmt.Fprintln(out, "The pasted OAuth redirect has an invalid state; try again:")
					continue
				}
				if oauthErr := q.Get("error"); oauthErr != "" {
					detail := q.Get("error_description")
					if detail == "" {
						detail = oauthErr
					}
					select {
					case result <- callbackResult{err: fmt.Errorf("authorization denied: %s", detail)}:
					default:
					}
					return
				}
				code := q.Get("code")
				if code == "" {
					_, _ = fmt.Fprintln(out, "The pasted OAuth redirect does not contain a code; try again:")
					continue
				}
				select {
				case result <- callbackResult{callback: AuthorizationCallback{Code: code, RedirectURL: actualRedirect, Query: q}}:
				default:
				}
				return
			}
		}()
	}
	browser := c.Browser
	if browser == nil {
		browser = openBrowser
	}
	// Browser launch can fail over SSH. The URL is printed first, so continue
	// waiting for the user to open it manually.
	_ = browser(authURL)

	select {
	case <-ctx.Done():
		return AuthorizationCallback{}, ctx.Err()
	case got := <-result:
		return got.callback, got.err
	}
}

func sameRedirectURL(expected, actual *url.URL) bool {
	return expected != nil && actual != nil && actual.User == nil && actual.Fragment == "" &&
		strings.EqualFold(expected.Scheme, actual.Scheme) && strings.EqualFold(expected.Host, actual.Host) &&
		expected.EscapedPath() == actual.EscapedPath()
}

// MakePKCE returns an RFC 7636 verifier and S256 challenge.
func MakePKCE() (verifier, challenge string, err error) { return makePKCE() }

// RandomURLString returns a cryptographically random base64url string.
func RandomURLString(size int) (string, error) {
	if size <= 0 {
		return "", errors.New("random string size must be positive")
	}
	return randomURLString(size)
}
