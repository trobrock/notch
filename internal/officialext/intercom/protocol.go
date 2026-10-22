package intercom

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	protocolVersion    = 1
	maxBodyBytes       = 16 * 1024
	maxWireBytes       = 6*maxBodyBytes + 16*1024
	maxInboundEntries  = 1000
	maxOutboundEntries = 1000
	entryKind          = "intercom_inbound"
	outboundKind       = "intercom_outbound"
	configKind         = "intercom_config"
	wakeKind           = "intercom_wake_attempt"
	maxAutomatedWakes  = 8
)

// The versioned message envelope and receipt do not depend on local sockets.
// Future remote transports must authenticate routing separately, but can keep
// the same session IDs, request IDs, reply correlation, and delivery semantics.
type message struct {
	Version       int       `json:"version"`
	Type          string    `json:"type"`
	ID            string    `json:"id,omitempty"`
	Project       string    `json:"project,omitempty"`
	ToSession     string    `json:"to_session,omitempty"`
	FromSession   string    `json:"from_session,omitempty"`
	FromName      string    `json:"from_name,omitempty"`
	ReplyTo       string    `json:"reply_to,omitempty"`
	Wake          bool      `json:"wake,omitempty"`
	AutomatedWake bool      `json:"automated_wake,omitempty"`
	Body          string    `json:"body,omitempty"`
	SentAt        time.Time `json:"sent_at,omitempty"`
}

type response struct {
	WakeQueued bool   `json:"wake_queued,omitempty"`
	Version    int    `json:"version"`
	ID         string `json:"id,omitempty"`
	Delivered  bool   `json:"delivered,omitempty"`
	Duplicate  bool   `json:"duplicate,omitempty"`
	Warning    string `json:"warning,omitempty"`
	Error      string `json:"error,omitempty"`
}

func readBoundedLine(r io.Reader) ([]byte, error) {
	reader := bufio.NewReader(io.LimitReader(r, maxWireBytes+2))
	line, err := reader.ReadBytes('\n')
	if len(line) > maxWireBytes {
		return nil, errors.New("message exceeds wire limit")
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("message is not terminated by LF")
		}
		return nil, err
	}
	return line, nil
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create request ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func deliveryKey(sender, id string) string { return sender + "\x00" + id }

func messageFingerprint(request message) [32]byte {
	flags := fmt.Sprintf("%t\x00%t", request.Wake, request.AutomatedWake)
	return sha256.Sum256([]byte(request.FromSession + "\x00" + request.ReplyTo + "\x00" + flags + "\x00" + request.Body))
}

func validateMessage(request message) error {
	if request.Version != protocolVersion {
		return fmt.Errorf("unsupported intercom version %d", request.Version)
	}
	if request.Type != "deliver" {
		return fmt.Errorf("unsupported message type %q", request.Type)
	}
	if request.ID == "" || len(request.ID) > 128 {
		return errors.New("request ID is missing or too long")
	}
	if request.Project == "" || len(request.Project) > 64 || request.ToSession == "" || len(request.ToSession) > 256 {
		return errors.New("target project or session is missing or too long")
	}
	if request.FromSession == "" || len(request.FromSession) > 256 {
		return errors.New("sender session is missing or too long")
	}
	if len(request.FromName) > 64 || len(request.ReplyTo) > 128 {
		return errors.New("sender name or reply ID is too long")
	}
	if strings.TrimSpace(request.Body) == "" {
		return errors.New("message body is empty")
	}
	if len(request.Body) > maxBodyBytes {
		return fmt.Errorf("message exceeds %d-byte limit", maxBodyBytes)
	}
	return nil
}
