package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const sessionHeader = "Mcp-Session-Id"

type httpClient struct {
	url     string
	headers map[string]string
	client  *http.Client
	nextID  atomic.Int64

	mu       sync.RWMutex
	session  string
	protocol string
	closed   bool
}

func newHTTPClient(cfg ServerConfig) *httpClient {
	headers := make(map[string]string, len(cfg.Headers))
	for key, value := range cfg.Headers {
		headers[key] = value
	}
	return &httpClient{
		url:     cfg.URL,
		headers: headers,
		client:  &http.Client{},
	}
}

func (c *httpClient) setProtocolVersion(version string) {
	c.mu.Lock()
	c.protocol = version
	c.mu.Unlock()
}

func (c *httpClient) call(ctx context.Context, method string, params any, dst any) error {
	id := c.nextID.Add(1)
	response, err := c.post(ctx, rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}, id, true)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			// The HTTP request cancellation normally signals the server as well, but
			// send the protocol notification for transports that keep processing it.
			go func(reason string) {
				cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = c.notify(cancelCtx, "notifications/cancelled", map[string]any{
					"requestId": id,
					"reason":    reason,
				})
			}(contextErr.Error())
			return contextErr
		}
		return err
	}
	if err := validateResponse(response, id); err != nil {
		return err
	}
	if response.Error != nil {
		return response.Error
	}
	if dst != nil && len(response.Result) != 0 {
		if err := json.Unmarshal(response.Result, dst); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

func (c *httpClient) notify(ctx context.Context, method string, params any) error {
	message := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", method, params}
	_, err := c.post(ctx, message, 0, false)
	return err
}

func (c *httpClient) post(ctx context.Context, message any, expectedID int64, responseRequired bool) (rpcResponse, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return rpcResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return rpcResponse{}, fmt.Errorf("create MCP HTTP request: %w", err)
	}
	c.applyHeaders(req)
	response, err := c.client.Do(req)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("MCP HTTP request: %w", err)
	}
	defer response.Body.Close()
	c.captureSession(response.Header)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return rpcResponse{}, fmt.Errorf("MCP HTTP server returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	if !responseRequired {
		_, err = io.Copy(io.Discard, response.Body)
		return rpcResponse{}, err
	}

	mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil {
		return rpcResponse{}, fmt.Errorf("invalid MCP HTTP Content-Type: %w", parseErr)
	}
	switch mediaType {
	case "application/json":
		var result rpcResponse
		decoder := json.NewDecoder(response.Body)
		if err := decoder.Decode(&result); err != nil {
			return rpcResponse{}, fmt.Errorf("decode MCP HTTP response: %w", err)
		}
		return result, nil
	case "text/event-stream":
		return readSSEResponse(response.Body, expectedID)
	default:
		return rpcResponse{}, fmt.Errorf("unsupported MCP HTTP Content-Type %q", mediaType)
	}
}

func (c *httpClient) applyHeaders(request *http.Request) {
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	for key, value := range c.headers {
		request.Header.Set(key, value)
	}
	c.mu.RLock()
	session, protocol := c.session, c.protocol
	c.mu.RUnlock()
	if session != "" {
		request.Header.Set(sessionHeader, session)
	}
	if protocol != "" {
		request.Header.Set("MCP-Protocol-Version", protocol)
	}
}

func (c *httpClient) captureSession(header http.Header) {
	if session := header.Get(sessionHeader); session != "" {
		c.mu.Lock()
		c.session = session
		c.mu.Unlock()
	}
}

func readSSEResponse(reader io.Reader, expectedID int64) (rpcResponse, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var data []string
	parseEvent := func() (rpcResponse, bool, error) {
		if len(data) == 0 {
			return rpcResponse{}, false, nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var response rpcResponse
		if err := json.Unmarshal([]byte(payload), &response); err != nil {
			return rpcResponse{}, false, fmt.Errorf("decode MCP SSE data: %w", err)
		}
		if len(response.ID) == 0 || bytes.Equal(response.ID, []byte("null")) {
			return rpcResponse{}, false, nil // progress or another server notification
		}
		want := fmt.Sprintf("%d", expectedID)
		got := strings.Trim(string(response.ID), `"`)
		return response, got == want, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			response, matched, err := parseEvent()
			if err != nil || matched {
				return response, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return rpcResponse{}, fmt.Errorf("read MCP SSE response: %w", err)
	}
	response, matched, err := parseEvent()
	if err != nil {
		return rpcResponse{}, err
	}
	if matched {
		return response, nil
	}
	return rpcResponse{}, errors.New("MCP SSE stream ended without a matching JSON-RPC response")
}

func (c *httpClient) close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	session := c.session
	c.mu.Unlock()
	if session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil)
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	response, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("close MCP HTTP session: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotFound {
		return nil // Session deletion is optional for older implementations.
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("close MCP HTTP session: server returned %s", response.Status)
	}
	return nil
}
