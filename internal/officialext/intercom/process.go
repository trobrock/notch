package intercom

import (
	"context"
	"errors"
	"time"
)

// process is an explicit user recovery action, not an automatic replay. A
// durable delivery receipt cannot prove an agent finished work before a crash.
func (s *service) process(ctx context.Context, id string) (string, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.mu.Lock()
	if !s.wake || s.listener == nil {
		s.mu.Unlock()
		return "", errors.New("enable intercom and wakeups before processing a stored message")
	}
	var entry inboundEntry
	matches := 0
	for _, item := range s.inbox {
		if item.ID == id {
			entry = item
			matches++
		}
	}
	if matches != 1 {
		s.mu.Unlock()
		return "", errors.New("inbox request ID is missing or ambiguous")
	}
	host, sessionID := s.host, s.sessionID
	s.mu.Unlock()
	err := host.AppendSessionEntryFor(ctx, sessionID, wakeKind, map[string]any{"request_id": id, "at": time.Now().UTC()})
	if err != nil {
		return "", err
	}
	if err := host.FollowUpFor(ctx, sessionID, wakeMessage(entry)); err != nil {
		return "", err
	}
	return "Intercom message queued for processing.", nil
}
