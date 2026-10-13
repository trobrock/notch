package main

import (
	"context"
	"strings"
	"testing"
)

func TestConcurrentOpenAllowsOneSessionOwner(t *testing.T) {
	root := testRoot(t)
	services := []*service{newService(), newService()}
	for _, s := range services {
		s.root, s.project, s.sessionID, s.name, s.mode = root, "project", "shared-session", "shared", "tui"
		s.host = &fakeHost{}
	}
	start := make(chan struct{})
	results := make(chan error, len(services))
	for _, s := range services {
		go func(s *service) {
			<-start
			results <- s.openEndpoint()
		}(s)
	}
	close(start)
	successes := 0
	for range services {
		if err := <-results; err == nil {
			successes++
		}
	}
	for _, s := range services {
		s.closeEndpoint()
	}
	if successes != 1 {
		t.Fatalf("successful owners = %d, want 1", successes)
	}
}

func TestReplyRejectsAmbiguousRequestID(t *testing.T) {
	s := newService()
	s.inbox = []inboundEntry{{ID: "collision", FromSession: "one"}, {ID: "collision", FromSession: "two"}}
	if _, err := s.resolveReply(context.Background(), "collision", "answer"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("resolveReply error = %v", err)
	}
}

func TestConcurrentSettingsPersistFinalCombinedState(t *testing.T) {
	root := testRoot(t)
	host := &fakeHost{}
	s := startTestService(t, root, "project", "settings-session", "before", false, host)
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, err := s.setName(context.Background(), "after")
		errs <- err
	}()
	go func() {
		<-start
		_, err := s.setWake(context.Background(), true)
		errs <- err
	}()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	name, wake := s.name, s.wake
	s.mu.Unlock()
	host.mu.Lock()
	last := host.configs[len(host.configs)-1]
	host.mu.Unlock()
	if last.Name != name || last.Wake != wake || name != "after" || !wake {
		t.Fatalf("memory=(%q,%v), durable=(%q,%v)", name, wake, last.Name, last.Wake)
	}
}
