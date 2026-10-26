package intercom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/trobrock/notch/internal/extension"
	"strings"
)

func (s *service) command(ctx context.Context, args string) (string, error) {
	fields := strings.Fields(args)
	if len(fields) == 0 || fields[0] == "status" {
		return s.status(), nil
	}
	switch fields[0] {
	case "on", "off":
		if len(fields) != 1 {
			return "", errors.New("usage: /intercom on|off")
		}
		return s.setEnabled(ctx, fields[0] == "on")
	case "process":
		if len(fields) != 2 {
			return "", errors.New("usage: /intercom process REQUEST_ID")
		}
		return s.process(ctx, fields[1])
	case "retry":
		if len(fields) != 2 {
			return "", errors.New("usage: /intercom retry REQUEST_ID")
		}
		result, err := s.retry(ctx, fields[1])
		return formatDelivery(result), err
	case "list":
		peers, err := s.peers(ctx)
		if err != nil {
			return "", err
		}
		return formatPeers(peers), nil
	case "inbox":
		return s.formatInbox(), nil
	case "request":
		if len(fields) != 2 {
			return "", errors.New("usage: /intercom request REQUEST_ID")
		}
		return s.requestStatus(fields[1])
	case "wake":
		if len(fields) != 2 || (fields[1] != "on" && fields[1] != "off") {
			return "", errors.New("usage: /intercom wake on|off")
		}
		return s.setWake(ctx, fields[1] == "on")
	case "name":
		if len(fields) != 2 {
			return "", errors.New("usage: /intercom name NAME")
		}
		return s.setName(ctx, fields[1])
	case "collaboration":
		if len(fields) == 2 && fields[1] == "status" {
			return s.status(), nil
		}
		if len(fields) == 2 && fields[1] == "leave" {
			return s.setCollaboration(ctx, "")
		}
		if len(fields) == 3 && (fields[1] == "create" || fields[1] == "join") {
			return s.setCollaboration(ctx, fields[2])
		}
		return "", errors.New("usage: /intercom collaboration create NAME|join NAME|leave|status")
	case "send":
		parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(args, "send")), " ", 2)
		if len(parts) != 2 {
			return "", errors.New("usage: /intercom send TARGET MESSAGE")
		}
		result, err := s.send(ctx, parts[0], parts[1], "", true)
		return formatDelivery(result), err
	case "reply":
		parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(args, "reply")), " ", 2)
		if len(parts) != 2 {
			return "", errors.New("usage: /intercom reply REQUEST_ID MESSAGE")
		}
		result, err := s.resolveReply(ctx, parts[0], parts[1])
		return formatDelivery(result), err
	default:
		return "", errors.New("usage: /intercom on|off|status|list|inbox|process REQUEST_ID|retry REQUEST_ID|request REQUEST_ID|wake on|off|name NAME|collaboration create NAME|join NAME|leave|status|send TARGET MESSAGE|reply REQUEST_ID MESSAGE")
	}
}

func (s *service) formatInbox() string {
	s.mu.Lock()
	inbox := append([]inboundEntry(nil), s.inbox...)
	s.mu.Unlock()
	if len(inbox) == 0 {
		return "Intercom inbox is empty."
	}
	start := len(inbox) - 20
	if start < 0 {
		start = 0
	}
	var lines []string
	for _, item := range inbox[start:] {
		label := item.FromName
		if label == "" {
			label = shortID(item.FromSession)
		}
		if item.FromWorkspace != "" {
			label = item.FromWorkspace + "/" + label
		}
		reply := ""
		if item.ReplyTo != "" {
			reply = " reply-to=" + item.ReplyTo
		}
		trust := " untrusted-project-message"
		if item.Collaboration != "" {
			trust = fmt.Sprintf(" trusted-collaboration=%q (member may request work including edits)", item.Collaboration)
		}
		lines = append(lines, fmt.Sprintf("%s from=%s%s%s: %s", item.ID, label, reply, trust, item.Body))
	}
	return strings.Join(lines, "\n")
}

func formatPeers(peers []peerRecord) string {
	if len(peers) == 0 {
		return "No other intercom sessions are available in this scope."
	}
	lines := make([]string, 0, len(peers))
	for _, peer := range peers {
		name := peer.Name
		if peer.Workspace != "" {
			name = peer.Workspace + "/" + name
		}
		lines = append(lines, fmt.Sprintf("%s\t%s\t%s", name, peer.SessionID, peer.Mode))
	}
	return strings.Join(lines, "\n")
}

func formatDelivery(result response) string {
	if !result.Delivered {
		return ""
	}
	text := "Delivered intercom request " + result.ID + "."
	if result.Duplicate {
		text += " The peer had already stored it."
	}
	if result.Warning != "" {
		text += " Warning: " + result.Warning
	}
	return text
}

