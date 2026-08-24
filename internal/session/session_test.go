package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestCompactionResetRoundTrip(t *testing.T) {
	s, err := New(t.TempDir(), "/", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(model.TextMessage("user", "old question")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(model.TextMessage("assistant", "old answer")); err != nil {
		t.Fatal(err)
	}

	compacted := []model.Message{{
		Role: "user",
		Content: []model.Block{{
			Type: "tool_use", Name: "lookup", Arguments: json.RawMessage(`{"q":"original"}`),
		}},
	}}
	if err := s.AppendCompaction("summary", compacted, true); err != nil {
		t.Fatal(err)
	}
	// The effective context must not retain caller-owned slices.
	compacted[0].Role = "changed"
	compacted[0].Content[0].Name = "changed"
	compacted[0].Content[0].Arguments[0] = '['
	if len(s.Messages) != 1 || s.Messages[0].Role != "user" || s.Messages[0].Content[0].Name != "lookup" || string(s.Messages[0].Content[0].Arguments) != `{"q":"original"}` {
		t.Fatalf("compaction did not install an independent context: %+v", s.Messages)
	}

	if err := s.AppendMessage(model.TextMessage("assistant", "after compaction")); err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("messages after compaction append = %d, want 2", len(s.Messages))
	}
	if err := s.AppendReset(); err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 0 {
		t.Fatalf("messages after reset: %+v", s.Messages)
	}
	final := model.TextMessage("user", "new conversation")
	if err := s.AppendMessage(final); err != nil {
		t.Fatal(err)
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if !reflect.DeepEqual(loaded.Messages, []model.Message{final}) {
		t.Fatalf("loaded effective messages = %+v, want %+v", loaded.Messages, []model.Message{final})
	}
	if len(loaded.Entries) != 6 {
		t.Fatalf("loaded entries = %d, want 6", len(loaded.Entries))
	}
	var compaction CompactionEntry
	if err := json.Unmarshal(loaded.Entries[2], &compaction); err != nil {
		t.Fatal(err)
	}
	if compaction.Type != "compaction" || compaction.Summary != "summary" || !compaction.Auto || len(compaction.Messages) != 1 {
		t.Fatalf("bad compaction entry: %+v", compaction)
	}
	if compaction.Timestamp.IsZero() {
		t.Fatal("compaction timestamp is zero")
	}
	var reset ResetEntry
	if err := json.Unmarshal(loaded.Entries[4], &reset); err != nil {
		t.Fatal(err)
	}
	if reset.Type != "reset" || reset.Timestamp.IsZero() {
		t.Fatalf("bad reset entry: %+v", reset)
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

func TestListAndResolve(t *testing.T) {
	dir := t.TempDir()
	first, err := New(dir, "/work/one", "p", "first-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.AppendMessage(model.TextMessage("user", "first prompt with useful context")); err != nil {
		t.Fatal(err)
	}
	firstPath, firstID := first.Path(), first.Header.ID
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(dir, "/work/two", "p", "second-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.AppendMessage(model.TextMessage("user", strings.Repeat("long ", 30))); err != nil {
		t.Fatal(err)
	}
	secondPath := second.Path()
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(firstPath, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(secondPath, now, now); err != nil {
		t.Fatal(err)
	}

	infos, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Path != secondPath || infos[1].Path != firstPath {
		t.Fatalf("session order = %#v", infos)
	}
	if infos[0].MessageCount != 1 || len([]rune(infos[0].Preview)) > 80 || !strings.HasSuffix(infos[0].Preview, "…") {
		t.Fatalf("session info = %#v", infos[0])
	}
	prefix := firstID[:min(24, len(firstID))]
	resolved, err := Resolve(dir, prefix)
	if err != nil || resolved != firstPath {
		t.Fatalf("Resolve(%q) = %q, %v", prefix, resolved, err)
	}
	resolved, err = Resolve(dir, filepath.Base(secondPath))
	if err != nil || resolved != secondPath {
		t.Fatalf("Resolve(filename) = %q, %v", resolved, err)
	}
	if _, err := Resolve(dir, "missing"); err == nil {
		t.Fatal("missing session resolved")
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

func TestLoadStrictCompactionAndResetErrors(t *testing.T) {
	header := `{"type":"metadata","version":1,"id":"x","created_at":"2025-01-01T00:00:00Z","cwd":"/","provider":"p","model":"m"}` + "\n"
	for _, test := range []struct {
		name, entry, want string
	}{
		{"compaction", `{"type":"compaction","timestamp":"2025-01-01T00:00:00Z","summary":"s","messages":[],"auto":false,"extra":1}`, "invalid compaction entry"},
		{"reset", `{"type":"reset","timestamp":"2025-01-01T00:00:00Z","extra":1}`, "invalid reset entry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.jsonl")
			if err := os.WriteFile(path, []byte(header+test.entry+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unhelpful error: %v", err)
			}
		})
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

func TestConcurrentMessageCompactionAndReset(t *testing.T) {
	s, err := New(t.TempDir(), "/", "p", "m")
	if err != nil {
		t.Fatal(err)
	}
	const count = 60
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			switch i % 3 {
			case 0:
				err = s.AppendMessage(model.TextMessage("user", "message"))
			case 1:
				err = s.AppendCompaction("summary", []model.Message{model.TextMessage("user", "compacted")}, i%2 == 0)
			case 2:
				err = s.AppendReset()
			}
			if err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// Every operation updates memory under the same lock as its durable append,
	// so replaying the file must produce the identical effective context.
	want := cloneMessages(s.Messages)
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if len(loaded.Entries) != count {
		t.Fatalf("entries = %d, want %d", len(loaded.Entries), count)
	}
	if !reflect.DeepEqual(loaded.Messages, want) {
		t.Fatalf("loaded messages = %+v, in-memory messages = %+v", loaded.Messages, want)
	}
}
