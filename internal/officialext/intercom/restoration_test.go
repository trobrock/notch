package intercom

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltInIsOffUntilUserEnablesIt(t *testing.T) {
	base := testRoot(t)
	t.Setenv("NOTCH_INTERCOM_DIR", base+"/unused")
	host := &fakeHost{}
	s := newService()
	event := map[string]any{"session_id": "one", "cwd": t.TempDir(), "mode": "tui"}
	if _, err := s.startSession(context.Background(), host, event); err != nil {
		t.Fatal(err)
	}
	defer s.closeEndpoint()
	if s.listener != nil {
		t.Fatal("started an unrequested listener")
	}
	if _, err := os.Stat(base + "/unused"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled service touched runtime directory: %v", err)
	}
	if _, err := s.setName(context.Background(), "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.setWake(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if s.listener != nil {
		t.Fatal("wake setting enabled messaging without permission")
	}
	if _, err := s.setEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if s.listener == nil {
		t.Fatal("no listener after enabling")
	}
	if _, err := s.setEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if s.listener != nil {
		t.Fatal("listener survived disabling")
	}
	if _, err := s.startSession(context.Background(), host, event); err != nil {
		t.Fatal(err)
	}
	if s.listener != nil || s.name != "reviewer" || !s.wake {
		t.Fatalf("resume settings: enabled=%v name=%q wake=%v", s.listener != nil, s.name, s.wake)
	}
	if _, err := s.setEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.startSession(context.Background(), host, event); err != nil {
		t.Fatal(err)
	}
	if s.listener == nil {
		t.Fatal("opt-in not restored")
	}
	// A new durable session has its own permission and no old inbox.
	if _, err := s.startSession(context.Background(), &fakeHost{}, map[string]any{"session_id": "two"}); err != nil {
		t.Fatal(err)
	}
	if s.listener != nil || s.enabled || s.wake {
		t.Fatal("new session inherited opt-in")
	}
	if _, err := s.startSession(context.Background(), &fakeHost{}, map[string]any{}); err != nil {
		t.Fatalf("--no-session startup: %v", err)
	}
	if _, err := s.setEnabled(context.Background(), true); err == nil {
		t.Fatal("enabled without a durable session")
	}
}

func TestCorrelatedReplyWakesRequesterAndCannotLoop(t *testing.T) {
	root := testRoot(t)
	aHost, bHost := &fakeHost{}, &fakeHost{}
	a := startTestService(t, root, "project", "a", "a", true, aHost)
	b := startTestService(t, root, "project", "b", "b", true, bHost)
	req, err := a.sendAutomated(context.Background(), "b", "review this", true)
	if err != nil || !req.WakeQueued {
		t.Fatalf("request = %#v, %v", req, err)
	}
	reply, err := b.resolveReply(context.Background(), req.ID, "done")
	if err != nil || !reply.WakeQueued {
		t.Fatalf("reply = %#v, %v", reply, err)
	}
	aHost.mu.Lock()
	wakes := len(aHost.followUps)
	aHost.mu.Unlock()
	if wakes != 1 {
		t.Fatalf("requester wakes = %d", wakes)
	}
	if _, err := a.resolveReply(context.Background(), reply.ID, "thanks"); err == nil {
		t.Fatal("allowed reply to reply")
	}
	if _, err := b.resolveReply(context.Background(), req.ID, "again"); err == nil {
		t.Fatal("allowed second reply")
	}
	receipt, err := b.retry(context.Background(), reply.ID)
	if err != nil || !receipt.Duplicate {
		t.Fatalf("retry reply = %#v, %v", receipt, err)
	}
	aHost.mu.Lock()
	defer aHost.mu.Unlock()
	if len(aHost.followUps) != 1 || len(aHost.entries) != 1 {
		t.Fatal("reply retry duplicated processing")
	}
}

