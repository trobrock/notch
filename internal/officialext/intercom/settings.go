package intercom

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func (s *service) setEnabled(ctx context.Context, enabled bool) (string, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.mu.Lock()
	host, id, name, wake, mode, cwd, root, collaboration := s.host, s.sessionID, s.name, s.wake, s.mode, s.cwd, s.root, s.collaboration
	ready := s.listener != nil
	wasEnabled := s.enabled
	s.mu.Unlock()
	if host == nil || id == "" {
		return "", errors.New("intercom requires a durable session")
	}
	if enabled && mode != "tui" && mode != "rpc" {
		return "", errors.New("intercom requires fullscreen TUI or RPC mode")
	}
	if enabled && !ready {
		if root == "" {
			root, project, workspaceName, err := scopeRoot(cwd, collaboration)
			if err != nil {
				return "", err
			}
			s.mu.Lock()
			s.root, s.project, s.workspace = root, project, workspaceName
			s.mu.Unlock()
		}
		if err := s.openEndpoint(); err != nil {
			return "", err
		}
	}
	if !enabled {
		// Stop accepting deliveries before recording the opt-out. Restore the
		// prior gate if persistence fails.
		s.mu.Lock()
		s.enabled = false
		s.mu.Unlock()
	}
	if err := host.AppendSessionEntryFor(ctx, id, configKind, savedConfig{Enabled: enabled, Name: name, Wake: wake, Collaboration: collaboration}); err != nil {
		if enabled && !ready {
			s.closeEndpoint()
		}
		if !enabled {
			s.mu.Lock()
			s.enabled = wasEnabled
			s.mu.Unlock()
		}
		return "", err
	}
	if !enabled {
		s.closeEndpoint()
	}
	s.mu.Lock()
	s.enabled = enabled
	s.mu.Unlock()
	status := ""
	if enabled {
		status = fmt.Sprintf("%s · wake %s", name, onOff(wake))
	}
	_ = host.SetStatus(ctx, "intercom", status)
	return "Intercom is " + onOff(enabled) + ".", nil
}
func (s *service) setWake(ctx context.Context, enabled bool) (string, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.mu.Lock()
	host, sessionID, name, generation, currentEnabled, collaboration := s.host, s.sessionID, s.name, s.generation, s.enabled, s.collaboration
	s.mu.Unlock()
	if host == nil {
		return "", errors.New("intercom session is not ready")
	}
	if err := host.AppendSessionEntryFor(ctx, sessionID, configKind, savedConfig{Enabled: currentEnabled, Wake: enabled, Name: name, Collaboration: collaboration}); err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.generation != generation || s.sessionID != sessionID {
		s.mu.Unlock()
		return "", errors.New("intercom session changed while saving wake setting")
	}
	s.wake = enabled
	s.mu.Unlock()
	if s.enabled {
		_ = host.SetStatus(ctx, "intercom", fmt.Sprintf("%s · wake %s", name, onOff(enabled)))
	}
	return "Intercom wakeups are " + onOff(enabled) + ".", nil
}

func (s *service) setName(ctx context.Context, name string) (string, error) {
	if !validName(name) {
		return "", errors.New("name must be 1-32 letters, numbers, dots, dashes, or underscores")
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.mu.Lock()
	host, sessionID, wake, generation, currentEnabled, collaboration := s.host, s.sessionID, s.wake, s.generation, s.enabled, s.collaboration
	s.mu.Unlock()
	if host == nil {
		return "", errors.New("intercom session is not ready")
	}
	if err := host.AppendSessionEntryFor(ctx, sessionID, configKind, savedConfig{Enabled: currentEnabled, Wake: wake, Name: name, Collaboration: collaboration}); err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.generation != generation || s.sessionID != sessionID {
		s.mu.Unlock()
		return "", errors.New("intercom session changed while saving name")
	}
	record := peerRecord{Version: protocolVersion, Project: s.project, Workspace: s.workspace, SessionID: sessionID, Name: name, Mode: s.mode, Endpoint: s.endpoint, Token: s.token, PID: os.Getpid(), StartedAt: time.Now().UTC()}
	// The append is authoritative even when discovery publication fails.
	s.name = name
	var publishErr error
	if s.record != "" {
		publishErr = writeRecord(s.record, record)
	}
	s.mu.Unlock()
	if s.enabled {
		_ = host.SetStatus(ctx, "intercom", fmt.Sprintf("%s · wake %s", name, onOff(wake)))
	}
	if publishErr != nil {
		return "", fmt.Errorf("name saved, but discovery publication failed (toggle intercom off/on to retry): %w", publishErr)
	}

	return "Intercom name is now " + name + ".", nil
}

func (s *service) setCollaboration(ctx context.Context, collaboration string) (string, error) {
	if collaboration != "" && !validName(collaboration) {
		return "", errors.New("collaboration must be 1-32 letters, numbers, dots, dashes, or underscores")
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.mu.Lock()
	host, sessionID, cwd, name, enabled, generation := s.host, s.sessionID, s.cwd, s.name, s.enabled, s.generation
	s.mu.Unlock()
	if host == nil || sessionID == "" {
		return "", errors.New("intercom requires a durable session")
	}
	if enabled {
		return "", errors.New("turn intercom off before changing collaboration")
	}
	root, project, workspaceName, err := scopeRoot(cwd, collaboration)
	if err != nil {
		return "", err
	}
	cfg := savedConfig{Enabled: false, Wake: false, Name: name, Collaboration: collaboration}
	if err := host.AppendSessionEntryFor(ctx, sessionID, configKind, cfg); err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.generation != generation || s.sessionID != sessionID {
		s.mu.Unlock()
		return "", errors.New("intercom session changed while saving collaboration")
	}
	s.root, s.project, s.workspace = root, project, workspaceName
	s.collaboration, s.wake = collaboration, false
	s.mu.Unlock()
	_ = host.SetStatus(ctx, "intercom", "")
	if collaboration == "" {
		return "Intercom now uses this project only. Messaging and wakeups are off.", nil
	}
	return "Joined intercom collaboration " + collaboration + ". Members are trusted to request work, including edits. Messaging and wakeups are off.", nil
}

func (s *service) status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := "project"
	if s.collaboration != "" {
		scope = "collaboration " + s.collaboration + " (trusted member requests)"
	}
	return fmt.Sprintf("Intercom %s as %s (%s), wake %s, scope %s.", readyWord(s.listener != nil), s.name, s.sessionID, onOff(s.wake), scope)
}

func validName(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r) {
			continue
		}
		return false
	}
	return true
}
