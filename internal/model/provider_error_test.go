package model

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestProviderErrorRetryClassification(t *testing.T) {
	for _, test := range []struct {
		err  *ProviderError
		want bool
	}{
		{&ProviderError{StatusCode: 429}, true},
		{&ProviderError{StatusCode: 503}, true},
		{&ProviderError{StatusCode: 400}, false},
		{&ProviderError{Code: "overloaded_error"}, true},
		{&ProviderError{Code: "invalid_request_error"}, false},
	} {
		if got := test.err.Retryable(); got != test.want {
			t.Fatalf("%+v retryable=%v, want %v", test.err, got, test.want)
		}
	}
	wrapped := errors.New("plain")
	if ok, _ := RetryInfo(wrapped); ok {
		t.Fatal("plain error is retryable")
	}
	if ok, _ := RetryInfo(timeoutError{}); !ok {
		t.Fatal("network timeout is not retryable")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := ParseRetryAfter("3", now); got != 3*time.Second {
		t.Fatalf("seconds=%v", got)
	}
	if got := ParseRetryAfter(now.Add(5*time.Second).Format(http.TimeFormat), now); got != 5*time.Second {
		t.Fatalf("date=%v", got)
	}
}
