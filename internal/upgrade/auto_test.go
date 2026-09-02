package upgrade

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutomaticChecksAtMostOncePerInterval(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.1.0",
			"assets": []map[string]string{
				{"name": "notch_1.1.0_linux_amd64.tar.gz", "browser_download_url": serverURL(r) + "/asset"},
				{"name": "checksums.txt", "browser_download_url": serverURL(r) + "/checksums"},
			},
		})
	}))
	defer server.Close()

	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "updates", "state.json")
	options := AutomaticOptions{
		Upgrade: Options{
			CurrentVersion: "v1.0.0", APIBaseURL: server.URL,
			GOOS: "linux", GOARCH: "amd64", CheckOnly: true,
		},
		StatePath: statePath,
		Now:       func() time.Time { return now },
	}
	result, checked, err := Automatic(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !checked || !result.Available || requests.Load() != 1 {
		t.Fatalf("first check = (%+v, %v), requests = %d", result, checked, requests.Load())
	}
	if info, err := os.Stat(statePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file = %v, %v", info, err)
	}

	result, checked, err = Automatic(context.Background(), options)
	if err != nil || checked || result != (Result{}) || requests.Load() != 1 {
		t.Fatalf("cached check = (%+v, %v, %v), requests = %d", result, checked, err, requests.Load())
	}

	now = now.Add(DefaultCheckInterval)
	_, checked, err = Automatic(context.Background(), options)
	if err != nil || !checked || requests.Load() != 2 {
		t.Fatalf("later check = (%v, %v), requests = %d", checked, err, requests.Load())
	}
}

func TestAutomaticSkipsDevelopmentBuilds(t *testing.T) {
	for _, version := range []string{"dev", "v1.0.0-3-gabcdef", "v1.0.0-dirty"} {
		t.Run(version, func(t *testing.T) {
			_, checked, err := Automatic(context.Background(), AutomaticOptions{
				Upgrade:   Options{CurrentVersion: version},
				StatePath: filepath.Join(t.TempDir(), "state.json"),
			})
			if err != nil || checked {
				t.Fatalf("Automatic() checked = %v, err = %v", checked, err)
			}
		})
	}
}

func TestAutomaticRecoversFromMalformedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.0.0"}`))
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, checked, err := Automatic(context.Background(), AutomaticOptions{
		Upgrade:   Options{CurrentVersion: "v1.0.0", APIBaseURL: server.URL, CheckOnly: true},
		StatePath: statePath,
	})
	if err != nil || !checked {
		t.Fatalf("Automatic() checked = %v, err = %v", checked, err)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
