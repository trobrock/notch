package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHost struct {
	mu            sync.Mutex
	entries       []inboundEntry
	configs       []savedConfig
	followUps     []string
	notices       []string
	activeSession string
	appendWait    <-chan struct{}
	appendStarted chan struct{}
	appendErr     error
}

func (h *fakeHost) AppendSessionEntryFor(ctx context.Context, _ string, kind string, value any) error {
	if h.appendStarted != nil {
		close(h.appendStarted)
	}
	if h.appendWait != nil {
		select {
		case <-h.appendWait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if h.appendErr != nil {
		return h.appendErr
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch kind {
	case entryKind:
		data, _ := json.Marshal(value)
		var entry inboundEntry
		_ = json.Unmarshal(data, &entry)
		h.entries = append(h.entries, entry)
	case configKind:
		data, _ := json.Marshal(value)
		var cfg savedConfig
		_ = json.Unmarshal(data, &cfg)
		h.configs = append(h.configs, cfg)
	}
	return nil
}

func (h *fakeHost) SessionEntries(context.Context, string) ([]json.RawMessage, error) {
	return nil, nil
}
func (h *fakeHost) FollowUp(_ context.Context, value string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.followUps = append(h.followUps, value)
	return nil
}
func (h *fakeHost) FollowUpFor(ctx context.Context, sessionID string, value string) error {
	h.mu.Lock()
	active := h.activeSession
	h.mu.Unlock()
	if active != "" && active != sessionID {
		return errors.New("active session changed before extension follow-up was delivered")
	}
	return h.FollowUp(ctx, value)
}
func (h *fakeHost) Notify(_ context.Context, value, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notices = append(h.notices, value)
	return nil
}
func (*fakeHost) SetStatus(context.Context, string, string) error { return nil }

func startTestService(t *testing.T, root, project, sessionID, name string, wake bool, host *fakeHost) *service {
	t.Helper()
	s := newService()
	s.host, s.root, s.project = host, root, project
	s.sessionID, s.name, s.mode, s.wake = sessionID, name, "tui", wake
	if err := s.openEndpoint(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeEndpoint)
	return s
}

func testRoot(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("", "notch-ic-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	root := filepath.Join(base, "p")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func delivery(id, replyTo string) message {
	return message{Version: protocolVersion, Type: "deliver", ID: id, Project: "project", ToSession: "receiver-session", FromSession: "sender-session", FromName: "sender", ReplyTo: replyTo, Wake: true, Body: "finding", SentAt: time.Now()}
}

func TestDeliveryRetryIsDurableAndDeduplicated(t *testing.T) {
	root, host := testRoot(t), &fakeHost{}
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", false, host)

	first, err := exchange(context.Background(), receiver.endpoint, delivery("request-1", ""))
	if err != nil || !first.Delivered || first.Duplicate {
		t.Fatalf("first delivery = %#v, %v", first, err)
	}
	second, err := exchange(context.Background(), receiver.endpoint, delivery("request-1", ""))
	if err != nil || !second.Delivered || !second.Duplicate {
		t.Fatalf("retry = %#v, %v", second, err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.entries) != 1 {
		t.Fatalf("durable entries = %d, want 1", len(host.entries))
	}
}

func TestConcurrentDeliveryRaceAppendsOnce(t *testing.T) {
	root, host := testRoot(t), &fakeHost{}
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", false, host)
	const attempts = 12
	var wg sync.WaitGroup
	results := make(chan response, attempts)
	errs := make(chan error, attempts)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := exchange(context.Background(), receiver.endpoint, delivery("same-request", ""))
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if !result.Delivered {
			t.Fatalf("delivery was not acknowledged: %#v", result)
		}
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.entries) != 1 {
		t.Fatalf("durable entries = %d, want 1", len(host.entries))
	}
}

func TestStaleSessionsAreNotListed(t *testing.T) {
	root := testRoot(t)
	staleSocket := filepath.Join(root, "stale.sock")
	staleRecord := filepath.Join(root, "stale.json")
	if err := writeRecord(staleRecord, peerRecord{Version: protocolVersion, Project: "project", SessionID: "stale", Name: "stale", Endpoint: staleSocket}); err != nil {
		t.Fatal(err)
	}
	s := &service{root: root, project: "project", sessionID: "self", seen: map[string]cachedDelivery{}, inflight: map[string]chan struct{}{}}
	peers, err := s.peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatalf("peers = %#v", peers)
	}
}

func TestMalformedAndOversizedMessagesAreRejected(t *testing.T) {
	root, host := testRoot(t), &fakeHost{}
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", false, host)

	conn, err := net.Dial("unix", receiver.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("not-json\n"))
	var malformed response
	if err := json.NewDecoder(conn).Decode(&malformed); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if !strings.Contains(malformed.Error, "malformed") {
		t.Fatalf("malformed response = %#v", malformed)
	}

	result, err := exchange(context.Background(), receiver.endpoint, message{Version: protocolVersion, Type: "deliver", ID: "large", Project: "project", ToSession: "receiver-session", FromSession: "sender", Body: strings.Repeat("x", maxBodyBytes+1)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Error, "limit") {
		t.Fatalf("oversized response = %#v", result)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.entries) != 0 {
		t.Fatalf("invalid messages were persisted: %d", len(host.entries))
	}
}

func TestCanceledDeliveryReturnsPromptly(t *testing.T) {
	root := testRoot(t)
	listener, err := net.Listen("unix", filepath.Join(root, "blocked.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			close(accepted)
			defer conn.Close()
			_, _ = ioReadOneLine(conn)
			select {}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, callErr := exchange(ctx, listener.Addr().String(), delivery("cancel", ""))
		done <- callErr
	}()
	<-accepted
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled exchange returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled exchange did not return promptly")
	}
}

func ioReadOneLine(conn net.Conn) ([]byte, error) { return readBoundedLine(conn) }

func TestRepliesNeverWakeAndRequestsWakeOnlyWhenOptedIn(t *testing.T) {
	root, host := testRoot(t), &fakeHost{}
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", true, host)
	if result, err := exchange(context.Background(), receiver.endpoint, delivery("request", "")); err != nil || !result.Delivered {
		t.Fatalf("request = %#v, %v", result, err)
	}
	if result, err := exchange(context.Background(), receiver.endpoint, delivery("reply", "request")); err != nil || !result.Delivered {
		t.Fatalf("reply = %#v, %v", result, err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.followUps) != 1 {
		t.Fatalf("wakeups = %d, want 1", len(host.followUps))
	}
	if !strings.Contains(host.followUps[0], "untrusted data") || !strings.Contains(host.followUps[0], "Request ID: request") {
		t.Fatalf("unsafe wake text: %q", host.followUps[0])
	}
}

func TestDisconnectAndPersistenceErrorsAreClear(t *testing.T) {
	root := testRoot(t)
	host := &fakeHost{appendErr: errors.New("disk full")}
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", false, host)
	result, err := exchange(context.Background(), receiver.endpoint, delivery("persist", ""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Error, "disk full") {
		t.Fatalf("persistence response = %#v", result)
	}
	receiver.closeEndpoint()
	if _, err := exchange(context.Background(), receiver.endpoint, delivery("gone", "")); err == nil {
		t.Fatal("delivery to disconnected session succeeded")
	}
}

func TestProjectNamespacesDoNotSeeEachOther(t *testing.T) {
	base := t.TempDir()
	t.Setenv("NOTCH_INTERCOM_DIR", base)
	projectA := t.TempDir()
	projectB := t.TempDir()
	rootA, hashA, err := runtimeRoot(projectA)
	if err != nil {
		t.Fatal(err)
	}
	rootB, hashB, err := runtimeRoot(projectB)
	if err != nil {
		t.Fatal(err)
	}
	if rootA == rootB || hashA == hashB {
		t.Fatal("different projects shared an intercom namespace")
	}
	peer := startTestService(t, rootB, hashB, "other", "other", false, &fakeHost{})
	_ = peer
	s := &service{root: rootA, project: hashA, sessionID: "self", seen: map[string]cachedDelivery{}, inflight: map[string]chan struct{}{}}
	peers, err := s.peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatalf("cross-project peers = %#v", peers)
	}
}
