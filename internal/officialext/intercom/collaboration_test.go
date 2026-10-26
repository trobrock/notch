package intercom

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollaborationScopeIsSharedAcrossProjects(t *testing.T) {
	base := testRoot(t)
	t.Setenv("NOTCH_INTERCOM_DIR", base)
	projectA := filepath.Join(t.TempDir(), "notch")
	projectB := filepath.Join(t.TempDir(), "developerly")
	if err := os.MkdirAll(projectA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectB, 0o755); err != nil {
		t.Fatal(err)
	}

	projectRootA, projectIDA, _, err := scopeRoot(projectA, "")
	if err != nil {
		t.Fatal(err)
	}
	projectRootB, projectIDB, _, err := scopeRoot(projectB, "")
	if err != nil {
		t.Fatal(err)
	}
	if projectRootA == projectRootB || projectIDA == projectIDB {
		t.Fatal("unrelated projects unexpectedly share the default intercom scope")
	}

	roomRootA, roomIDA, workspaceA, err := scopeRoot(projectA, "platform-work")
	if err != nil {
		t.Fatal(err)
	}
	roomRootB, roomIDB, workspaceB, err := scopeRoot(projectB, "platform-work")
	if err != nil {
		t.Fatal(err)
	}
	if roomRootA != roomRootB || roomIDA != roomIDB {
		t.Fatalf("collaboration differs across projects: %q/%q and %q/%q", roomRootA, roomIDA, roomRootB, roomIDB)
	}
	if workspaceA == workspaceB {
		t.Fatalf("workspace labels should identify each project, both were %q", workspaceA)
	}
	if !strings.Contains(roomRootA, string(filepath.Separator)+"collaboration"+string(filepath.Separator)) {
		t.Fatalf("collaboration root = %q", roomRootA)
	}
}

