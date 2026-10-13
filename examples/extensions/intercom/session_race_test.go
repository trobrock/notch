package main

import (
	"context"
	"strings"
	"testing"
)

func TestWakeDoesNotCrossSessionChangeAfterPersistence(t *testing.T) {
	host := &fakeHost{activeSession: "new-session"}
	s := newService()
	s.host, s.project, s.sessionID, s.wake = host, "project", "receiver-session", true
	result := s.receive(context.Background(), delivery("session-race", ""))
	if !result.Delivered || !strings.Contains(result.Warning, "session changed") {
		t.Fatalf("delivery result = %#v", result)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.entries) != 1 || len(host.followUps) != 0 {
		t.Fatalf("entries=%d follow-ups=%d", len(host.entries), len(host.followUps))
	}
}
