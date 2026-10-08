package pricing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trobrock/notch/internal/model"
)

const fixture = `{"openai":{"models":{
"new-model":{"cost":{"input":1,"output":2,"cache_read":0,"cache_write":3}},
"missing-cache":{"cost":{"input":1,"output":2}},
"gpt-5":{"cost":{"input":1,"output":2}},
"tiered":{"cost":{"input":1,"output":2,"cache_read":0.1,"tiers":[
{"input":4,"output":8,"cache_read":0.4,"tier":{"type":"context","size":300000}},
{"input":2,"output":4,"cache_read":0.2,"tier":{"type":"context","size":100000}}
]}},
"legacy":{"cost":{"input":1,"output":2,"context_over_200k":{"input":3,"output":6}}},
"invalid":{"cost":{"input":-1,"output":2}},
"invalid-cache":{"cost":{"input":1,"output":2,"cache_read":-1}},
"invalid-output":{"cost":{"input":1}},
"invalid-type":{"cost":{"input":"free","output":2}},
"invalid-tier":{"cost":{"input":1,"output":2,"tiers":[{"input":-1,"output":2,"tier":{"type":"context","size":100000}}]}},
"missing-tier-cache":{"cost":{"input":1,"output":2,"cache_read":0.1,"tiers":[{"input":2,"output":4,"tier":{"type":"context","size":100000}}]}},
"zero":{"cost":{"input":0,"output":0,"cache_read":0,"cache_write":0}}
}},"anthropic":{"models":{"new-claude":{"cost":{"input":3,"output":15,"cache_read":0.3,"cache_write":3.75}}}}}`

func fixtureService(t *testing.T, body string) (*Service, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	s := NewService(filepath.Join(t.TempDir(), "cache", "api.json"), time.Hour, false)
	s.url = server.URL
	s.client = server.Client()
	return s, calls
}

func wantEstimate(t *testing.T, s *Service, provider, id, retention string, usage model.Response, want float64, version string, known bool) {
	t.Helper()
	got, gotVersion, ok := s.Estimate(provider, id, retention, usage)
	if ok != known || gotVersion != version || math.Abs(got-want) > 1e-9 {
		t.Fatalf("Estimate(%s, %s) = %g, %q, %v; want %g, %q, %v", provider, id, got, gotVersion, ok, want, version, known)
	}
}

func fixtureVersion() string {
	return fmt.Sprintf("models.dev-sha256:%x", sha256.Sum256([]byte(fixture)))
}

func TestServiceRefreshCacheOfflineForceAndVersion(t *testing.T) {
	s, calls := fixtureService(t, fixture)
	usage := model.Response{InputTokens: 1_000_000}
	wantEstimate(t, s, "openai", "new-model", "short", usage, 0, "", false)
	if err := s.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	wantEstimate(t, s, "openai-codex", "new-model-20260101", "short", usage, 1, fixtureVersion(), true)
	if err := s.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("fresh cache fetched %d times", calls.Load())
	}
	if err := s.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("force fetched %d times", calls.Load())
	}
	data, err := os.ReadFile(s.path)
	if err != nil || string(data) != fixture {
		t.Fatalf("cache = %q, %v", data, err)
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(s.path), ".pricing-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary cache files: %v, %v", files, err)
	}

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.path, old, old); err != nil {
		t.Fatal(err)
	}
	offline := NewService(s.path, time.Hour, false)
	// A stale disk snapshot must work immediately, without a network request.
	wantEstimate(t, offline, "openai", "new-model", "short", usage, 1, fixtureVersion(), true)
	offline.url = ":invalid"
	if err := offline.Refresh(context.Background(), false); err == nil {
		t.Fatal("expected offline error")
	}
	wantEstimate(t, offline, "openai", "new-model", "short", usage, 1, fixtureVersion(), true)

	other := NewService("", time.Hour, false)
	wantEstimate(t, other, "openai", "new-model", "short", usage, 0, "", false)
	wantEstimate(t, other, "openai", "gpt-5", "short", usage, 1.25, Version, true)

	disabled := NewService(s.path, time.Hour, true)
	disabled.url = ":invalid"
	if err := disabled.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	wantEstimate(t, disabled, "openai", "new-model", "short", usage, 0, "", false)
	wantEstimate(t, disabled, "openai", "gpt-5", "short", usage, 1.25, Version, true)
}

