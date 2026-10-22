package intercom

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSendRetriesSameRequestIDAfterLostAcknowledgement(t *testing.T) {
	root := testRoot(t)
	receiverHost := &fakeHost{}
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", false, receiverHost)
	sender := startTestService(t, root, "project", "sender-session", "sender", false, &fakeHost{})
	realExchange := exchange
	calls := 0
	var firstID string
	sender.exchange = func(ctx context.Context, endpoint string, request message) (response, error) {
		calls++
		if firstID == "" {
			firstID = request.ID
		} else if request.ID != firstID {
			t.Fatalf("retry ID = %q, want %q", request.ID, firstID)
		}
		result, err := realExchange(ctx, endpoint, request)
		if calls == 1 && err == nil && result.Delivered {
			return response{}, errors.New("acknowledgement lost")
		}
		return result, err
	}
	result, err := sender.send(context.Background(), "receiver", "one finding", "", true)
	if err != nil || !result.Delivered || !result.Duplicate {
		t.Fatalf("send = %#v, %v", result, err)
	}
	if calls != 2 {
		t.Fatalf("exchange calls = %d, want 2", calls)
	}
	receiverHost.mu.Lock()
	defer receiverHost.mu.Unlock()
	if len(receiverHost.entries) != 1 {
		t.Fatalf("durable entries = %d, want 1", len(receiverHost.entries))
	}
	_ = receiver
}

func TestDurableDedupeReloadAndCollisionChecks(t *testing.T) {
	entry := inboundEntry{ID: "same", FromSession: "sender-a", Wake: true, AutomatedWake: true, Body: "original"}
	raw, _ := json.Marshal(entry)
	seen, inbox := loadInbox([]json.RawMessage{raw})
	s := newService()
	s.project, s.sessionID, s.host = "project", "receiver-session", &fakeHost{}
	s.seen, s.inbox = seen, inbox

	duplicate := message{Version: protocolVersion, Type: "deliver", ID: "same", Project: "project", ToSession: "receiver-session", FromSession: "sender-a", Wake: true, AutomatedWake: true, Body: "original"}
	if result := s.receive(context.Background(), duplicate); !result.Delivered || !result.Duplicate {
		t.Fatalf("reloaded duplicate = %#v", result)
	}
	collision := duplicate
	collision.Body = "changed"
	if result := s.receive(context.Background(), collision); !strings.Contains(result.Error, "reused") {
		t.Fatalf("changed collision = %#v", result)
	}
	otherSender := duplicate
	otherSender.FromSession = "sender-b"
	if result := s.receive(context.Background(), otherSender); !result.Delivered {
		t.Fatalf("other sender = %#v", result)
	}
}

func TestForgedCrossProjectRecordCannotDeliver(t *testing.T) {
	root := testRoot(t)
	host := &fakeHost{}
	receiver := startTestService(t, root, "project-b", "receiver-session", "receiver", true, host)
	forged := delivery("forged", "")
	forged.Project = "project-a"
	result, err := exchange(context.Background(), receiver.endpoint, forged)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Error, "identity mismatch") {
		t.Fatalf("forged delivery = %#v", result)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.entries) != 0 || len(host.followUps) != 0 {
		t.Fatalf("forged delivery changed receiver: entries=%d wakeups=%d", len(host.entries), len(host.followUps))
	}
}

func TestShutdownCancelsBlockedInboundPersistence(t *testing.T) {
	root := testRoot(t)
	blocked := make(chan struct{})
	started := make(chan struct{})
	host := &fakeHost{appendWait: blocked, appendStarted: started}
	receiver := startTestService(t, root, "project", "receiver-session", "receiver", false, host)
	done := make(chan error, 1)
	go func() {
		_, err := exchange(context.Background(), receiver.endpoint, delivery("blocked", ""))
		done <- err
	}()
	<-started
	closed := make(chan struct{})
	go func() {
		receiver.closeEndpoint()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked behind inbound persistence")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("inbound delivery did not stop after shutdown")
	}
}

func TestToolSendIsPassiveToPreventAutomaticWakeLoops(t *testing.T) {
	root := testRoot(t)
	receiverHost := &fakeHost{}
	_ = startTestService(t, root, "project", "receiver-session", "receiver", true, receiverHost)
	sender := startTestService(t, root, "project", "sender-session", "sender", false, &fakeHost{})
	raw := json.RawMessage(`{"target":"receiver","message":"agent finding"}`)
	result, err := sender.sendTool(context.Background(), raw, nil)
	if err != nil || result.IsError {
		t.Fatalf("tool send = %#v, %v", result, err)
	}
	receiverHost.mu.Lock()
	defer receiverHost.mu.Unlock()
	if len(receiverHost.entries) != 1 || len(receiverHost.followUps) != 0 {
		t.Fatalf("tool delivery entries=%d wakeups=%d", len(receiverHost.entries), len(receiverHost.followUps))
	}
}
