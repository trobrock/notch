package intercom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	wakeAttempts  []json.RawMessage
	entries       []inboundEntry
	outbound      []outboundEntry
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
	case wakeKind:
		data, _ := json.Marshal(value)
		h.wakeAttempts = append(h.wakeAttempts, data)
	case entryKind:
		data, _ := json.Marshal(value)
		var entry inboundEntry
		_ = json.Unmarshal(data, &entry)
		h.entries = append(h.entries, entry)
	case outboundKind:
		data, _ := json.Marshal(value)
		var entry outboundEntry
		_ = json.Unmarshal(data, &entry)
		h.outbound = append(h.outbound, entry)
	case configKind:
		data, _ := json.Marshal(value)
		var cfg savedConfig
		_ = json.Unmarshal(data, &cfg)
		h.configs = append(h.configs, cfg)
	}
	return nil
}

func (h *fakeHost) SessionEntries(_ context.Context, kind string) ([]json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var values []json.RawMessage
	add := func(v any) { data, _ := json.Marshal(v); values = append(values, data) }
	switch kind {
	case wakeKind:
		return append([]json.RawMessage(nil), h.wakeAttempts...), nil
	case entryKind:
		for _, v := range h.entries {
			add(v)
		}
	case outboundKind:
		for _, v := range h.outbound {
			add(v)
		}
	case configKind:
		for _, v := range h.configs {
			add(v)
		}
	}
	return values, nil
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
	s.sessionID, s.name, s.mode, s.wake, s.enabled = sessionID, name, "tui", wake, true
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
	s := startTestService(t, root, "project", "self", "self", false, &fakeHost{})
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

func TestUncorrelatedRepliesDoNotWake(t *testing.T) {
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

func TestAutomatedWakeIsBoundedAndRequestReplyIsTracked(t *testing.T) {
	root := testRoot(t)
	senderHost, receiverHost := &fakeHost{}, &fakeHost{}
	sender := startTestService(t, root, "project", "sender-session", "sender", true, senderHost)
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", true, receiverHost)

	request, err := sender.sendAutomated(context.Background(), "receiver", "please review", true)
	if err != nil || !request.Delivered {
		t.Fatalf("send request = %#v, %v", request, err)
	}
	reply, err := receiver.resolveReply(context.Background(), request.ID, "review complete")
	if err != nil || !reply.Delivered {
		t.Fatalf("send reply = %#v, %v", reply, err)
	}
	status, err := sender.requestStatus(request.ID)
	if err != nil || !strings.Contains(status, `"status": "replied"`) || !strings.Contains(status, "review complete") {
		t.Fatalf("request status = %q, %v", status, err)
	}
	senderHost.mu.Lock()
	if len(senderHost.outbound) != 2 || senderHost.outbound[0].ID != request.ID || !senderHost.outbound[0].DeliveredAt.IsZero() || senderHost.outbound[1].DeliveredAt.IsZero() {
		t.Fatalf("sender outbox events = %#v", senderHost.outbound)
	}
	senderHost.mu.Unlock()
	if _, err := receiver.resolveReply(context.Background(), request.ID, "duplicate reply"); err == nil || !strings.Contains(err.Error(), "already has outbound reply") {
		t.Fatalf("duplicate reply error = %v", err)
	}

	for i := 1; i < maxAutomatedWakes+2; i++ {
		msg := delivery(fmt.Sprintf("automated-%d", i), "")
		msg.AutomatedWake = true
		result, exchangeErr := exchange(context.Background(), receiver.endpoint, msg)
		if exchangeErr != nil || !result.Delivered {
			t.Fatalf("automated wake %d = %#v, %v", i, result, exchangeErr)
		}
	}
	receiverHost.mu.Lock()
	defer receiverHost.mu.Unlock()
	if len(receiverHost.followUps) != maxAutomatedWakes {
		t.Fatalf("automated wakeups = %d, want %d", len(receiverHost.followUps), maxAutomatedWakes)
	}
}

func TestOldWakeCompletionDoesNotChangeNewSessionReservation(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	host := &fakeHost{appendWait: release, appendStarted: started}
	s := newService()
	s.host, s.project, s.sessionID, s.wake, s.generation = host, "project", "receiver-session", true, 1
	msg := delivery("old-generation", "")
	msg.AutomatedWake = true
	done := make(chan response, 1)
	go func() { done <- s.receive(context.Background(), msg) }()
	<-started
	s.mu.Lock()
	s.generation = 2
	s.sessionID = "new-session"
	s.autoWakeReservations = 1
	s.autoWakes = 0
	s.mu.Unlock()
	close(release)
	<-done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.autoWakeReservations != 1 || s.autoWakes != 0 {
		t.Fatalf("new generation wake state = reservations %d, wakes %d", s.autoWakeReservations, s.autoWakes)
	}
}

func TestFailedPersistenceDoesNotConsumeAutomatedWakeBudget(t *testing.T) {
	root := testRoot(t)
	host := &fakeHost{appendErr: errors.New("disk full")}
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", true, host)
	failed := delivery("failed-wake", "")
	failed.AutomatedWake = true
	if result, err := exchange(context.Background(), receiver.endpoint, failed); err != nil || result.Error == "" {
		t.Fatalf("failed wake = %#v, %v", result, err)
	}
	host.mu.Lock()
	host.appendErr = nil
	host.mu.Unlock()
	for i := 0; i < maxAutomatedWakes; i++ {
		msg := delivery(fmt.Sprintf("accepted-%d", i), "")
		msg.AutomatedWake = true
		if result, err := exchange(context.Background(), receiver.endpoint, msg); err != nil || !result.Delivered {
			t.Fatalf("accepted wake %d = %#v, %v", i, result, err)
		}
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.followUps) != maxAutomatedWakes {
		t.Fatalf("wakeups = %d, want %d", len(host.followUps), maxAutomatedWakes)
	}
}

func TestConcurrentSendCapAndSingleReplyAreAtomic(t *testing.T) {
	root := testRoot(t)
	sender := startTestService(t, root, "project", "sender-session", "sender", false, &fakeHost{})
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", false, &fakeHost{})

	receiver.mu.Lock()
	receiver.inbox = append(receiver.inbox, inboundEntry{ID: "needs-one-reply", FromSession: "sender-session"})
	receiver.mu.Unlock()
	var replyWG sync.WaitGroup
	replyResults := make(chan error, 2)
	for range 2 {
		replyWG.Add(1)
		go func() {
			defer replyWG.Done()
			_, err := receiver.resolveReply(context.Background(), "needs-one-reply", "done")
			replyResults <- err
		}()
	}
	replyWG.Wait()
	close(replyResults)
	successes := 0
	for err := range replyResults {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent replies = %d, want 1", successes)
	}

	sender.mu.Lock()
	sender.outbox = make([]outboundEntry, maxOutboundEntries-1)
	sender.mu.Unlock()
	var sendWG sync.WaitGroup
	sendResults := make(chan error, 2)
	for range 2 {
		sendWG.Add(1)
		go func() {
			defer sendWG.Done()
			_, err := sender.send(context.Background(), "receiver", "bounded", "", false)
			sendResults <- err
		}()
	}
	sendWG.Wait()
	close(sendResults)
	successes = 0
	for err := range sendResults {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful capped sends = %d, want 1", successes)
	}
}

func TestOutboundPersistenceFailurePreventsDelivery(t *testing.T) {
	root := testRoot(t)
	receiverHost := &fakeHost{}
	_ = startTestService(t, root, "project", "receiver-session", "receiver", true, receiverHost)
	sender := startTestService(t, root, "project", "sender-session", "sender", true, &fakeHost{appendErr: errors.New("disk full")})
	if _, err := sender.sendAutomated(context.Background(), "receiver", "do work", true); err == nil || !strings.Contains(err.Error(), "before delivery") {
		t.Fatalf("send error = %v", err)
	}
	receiverHost.mu.Lock()
	defer receiverHost.mu.Unlock()
	if len(receiverHost.entries) != 0 || len(receiverHost.followUps) != 0 {
		t.Fatalf("receiver changed after sender persistence failure: entries=%d wakes=%d", len(receiverHost.entries), len(receiverHost.followUps))
	}
}

func TestRequestStatusRequiresReplyFromRequestedPeer(t *testing.T) {
	s := newService()
	if _, err := s.requestStatus("missing"); err == nil || !strings.Contains(err.Error(), "not recorded") {
		t.Fatalf("missing request error = %v", err)
	}
	s.outbox = []outboundEntry{{ID: "request", ToSession: "reviewer", DeliveredAt: time.Now()}}
	s.inbox = []inboundEntry{{ID: "forged", FromSession: "other", ReplyTo: "request", Body: "looks good"}}
	status, err := s.requestStatus("request")
	if err != nil || !strings.Contains(status, `"status": "delivered_pending_reply"`) {
		t.Fatalf("request status = %q, %v", status, err)
	}
}

func TestDurableWakeCountAndOutboxHistory(t *testing.T) {
	inbox := make([]inboundEntry, maxAutomatedWakes+3)
	for i := range inbox {
		inbox[i] = inboundEntry{Wake: true, AutomatedWake: true, WakeAccepted: true}
	}
	if got := countAutomatedWakeRequests(inbox); got != maxAutomatedWakes {
		t.Fatalf("wake count = %d, want %d", got, maxAutomatedWakes)
	}

	raw := make([]json.RawMessage, maxOutboundEntries+1)
	for i := range raw {
		data, _ := json.Marshal(outboundEntry{ID: fmt.Sprintf("request-%d", i)})
		raw[i] = data
	}
	if got := loadOutbox(raw); len(got) != len(raw) || got[0].ID != "request-0" {
		t.Fatalf("loaded outbox len=%d first=%#v", len(got), got[0])
	}
	pending, _ := json.Marshal(outboundEntry{ID: "same"})
	delivered, _ := json.Marshal(outboundEntry{ID: "same", DeliveredAt: time.Now()})
	if got := loadOutbox([]json.RawMessage{pending, delivered}); len(got) != 1 || got[0].DeliveredAt.IsZero() {
		t.Fatalf("collapsed outbox = %#v", got)
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
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
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
	s := startTestService(t, rootA, hashA, "self", "self", false, &fakeHost{})
	peers, err := s.peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatalf("cross-project peers = %#v", peers)
	}
}
