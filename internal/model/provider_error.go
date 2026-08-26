package model

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ProviderError describes a provider failure that callers may be able to
// retry. RetryAfter is populated when the service supplies retry guidance.
type ProviderError struct {
	Message    string
	StatusCode int
	Code       string
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string { return e.Message }

// Retryable reports whether this provider error represents transient service
// pressure or an HTTP failure that is normally safe to retry.
func (e *ProviderError) Retryable() bool {
	if e == nil {
		return false
	}
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly,
		http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	code := strings.ToLower(strings.TrimSpace(e.Code))
	return strings.Contains(code, "overload") || strings.Contains(code, "rate_limit") ||
		strings.Contains(code, "temporarily_unavailable") || strings.Contains(code, "server_error")
}

// RetryInfo extracts retryability and optional server-directed delay.
func RetryInfo(err error) (bool, time.Duration) {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Retryable(), providerErr.RetryAfter
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary(), 0
	}
	return false, 0
}

// ParseRetryAfter accepts the standard delta-seconds and HTTP-date forms.
func ParseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}
