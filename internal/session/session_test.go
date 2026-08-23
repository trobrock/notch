package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/trobrock/notch/internal/model"
)

func TestNewAppendLoad(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "/work", "anthropic", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	path := s.Path()
	if filepath.Dir(path) != dir || !strings.HasSuffix(path, ".jsonl") {
		t.Fatalf("unexpected path %q", path)
	}
	message := model.TextMessage("user", "hello")
	if err := s.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEntry("bookmark", map[string]int{"offset": 3}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEntry(map[string]any{"type": "note", "text": "remember"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := s.AppendMessage(message); err == nil {
		t.Fatal("append after close succeeded")
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if loaded.Header.CWD != "/work" || loaded.Header.Provider != "anthropic" || loaded.Header.Model != "test-model" {
		t.Fatalf("bad header: %+v", loaded.Header)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Role != "user" {
		t.Fatalf("bad messages: %+v", loaded.Messages)
	}
	if len(loaded.Entries) != 3 {
		t.Fatalf("got %d entries", len(loaded.Entries))
	}
	var custom CustomEntry
	if err := json.Unmarshal(loaded.Entries[1], &custom); err != nil {
		t.Fatal(err)
	}
	if custom.Type != "bookmark" {
		t.Fatalf("bad custom entry: %+v", custom)
	}
}

func TestLatest(t *testing.T) {
	dir := t.TempDir()
	if _, err := Latest(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Latest(empty) error = %v", err)
	}
	first, err := New(dir, "one", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(dir, "two", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.AppendEntry(map[string]string{"type": "note"}); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	latest, err := Latest(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	if latest.Path() != second.Path() {
		t.Fatalf("latest = %q, want %q", latest.Path(), second.Path())
	}
}

func TestLoadReportsLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	contents := `{"type":"metadata","version":1,"id":"x","created_at":"2025-01-01T00:00:00Z","cwd":"/","provider":"p","model":"m"}` + "\n" +
		`{"type":"message","message":` + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), path) {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestConcurrentAppend(t *testing.T) {
	s, err := New(t.TempDir(), "/", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	const count = 20
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.AppendMessage(model.TextMessage("user", "x")); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if len(loaded.Messages) != count || len(loaded.Entries) != count {
		t.Fatalf("messages=%d entries=%d", len(loaded.Messages), len(loaded.Entries))
	}
}