func TestServiceRatesTiersAndFallback(t *testing.T) {
	s, _ := fixtureService(t, fixture)
	if err := s.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	version := fixtureVersion()
	for _, tc := range []struct {
		tokens int
		want   float64
	}{
		{100000, .1}, {100001, .200002}, {300000, .6}, {300001, 1.200004},
	} {
		wantEstimate(t, s, "openai", "tiered", "short", model.Response{InputTokens: tc.tokens}, tc.want, version, true)
	}
	// Cached input contributes to context-tier selection.
	wantEstimate(t, s, "openai", "tiered", "short", model.Response{InputTokens: 50000, CacheReadTokens: 100000, OutputTokens: 10000}, .16, version, true)
	wantEstimate(t, s, "openai", "legacy", "short", model.Response{InputTokens: 200001}, .600003, version, true)
	wantEstimate(t, s, "openai", "missing-cache", "short", model.Response{InputTokens: 1000000}, 1, version, true)
	wantEstimate(t, s, "openai", "missing-cache", "short", model.Response{CacheReadTokens: 1}, 0, "", false)
	wantEstimate(t, s, "openai", "missing-cache", "short", model.Response{CacheWriteTokens: 1}, 0, "", false)
	wantEstimate(t, s, "openai", "missing-tier-cache", "short", model.Response{CacheReadTokens: 100001}, 0, "", false)
	wantEstimate(t, s, "openai", "gpt-5", "short", model.Response{CacheReadTokens: 1000000}, .125, Version, true)
	wantEstimate(t, s, "openai", "new-model", "short", model.Response{CacheReadTokens: 1000000}, 0, version, true)
	wantEstimate(t, s, "openai", "zero", "short", model.Response{InputTokens: 1000000, OutputTokens: 1000000, CacheReadTokens: 1, CacheWriteTokens: 1}, 0, version, true)
	for _, id := range []string{"invalid", "invalid-cache", "invalid-output", "invalid-type", "invalid-tier"} {
		wantEstimate(t, s, "openai", id, "short", model.Response{InputTokens: 1}, 0, "", false)
	}
	wantEstimate(t, s, "anthropic", "new-claude", "short", model.Response{CacheWriteTokens: 1000000}, 3.75, version, true)
	wantEstimate(t, s, "anthropic-claude-code", "new-claude", "long", model.Response{CacheWriteTokens: 1000000}, 6, version, true)
	wantEstimate(t, s, "openai", "tiered", "short", model.Response{InputTokens: -1}, 0, "", false)
	wantEstimate(t, s, "openrouter", "openai/new-model", "short", model.Response{InputTokens: 1}, 0, "", false)
}

func TestServiceRefreshErrorsPreserveCache(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"http", fixture, 503}, {"json", `{`, 200}, {"empty", `{}`, 200},
		{"invalid-prices", `{"openai":{"models":{"bad":{"cost":{"input":-1,"output":2}}}}}`, 200},
		{"oversize", strings.Repeat(" ", maxSnapshotBytes+1), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "api.json")
			if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			s := NewService(path, 0, false)
			s.url, s.client = server.URL, server.Client()
			if err := s.Refresh(context.Background(), true); err == nil {
				t.Fatal("expected refresh error")
			}
			wantEstimate(t, s, "openai", "new-model", "short", model.Response{InputTokens: 1000000}, 1, fixtureVersion(), true)
			data, err := os.ReadFile(path)
			if err != nil || string(data) != fixture {
				t.Fatalf("old cache changed: %v", err)
			}
		})
	}
}

func TestServiceCorruptCacheCancellationAndWriteError(t *testing.T) {
	s, _ := fixtureService(t, fixture)
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, []byte(`{`), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := NewService(s.path, time.Hour, false)
	wantEstimate(t, corrupt, "openai", "gpt-5", "short", model.Response{InputTokens: 1000000}, 1.25, Version, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Refresh(ctx, true); err == nil {
		t.Fatal("expected canceled error")
	}
	// The target is a directory, so rename must fail without exposing new rates.
	s.path = t.TempDir()
	if err := s.Refresh(context.Background(), true); err == nil {
		t.Fatal("expected cache write error")
	}
	wantEstimate(t, s, "openai", "new-model", "short", model.Response{InputTokens: 1}, 0, "", false)
}

func TestServiceVersionChangesAndConcurrentRefresh(t *testing.T) {
	var calls atomic.Int64
	var body atomic.Value
	body.Store(fixture)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, body.Load().(string)) }))
	defer server.Close()
	s := NewService("", time.Hour, false)
	s.url, s.client = server.URL, server.Client()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Refresh(context.Background(), false); err != nil {
				t.Error(err)
			}
			wantEstimate(t, s, "openai", "new-model", "short", model.Response{InputTokens: 1000000}, 1, fixtureVersion(), true)
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent refresh fetched %d times", calls.Load())
	}
	// Attribution is the raw snapshot hash, not a retrieval timestamp.
	body.Store(fixture + "\n")
	if err := s.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	wantEstimate(t, s, "openai", "new-model", "short", model.Response{InputTokens: 1000000}, 1, fmt.Sprintf("models.dev-sha256:%x", sha256.Sum256([]byte(fixture+"\n"))), true)
}

func TestSnapshotReaderBound(t *testing.T) {
	if _, err := readSnapshot(bytes.NewReader(make([]byte, maxSnapshotBytes+1))); err == nil {
		t.Fatal("expected size error")
	}
}

func TestUnsupportedTierIsNotEstimated(t *testing.T) {
	snapshot, err := parseSnapshot([]byte(`{"openai":{"models":{"safe":{"cost":{"input":1,"output":2}},"unsafe":{"cost":{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"unknown","size":100}}]}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{snapshot: snapshot}
	if _, _, ok := s.Estimate("openai", "unsafe", "short", model.Response{InputTokens: 200}); ok {
		t.Fatal("unsupported tier must not use base rates")
	}
}