func TestRetryUncertainDeliveryAfterBothSessionsResume(t *testing.T) {
	root, cwd := testRoot(t), t.TempDir()
	t.Setenv("NOTCH_INTERCOM_DIR", root)
	aHost, bHost := &fakeHost{}, &fakeHost{}
	// Use real namespace resolution for resume, but a short runtime path for sockets.
	namespace, project, err := runtimeRoot(cwd)
	if err != nil {
		t.Fatal(err)
	}
	a := startTestService(t, namespace, project, "a", "a", true, aHost)
	b := startTestService(t, namespace, project, "b", "b", true, bHost)
	aHost.configs = []savedConfig{{Enabled: true, Wake: true, Name: "a"}}
	bHost.configs = []savedConfig{{Enabled: true, Wake: true, Name: "b"}}
	a.exchange = func(ctx context.Context, endpoint string, request message) (response, error) {
		if _, err := exchange(ctx, endpoint, request); err != nil {
			return response{}, err
		}
		return response{}, errors.New("simulated lost acknowledgment")
	}
	if _, err := a.sendAutomated(context.Background(), "b", "work", true); err == nil {
		t.Fatal("expected uncertain delivery")
	}
	id := a.outbox[0].ID
	if !a.outbox[0].DeliveredAt.IsZero() {
		t.Fatal("uncertain delivery marked delivered")
	}
	a.closeEndpoint()
	b.closeEndpoint()
	a, b = newService(), newService()
	defer a.closeEndpoint()
	defer b.closeEndpoint()
	if _, err := a.startSession(context.Background(), aHost, map[string]any{"session_id": "a", "cwd": cwd, "mode": "tui"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.startSession(context.Background(), bHost, map[string]any{"session_id": "b", "cwd": cwd, "mode": "tui"}); err != nil {
		t.Fatal(err)
	}
	receipt, err := a.retry(context.Background(), id)
	if err != nil || !receipt.Delivered || !receipt.Duplicate {
		t.Fatalf("resumed retry = %#v, %v", receipt, err)
	}
	if len(bHost.entries) != 1 || len(bHost.followUps) != 1 {
		t.Fatalf("replayed delivery/wake: %d/%d", len(bHost.entries), len(bHost.followUps))
	}
	if len(a.outbox) != 1 || a.outbox[0].DeliveredAt.IsZero() {
		t.Fatal("retry did not update durable outbox")
	}
}

type immediateHost struct {
	*fakeHost
	onWake func()
}

func (h immediateHost) FollowUpFor(ctx context.Context, id, text string) error {
	h.onWake()
	return h.fakeHost.FollowUpFor(ctx, id, text)
}
func TestInboxPublishedBeforeIdleAgentWake(t *testing.T) {
	s := newService()
	h := immediateHost{fakeHost: &fakeHost{}, onWake: func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.inbox) != 1 {
			t.Error("woken agent cannot find request in inbox")
		}
	}}
	s.host, s.project, s.sessionID, s.wake = h, "project", "receiver-session", true
	if result := s.receive(context.Background(), delivery("race", "")); !result.Delivered || !result.WakeQueued {
		t.Fatalf("receipt = %#v", result)
	}
}

func TestFullSizeEscapedMessageFitsWireBudget(t *testing.T) {
	root := testRoot(t)
	b := startTestService(t, root, "project", "b", "b", false, &fakeHost{})
	a := startTestService(t, root, "project", "a", "a", false, &fakeHost{})
	body := "x" + strings.Repeat("\x00", maxBodyBytes-2) + "x"
	result, err := a.sendAutomated(context.Background(), "b", body, false)
	if err != nil || !result.Delivered {
		t.Fatalf("escaped message = %#v, %v", result, err)
	}
	if len(b.inbox) != 1 || b.inbox[0].Body != body {
		t.Fatal("escaped message not preserved")
	}
}

func TestProcessRecoveryConsumesDurableWakeBudget(t *testing.T) {
	root, cwd := testRoot(t), t.TempDir()
	t.Setenv("NOTCH_INTERCOM_DIR", root)
	host := &fakeHost{configs: []savedConfig{{Enabled: true, Wake: true}}}
	s := newService()
	event := map[string]any{"session_id": "receiver-session", "cwd": cwd, "mode": "tui"}
	if _, err := s.startSession(context.Background(), host, event); err != nil {
		t.Fatal(err)
	}
	defer s.closeEndpoint()
	req := delivery("passive", "")
	req.Project = s.project
	req.Wake = false
	if receipt := s.receive(context.Background(), req); !receipt.Delivered {
		t.Fatal(receipt)
	}
	if _, err := s.process(context.Background(), req.ID); err != nil {
		t.Fatal(err)
	}
	if len(host.followUps) != 1 {
		t.Fatal("stored message was not queued")
	}
	if _, err := s.startSession(context.Background(), host, event); err != nil {
		t.Fatal(err)
	}
	if s.autoWakes != 1 {
		t.Fatalf("recovered wake budget = %d", s.autoWakes)
	}
	s.autoWakes = maxAutomatedWakes
	if _, err := s.process(context.Background(), req.ID); err == nil {
		t.Fatal("process bypassed wake limit")
	}
}