func (s *service) configureTool(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
	var args struct {
		Name          *string `json:"name"`
		Collaboration *string `json:"collaboration"`
		Enabled       *bool   `json:"enabled"`
		Wake          *bool   `json:"wake"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return extension.ToolResult{}, err
	}
	if args.Name != nil && !validName(*args.Name) {
		return extension.ToolResult{}, errors.New("name must be 1-32 letters, numbers, dots, dashes, or underscores")
	}
	if args.Collaboration != nil && *args.Collaboration != "" && !validName(*args.Collaboration) {
		return extension.ToolResult{}, errors.New("collaboration must be 1-32 letters, numbers, dots, dashes, or underscores")
	}
	if args.Name == nil && args.Collaboration == nil && args.Enabled == nil && args.Wake == nil {
		return extension.ToolResult{Content: s.status()}, nil
	}

	if args.Enabled != nil && !*args.Enabled {
		if _, err := s.setEnabled(ctx, false); err != nil {
			return extension.ToolResult{Content: err.Error(), IsError: true}, err
		}
	}
	if args.Collaboration != nil {
		s.mu.Lock()
		currentCollaboration, currentlyEnabled := s.collaboration, s.enabled
		s.mu.Unlock()
		if *args.Collaboration != currentCollaboration {
			if currentlyEnabled {
				if _, err := s.setEnabled(ctx, false); err != nil {
					return extension.ToolResult{Content: err.Error(), IsError: true}, err
				}
			}
			if _, err := s.setCollaboration(ctx, *args.Collaboration); err != nil {
				return extension.ToolResult{Content: err.Error(), IsError: true}, err
			}
		}
	}
	if args.Name != nil {
		if _, err := s.setName(ctx, *args.Name); err != nil {
			return extension.ToolResult{Content: err.Error(), IsError: true}, err
		}
	}
	if args.Wake != nil {
		if _, err := s.setWake(ctx, *args.Wake); err != nil {
			return extension.ToolResult{Content: err.Error(), IsError: true}, err
		}
	}
	if args.Enabled != nil && *args.Enabled {
		if _, err := s.setEnabled(ctx, true); err != nil {
			return extension.ToolResult{Content: err.Error(), IsError: true}, err
		}
	}
	return extension.ToolResult{Content: s.status()}, nil
}

func (s *service) listTool(ctx context.Context, _ json.RawMessage, _ func(string)) (extension.ToolResult, error) {
	peers, err := s.peers(ctx)
	return extension.ToolResult{Content: formatPeers(peers), IsError: err != nil}, err
}

func (s *service) sendTool(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
	var args struct {
		Target  string `json:"target"`
		Message string `json:"message"`
		Wake    bool   `json:"wake"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return extension.ToolResult{}, err
	}
	result, err := s.sendAutomated(ctx, args.Target, args.Message, args.Wake)
	return extension.ToolResult{Content: formatDelivery(result), IsError: err != nil}, err
}

func (s *service) replyTool(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
	var args struct {
		RequestID string `json:"request_id"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return extension.ToolResult{}, err
	}
	result, err := s.resolveReply(ctx, args.RequestID, args.Message)
	return extension.ToolResult{Content: formatDelivery(result), IsError: err != nil}, err
}

func (s *service) inboxTool(context.Context, json.RawMessage, func(string)) (extension.ToolResult, error) {
	return extension.ToolResult{Content: s.formatInbox()}, nil
}

func (s *service) requestStatus(requestID string) (string, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return "", errors.New("request_id is required")
	}
	s.mu.Lock()
	var request *outboundEntry
	for i := len(s.outbox) - 1; i >= 0; i-- {
		if s.outbox[i].ID == requestID {
			copy := s.outbox[i]
			request = &copy
			break
		}
	}
	if request == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("outbound request %q is not recorded in this session", requestID)
	}
	var replies []inboundEntry
	for _, entry := range s.inbox {
		if entry.ReplyTo == requestID && entry.FromSession == request.ToSession {
			replies = append(replies, entry)
		}
	}
	s.mu.Unlock()
	status := "delivered_pending_reply"
	if request.DeliveredAt.IsZero() {
		status = "pending_delivery"
	}
	if request.ReplyTo != "" {
		status = "reply_delivered"
		if request.DeliveredAt.IsZero() {
			status = "reply_pending_delivery"
		}
	} else if len(replies) > 0 {
		status = "replied"
	}
	body, err := json.MarshalIndent(map[string]any{"status": status, "request": request, "replies": replies}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (s *service) requestStatusTool(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
	var args struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return extension.ToolResult{}, err
	}
	content, err := s.requestStatus(args.RequestID)
	return extension.ToolResult{Content: content, IsError: err != nil}, err
}

func (s *service) retryTool(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
	var args struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return extension.ToolResult{}, err
	}
	result, err := s.retry(ctx, args.RequestID)
	return extension.ToolResult{Content: formatDelivery(result), IsError: err != nil}, err
}

func shortID(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

func readyWord(value bool) string {
	if value {
		return "ready"
	}
	return "stopped"
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	out := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) != 0 {
		out["required"] = required
	}
	return out
}

func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
