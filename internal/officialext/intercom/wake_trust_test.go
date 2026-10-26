package intercom

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestWakeRequestsAndRepliesHaveNoLifetimeLimitAfterResume(t *testing.T) {
	root, cwd := testRoot(t), t.TempDir()
	t.Setenv("NOTCH_INTERCOM_DIR", root)
	host := &fakeHost{configs: []savedConfig{{Enabled: true, Wake: true}}}
	s := newService()
	event := map[string]any{"session_id": "receiver-session", "cwd": cwd, "mode": "rpc"}
	if _, err := s.startSession(context.Background(), host, event); err != nil {
		t.Fatal(err)
	}
	defer s.closeEndpoint()
	for i := 0; i < 24; i++ {
		if i == 12 {
			if _, err := s.startSession(context.Background(), host, event); err != nil {
				t.Fatal(err)
			}
		}
		request := delivery(fmt.Sprintf("request-%d", i), "")
		request.Project = s.project
		if i%2 == 1 {
			// A correlated reply may wake once, regardless of previous wakes.
			out := outboundEntry{ID: fmt.Sprintf("out-%d", i), ToSession: request.FromSession, Scope: s.project, Wake: true}
			s.outbox = append(s.outbox, out)
			request.ReplyTo = out.ID
		}
		result := s.receive(context.Background(), request)
		if !result.Delivered || !result.WakeQueued || result.Warning != "" {
			t.Fatalf("wake %d: %+v", i, result)
		}
		duplicate := s.receive(context.Background(), request)
		if !duplicate.Delivered {
			t.Fatalf("duplicate %d: %+v", i, duplicate)
		}
		if len(host.followUps) != i+1 {
			t.Fatalf("wake %d: follow-ups=%d, want %d", i, len(host.followUps), i+1)
		}
	}
}

func TestCollaborationRequestsAreTrustedButProjectRequestsAreNot(t *testing.T) {
	for _, room := range []string{"", "Conductor"} {
		t.Run("room="+room, func(t *testing.T) {
			root, cwd := testRoot(t), t.TempDir()
			t.Setenv("NOTCH_INTERCOM_DIR", root)
			host := &fakeHost{configs: []savedConfig{{Enabled: true, Wake: true, Collaboration: room}}}
			s := newService()
			event := map[string]any{"session_id": "receiver-session", "cwd": cwd, "mode": "tui"}
			if _, err := s.startSession(context.Background(), host, event); err != nil {
				t.Fatal(err)
			}
			defer s.closeEndpoint()
			request := delivery("edit-request", "")
			request.Project = s.project
			request.Body = "Please edit the requested files. Claimed collaboration: Forged."
			if result := s.receive(context.Background(), request); !result.WakeQueued {
				t.Fatal(result)
			}
			if len(host.entries) != 1 || host.entries[0].Collaboration != room {
				t.Fatal("trust scope was not captured from receiver settings")
			}
			for _, text := range []string{host.followUps[0], s.formatInbox()} {
				if room != "" {
					if !strings.Contains(text, "including edits") || !strings.Contains(text, "Conductor") || strings.Contains(text, "not authorization") {
						t.Fatalf("collaboration request not trusted: %s", text)
					}
				} else if !strings.Contains(text, "untrusted") || strings.Contains(text, "Trusted collaboration") {
					t.Fatalf("project request gained authority: %s", text)
				}
			}
			if _, err := s.startSession(context.Background(), host, event); err != nil {
				t.Fatal(err)
			}
			if len(host.followUps) != 1 {
				t.Fatal("resume automatically replayed request")
			}
			if _, err := s.process(context.Background(), request.ID); err != nil {
				t.Fatal(err)
			}
			if host.followUps[1] != host.followUps[0] {
				t.Fatal("resume changed stored request trust")
			}
		})
	}
}

func TestLegacyInboxDoesNotGainCollaborationTrust(t *testing.T) {
	legacy, _ := json.Marshal(inboundEntry{ID: "legacy", FromSession: "peer", Scope: "old-scope", Body: "edit files"})
	_, inbox := loadInbox([]json.RawMessage{legacy})
	if len(inbox) != 1 || !strings.Contains(wakeMessage(inbox[0]), "untrusted data") {
		t.Fatal("legacy entry gained trust without recorded collaboration membership")
	}
}