func TestOneReplyWakePerRequest(t *testing.T) {
	root := testRoot(t)
	h := &fakeHost{}
	a := startTestService(t, root, "project", "a", "a", true, h)
	_ = startTestService(t, root, "project", "b", "b", true, &fakeHost{})
	sent, err := a.sendAutomated(context.Background(), "b", "work", true)
	if err != nil {
		t.Fatal(err)
	}
	request := message{Version: protocolVersion, Type: "deliver", ID: "reply1", Project: "project", ToSession: "a", FromSession: "b", ReplyTo: sent.ID, Wake: true, Body: "done"}
	if result := a.receive(context.Background(), request); !result.WakeQueued {
		t.Fatal(result)
	}
	request.ID = "reply2"
	if result := a.receive(context.Background(), request); result.WakeQueued {
		t.Fatal("second reply woke requester")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.followUps) != 1 {
		t.Fatalf("wakeups = %d", len(h.followUps))
	}
	// Request status is JSON, not a claim that delivery means work completed.
	text, err := a.requestStatus(sent.ID)
	if err != nil || !json.Valid([]byte(text)) {
		t.Fatalf("status = %q, %v", text, err)
	}
}

func TestLinkedWorktreesShareLocalNamespace(t *testing.T) {
	t.Setenv("NOTCH_INTERCOM_DIR", testRoot(t))
	repo, linked := t.TempDir(), t.TempDir()
	gitDir := filepath.Join(repo, ".git")
	worktreeGit := filepath.Join(gitDir, "worktrees", "linked")
	if err := os.MkdirAll(worktreeGit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+worktreeGit+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeGit, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootA, projectA, err := runtimeRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	rootB, projectB, err := runtimeRoot(linked)
	if err != nil {
		t.Fatal(err)
	}
	if rootA != rootB || projectA != projectB {
		t.Fatalf("worktree namespace differs: %s/%s vs %s/%s", rootA, projectA, rootB, projectB)
	}
}

func TestNamePublicationFailureKeepsDurableNameAuthoritative(t *testing.T) {
	host := &fakeHost{}
	s := startTestService(t, testRoot(t), "project", "one", "before", false, host)
	// Force an atomic publication failure without changing session persistence.
	s.mu.Lock()
	s.record = filepath.Join(s.root, "missing", "record.json")
	s.mu.Unlock()
	if _, err := s.setName(context.Background(), "after"); err == nil || !strings.Contains(err.Error(), "name saved") {
		t.Fatalf("rename error = %v", err)
	}
	s.mu.Lock()
	name := s.name
	s.mu.Unlock()
	if name != "after" {
		t.Fatalf("memory name = %q", name)
	}
	if _, err := s.setWake(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if last := host.configs[len(host.configs)-1]; last.Name != "after" {
		t.Fatalf("wake setting reverted durable name: %#v", last)
	}
}

func TestIntercomRejectsSharedAndSymlinkRuntimeDirectories(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(shared); err == nil {
		t.Fatal("accepted shared runtime directory")
	}
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(link); err == nil {
		t.Fatal("accepted symlink runtime directory")
	}
	if err := ensurePrivateDirectory(target); err != nil {
		t.Fatal(err)
	}
}

func TestIntercomRejectsWritableNonStickyAncestor(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(filepath.Join(parent, "private")); err == nil || !strings.Contains(err.Error(), "unsafe ancestor") {
		t.Fatalf("directory error = %v", err)
	}
}

func TestRuntimeDirectoryChecksIntermediateSymlinkTargets(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	private := filepath.Join(base, "private")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	middle := filepath.Join(shared, "middle")
	if err := os.Symlink(private, middle); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(base, "first")
	if err := os.Symlink(middle, first); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(filepath.Join(first, "runtime")); err == nil || !strings.Contains(err.Error(), "unsafe ancestor") {
		t.Fatalf("intermediate target error=%v", err)
	}
}

func TestRuntimeDirectoryRejectsSymlinkParentTraversal(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	for _, dir := range []string{filepath.Join(base, "private"), filepath.Join(shared, "child"), filepath.Join(shared, "private")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared/child", filepath.Join(base, "hop")); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(base, "first")
	if err := os.Symlink("hop/../private", first); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(filepath.Join(first, "runtime")); err == nil || !strings.Contains(err.Error(), "unsupported parent traversal") {
		t.Fatalf("symlink parent traversal error=%v", err)
	}
}
