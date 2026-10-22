package intercom

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionChangeRetainsProjectAndMode(t *testing.T) {
	base, err := os.MkdirTemp("", "notch-ic-life-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	t.Setenv("NOTCH_INTERCOM_DIR", base)
	workspace := t.TempDir()
	cwd := filepath.Join(workspace, "nested")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	s := newService()
	host := &fakeHost{configs: []savedConfig{{Enabled: true}}}
	if _, err := s.startSession(context.Background(), host, map[string]any{"session_id": "first", "cwd": cwd, "workspace_root": workspace, "mode": "tui"}); err != nil {
		t.Fatal(err)
	}
	defer s.closeEndpoint()
	expectedRoot, expectedProject, err := runtimeRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	root, project := s.root, s.project
	s.mu.Unlock()
	if root != expectedRoot || project != expectedProject {
		t.Fatalf("namespace used cwd instead of workspace: root=%q project=%q", root, project)
	}
	if _, err := s.startSession(context.Background(), host, map[string]any{"session_id": "second"}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID != "second" || s.root != root || s.project != project || s.mode != "tui" || s.listener == nil {
		t.Fatalf("session change state = id=%q root=%q project=%q mode=%q listener=%v", s.sessionID, s.root, s.project, s.mode, s.listener != nil)
	}
}
