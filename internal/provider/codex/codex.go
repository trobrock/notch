// Package codex configures the OpenAI Responses provider for the Codex
// endpoint exposed by the ChatGPT backend.
package codex

import (
	"net/http"
	"strings"

	"github.com/trobrock/notch/internal/model"
	"github.com/trobrock/notch/internal/provider/openai"
)

const defaultBaseURL = "https://chatgpt.com/backend-api"

// Config configures a Codex provider.
type Config struct {
	AccessToken string
	AccountID   string
	BaseURL     string
	HTTPClient  *http.Client
}

// New returns a provider backed by ChatGPT's Codex Responses endpoint.
func New(cfg Config) model.Provider {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	officialEndpoint := baseURL == "" || strings.EqualFold(baseURL, defaultBaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return openai.New(openai.Config{
		APIKey:           cfg.AccessToken,
		BaseURL:          baseURL,
		Endpoint:         "/codex/responses",
		CodexMode:        true,
		OfficialEndpoint: officialEndpoint,
		HTTPClient:       cfg.HTTPClient,
		Headers: map[string]string{
			"ChatGPT-Account-ID": cfg.AccountID,
			"OpenAI-Beta":        "responses=experimental",
			"originator":         "notch",
			"User-Agent":         "notch",
		},
	})
}
