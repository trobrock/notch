package intercom

import (
	"context"
	"testing"
	"time"
)

func TestReceiveFinalizesAcrossEndpointRestart(t *testing.T) {
	for _, reply := range []bool{false, true} {
		name := "request"
		if reply {
			name = "reply"
		}
		t.Run(name, func(t *testing.T) {
			release, started := make(chan struct{}), make(chan struct{})
			host := &fakeHost{appendWait: release, appendStarted: started}
			s := newService()
			s.host, s.project, s.sessionID, s.enabled, s.wake = host, "project", "receiver-session", true, true
			request := delivery("restart-overlap", "")
			if reply {
				request.ReplyTo = "outbound-request"
				s.outbox = []outboundEntry{{ID: request.ReplyTo, ToSession: request.FromSession, Wake: true}}
			}
			done := make(chan response, 1)
			go func() { done <- s.receive(context.Background(), request) }()
			<-started
			// Off/on changes the endpoint generation without switching the session.
			s.closeEndpoint()
			s.mu.Lock()
			s.enabled = false
			s.enabled = true
			s.mu.Unlock()
			close(release)
			result := <-done
			if !result.Delivered {
				t.Fatalf("first delivery: %+v", result)
			}
			s.mu.Lock()
			inflight, reservations := len(s.inflight), len(s.replyWakeReservations)
			inbox := len(s.inbox)
			s.mu.Unlock()
			if inflight != 0 || reservations != 0 || inbox != 1 {
				t.Fatalf("inflight=%d reservations=%d inbox=%d", inflight, reservations, inbox)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			duplicate := s.receive(ctx, request)
			if !duplicate.Delivered || !duplicate.Duplicate || len(host.followUps) != 1 || len(host.entries) != 1 {
				t.Fatalf("retry duplicated or lost work: %+v entries=%d wakes=%d", duplicate, len(host.entries), len(host.followUps))
			}
		})
	}
}
