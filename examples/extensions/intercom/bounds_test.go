package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestInboundHistoryBudgetRejectsWithoutPersistence(t *testing.T) {
	host := &fakeHost{}
	s := newService()
	s.host, s.project, s.sessionID = host, "project", "receiver-session"
	s.inbox = make([]inboundEntry, maxInboundEntries)
	for i := range s.inbox {
		s.inbox[i] = inboundEntry{ID: fmt.Sprintf("old-%d", i), FromSession: "sender"}
	}
	request := delivery("over-budget", "")
	result := s.receive(context.Background(), request)
	if !strings.Contains(result.Error, "inbox limit") {
		t.Fatalf("over-budget delivery = %#v", result)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.entries) != 0 {
		t.Fatalf("over-budget delivery persisted %d entries", len(host.entries))
	}
}

func TestExchangeRejectsMismatchedResponseID(t *testing.T) {
	root := testRoot(t)
	listener, err := net.Listen("unix", filepath.Join(root, "mismatch.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = readBoundedLine(conn)
		_ = json.NewEncoder(conn).Encode(response{Version: protocolVersion, ID: "wrong", Delivered: true})
	}()
	if _, err := exchange(context.Background(), listener.Addr().String(), delivery("right", "")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("exchange error = %v", err)
	}
}
