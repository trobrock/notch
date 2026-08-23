package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/trobrock/notch/internal/extension"
)

func execute(t *testing.T, tool extension.Tool, args any) (extension.ToolResult, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), raw, nil)
}

func TestRegisterBuiltins(t *testing.T) {
	reg := extension.NewRegistry()
	if err := RegisterBuiltins(reg, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range reg.Tools() {
		names = append(names, tool.Definition.Name)
		if tool.Source != builtinSource {
			t.Errorf("tool %q source = %q", tool.Definition.Name, tool.Source)
		}
	}
	want := []string{"bash", "edit", "find", "grep", "ls", "read", "write"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("registered names = %v, want %v", names, want)
	}
	if err := RegisterBuiltins(reg, t.TempDir()); err == nil {
		t.Fatal("second registration unexpectedly succeeded")
	}
	if err := RegisterBuiltins(nil, ""); err == nil {
		t.Fatal("nil registry unexpectedly succeeded")
	}
}

func TestReadWriteAndEdit(t *testing.T) {
	dir := t.TempDir()
	write := NewWrite(dir)
	if _, err := execute(t, write, map[string]any{
		"path": "nested/file.txt", "content": "one\ntwo\nthree\nfour\n",
	}); err != nil {
		t.Fatal(err)
	}

	read := NewRead(dir)
	result, err := execute(t, read, map[string]any{"path": "nested/file.txt", "offset": 2, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "two\nthree" {
		t.Fatalf("read content = %q", result.Content)
	}

	edit := NewEdit(dir)
	if _, err := execute(t, edit, map[string]any{
		"path": "nested/file.txt", "old_text": "two\nthree", "new_text": "2\n3",
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "nested", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "one\n2\n3\nfour\n" {
		t.Fatalf("edited content = %q", content)
	}

	if _, err := execute(t, edit, map[string]any{
		"path": "nested/file.txt", "old_text": "\n", "new_text": "!",
	}); err == nil || !strings.Contains(err.Error(), "must be unique") {
		t.Fatalf("non-unique edit error = %v", err)
	}
}

func TestReadCapsOutputAndAllowsParentPath(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "base")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("x", OutputLimit+100)
	if err := os.WriteFile(filepath.Join(parent, "outside.txt"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := execute(t, NewRead(base), map[string]any{"path": "../outside.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != OutputLimit {
		t.Fatalf("output length = %d, want %d", len(result.Content), OutputLimit)
	}
	if truncated, _ := result.Details["truncated"].(bool); !truncated {
		t.Fatalf("details = %#v, want truncated", result.Details)
	}
}

func TestSearchAndListTools(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.go":        "package sample\n// needle\n",
		"b.txt":       "needle ignored by glob\n",
		"sub/c.go":    "package sample\nvar needle = true\n",
		"sub/skip.md": "nothing\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	grepResult, err := execute(t, NewGrep(dir), map[string]any{
		"pattern": `\bneedle\b`, "path": ".", "glob": "*.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	grepLines := strings.Split(grepResult.Content, "\n")
	if len(grepLines) != 2 || !strings.HasPrefix(grepLines[0], "a.go:2:") || !strings.HasPrefix(grepLines[1], filepath.Join("sub", "c.go")+":2:") {
		t.Fatalf("grep output = %q", grepResult.Content)
	}

	findResult, err := execute(t, NewFind(dir), map[string]any{"path": ".", "pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	found := strings.Split(findResult.Content, "\n")
	sort.Strings(found)
	wantFound := []string{"a.go", filepath.Join("sub", "c.go")}
	if !reflect.DeepEqual(found, wantFound) {
		t.Fatalf("find output = %v, want %v", found, wantFound)
	}

	lsResult, err := execute(t, NewLS(dir), map[string]any{"path": "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if lsResult.Content != "c.go\nskip.md" {
		t.Fatalf("ls output = %q", lsResult.Content)
	}
}

func TestBashExitAndCancellation(t *testing.T) {
	tool := NewBash(t.TempDir())
	// printf and exit are shell language built-ins, so this test does not depend
	// on optional external commands being installed.
	result, err := execute(t, tool, map[string]any{"command": "printf output; exit 7"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Content != "output" || result.Details["exit_code"] != 7 {
		t.Fatalf("bash result = %#v", result)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw := json.RawMessage(`{"command":"printf unreachable"}`)
	_, err = tool.Execute(ctx, raw, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bash error = %v", err)
	}
}