func TestCollaborationDeliversAcrossProjectsWithQualifiedTarget(t *testing.T) {
	base := testRoot(t)
	t.Setenv("NOTCH_INTERCOM_DIR", base)
	projectA := filepath.Join(t.TempDir(), "notch")
	projectB := filepath.Join(t.TempDir(), "developerly")
	if err := os.MkdirAll(projectA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectB, 0o755); err != nil {
		t.Fatal(err)
	}
	aHost := &fakeHost{configs: []savedConfig{{Enabled: true, Name: "worker", Collaboration: "platform-work"}}}
	bHost := &fakeHost{configs: []savedConfig{{Enabled: true, Name: "worker", Collaboration: "platform-work"}}}
	a, b := newService(), newService()
	defer a.closeEndpoint()
	defer b.closeEndpoint()
	if _, err := a.startSession(context.Background(), aHost, map[string]any{"session_id": "a", "cwd": projectA, "mode": "tui"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.startSession(context.Background(), bHost, map[string]any{"session_id": "b", "cwd": projectB, "mode": "tui"}); err != nil {
		t.Fatal(err)
	}

	peers, err := a.peers(context.Background())
	if err != nil || len(peers) != 1 {
		t.Fatalf("peers = %#v, %v", peers, err)
	}
	target := peers[0].Workspace + "/" + peers[0].Name
	result, err := a.sendAutomated(context.Background(), target, "review this", false)
	if err != nil || !result.Delivered {
		t.Fatalf("send = %#v, %v", result, err)
	}
	if len(bHost.entries) != 1 || bHost.entries[0].FromWorkspace != a.workspace {
		t.Fatalf("received entries = %#v", bHost.entries)
	}
}

func TestConfigureToolSetsCollaborationIdentityAndOptIns(t *testing.T) {
	base := testRoot(t)
	t.Setenv("NOTCH_INTERCOM_DIR", base)
	workspace := filepath.Join(t.TempDir(), "notch")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	s := newService()
	defer s.closeEndpoint()
	if _, err := s.startSession(context.Background(), host, map[string]any{"session_id": "session", "cwd": workspace, "mode": "tui"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"name": "notch-lead", "collaboration": "platform-work", "enabled": true, "wake": true,
	})
	result, err := s.configureTool(context.Background(), raw, nil)
	if err != nil || result.IsError {
		t.Fatalf("configure = %#v, %v", result, err)
	}
	if !strings.Contains(result.Content, "scope collaboration platform-work") {
		t.Fatalf("status = %q", result.Content)
	}
	s.mu.Lock()
	gotName, gotCollaboration, gotEnabled, gotWake, ready := s.name, s.collaboration, s.enabled, s.wake, s.listener != nil
	s.mu.Unlock()
	if gotName != "notch-lead" || gotCollaboration != "platform-work" || !gotEnabled || !gotWake || !ready {
		t.Fatalf("configured state = name=%q collaboration=%q enabled=%t wake=%t ready=%t", gotName, gotCollaboration, gotEnabled, gotWake, ready)
	}
	if len(host.configs) != 4 {
		t.Fatalf("saved configs = %#v", host.configs)
	}
	last := host.configs[len(host.configs)-1]
	if !last.Enabled || !last.Wake || last.Name != "notch-lead" || last.Collaboration != "platform-work" {
		t.Fatalf("last saved config = %#v", last)
	}
}

func TestMalformedConfigEntryDoesNotPartiallyRestore(t *testing.T) {
	valid := json.RawMessage(`{"enabled":false,"wake":false,"name":"lead","collaboration":"platform-work"}`)
	malformed := json.RawMessage(`{"enabled":true,"wake":"bad"}`)
	cfg := loadConfig([]json.RawMessage{valid, malformed})
	if cfg.Enabled || cfg.Wake || cfg.Name != "lead" || cfg.Collaboration != "platform-work" {
		t.Fatalf("restored config = %#v", cfg)
	}
}

func TestRetryAndReplyDoNotCrossScopes(t *testing.T) {
	s := newService()
	s.host, s.sessionID, s.project = &fakeHost{}, "sender", "new-scope"
	s.outbox = []outboundEntry{{ID: "out", Scope: "old-scope", ToSession: "peer", Body: "secret"}}
	if _, err := s.retry(context.Background(), "out"); err == nil || !strings.Contains(err.Error(), "different intercom scope") {
		t.Fatalf("retry error = %v", err)
	}
	s.inbox = []inboundEntry{{ID: "in", Scope: "old-scope", FromSession: "peer", Body: "request"}}
	if _, err := s.resolveReply(context.Background(), "in", "secret"); err == nil || !strings.Contains(err.Error(), "different intercom scope") {
		t.Fatalf("reply error = %v", err)
	}
}

func TestDisabledServiceRejectsDelivery(t *testing.T) {
	s := newService()
	s.project, s.sessionID, s.enabled = "project", "receiver-session", false
	result := s.receive(context.Background(), delivery("disabled", ""))
	if result.Delivered || !strings.Contains(result.Error, "disabled") {
		t.Fatalf("delivery result = %#v", result)
	}
}

func TestEnableDoesNotAcceptBeforePersistence(t *testing.T) {
	root := testRoot(t)
	release := make(chan struct{})
	started := make(chan struct{})
	host := &fakeHost{appendWait: release, appendStarted: started}
	s := newService()
	s.host, s.root, s.project, s.sessionID, s.name, s.mode = host, root, "project", "receiver-session", "receiver", "tui"
	defer s.closeEndpoint()
	done := make(chan error, 1)
	go func() {
		_, err := s.setEnabled(context.Background(), true)
		done <- err
	}()
	<-started
	result, err := exchange(context.Background(), s.endpoint, delivery("during-enable", ""))
	if err != nil {
		t.Fatal(err)
	}
	if result.Delivered || !strings.Contains(result.Error, "disabled") {
		t.Fatalf("delivery during enable = %#v", result)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFailedDisableRestoresAcceptance(t *testing.T) {
	root := testRoot(t)
	host := &fakeHost{appendErr: context.Canceled}
	s := startTestService(t, root, "project", "receiver-session", "receiver", false, host)
	if _, err := s.setEnabled(context.Background(), false); err == nil {
		t.Fatal("disable unexpectedly succeeded")
	}
	host.appendErr = nil
	result, err := exchange(context.Background(), s.endpoint, delivery("after-failed-disable", ""))
	if err != nil || !result.Delivered {
		t.Fatalf("delivery after failed disable = %#v, %v", result, err)
	}
}

func TestLeavingCollaborationPersistsProjectScope(t *testing.T) {
	joined, _ := json.Marshal(savedConfig{Enabled: false, Name: "lead", Collaboration: "platform-work"})
	left, _ := json.Marshal(savedConfig{Enabled: false, Name: "lead", Collaboration: ""})
	cfg := loadConfig([]json.RawMessage{joined, left})
	if cfg.Collaboration != "" {
		t.Fatalf("restored collaboration = %q, want project scope", cfg.Collaboration)
	}
	if !strings.Contains(string(left), `"collaboration":""`) {
		t.Fatalf("project scope was omitted from durable config: %s", left)
	}
}

func TestChangingCollaborationTurnsOffMessagingAndWakeups(t *testing.T) {
	base := testRoot(t)
	t.Setenv("NOTCH_INTERCOM_DIR", base)
	workspace := filepath.Join(t.TempDir(), "notch")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{configs: []savedConfig{{Enabled: true, Wake: true, Name: "lead"}}}
	s := newService()
	defer s.closeEndpoint()
	if _, err := s.startSession(context.Background(), host, map[string]any{"session_id": "session", "cwd": workspace, "mode": "tui"}); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"collaboration":"platform-work"}`)
	if result, err := s.configureTool(context.Background(), raw, nil); err != nil || result.IsError {
		t.Fatalf("configure = %#v, %v", result, err)
	}
	s.mu.Lock()
	enabled, wake, ready, collaboration := s.enabled, s.wake, s.listener != nil, s.collaboration
	s.mu.Unlock()
	if enabled || wake || ready || collaboration != "platform-work" {
		t.Fatalf("state = enabled=%t wake=%t ready=%t collaboration=%q", enabled, wake, ready, collaboration)
	}
}
