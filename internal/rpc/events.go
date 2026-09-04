package rpc

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/trobrock/notch/internal/agent"
	"github.com/trobrock/notch/internal/delegation"
	"github.com/trobrock/notch/internal/model"
)

type eventAdapter struct {
	server   *Server
	state    StateConfig
	current  *rpcTurn
	messages []any
}

type rpcTurn struct {
	startedAt   int64
	streamType  string
	streamIndex int
	streamText  string
	message     map[string]any
	toolResults []map[string]any
	ended       bool
}

func newEventAdapter(server *Server, state StateConfig) *eventAdapter {
	return &eventAdapter{server: server, state: state}
}

func (a *eventAdapter) Start() { _ = a.server.write(map[string]any{"type": "agent_start"}) }

func (a *eventAdapter) Handle(event agent.Event) {
	switch event.Type {
	case "session_start", "run_end":
		_ = a.server.write(event)
	case "turn_start":
		a.flushTurn()
		a.current = &rpcTurn{startedAt: time.Now().UnixMilli(), streamIndex: -1, toolResults: make([]map[string]any, 0)}
		_ = a.server.write(map[string]any{"type": "turn_start"})
		_ = a.server.write(map[string]any{"type": "message_start", "message": a.emptyAssistantMessage()})
	case "text_delta", "thinking_delta":
		a.streamDelta(event.Type, event.Text)
	case "turn_end":
		a.endAssistant(event)
	case "tool_start":
		args := any(map[string]any{})
		if len(event.Arguments) != 0 {
			_ = json.Unmarshal(event.Arguments, &args)
		}
		_ = a.server.write(map[string]any{
			"type": "tool_execution_start", "toolCallId": event.ToolCallID,
			"toolName": event.ToolName, "args": args,
		})
	case "tool_update":
		update := map[string]any{
			"type": "tool_execution_update", "toolCallId": event.ToolCallID,
			"toolName":      event.ToolName,
			"partialResult": map[string]any{"content": []map[string]any{{"type": "text", "text": event.Text}}},
		}
		if event.ToolUpdateMode != "" {
			update["updateMode"] = event.ToolUpdateMode
		}
		_ = a.server.write(update)
	case "tool_end":
		a.endTool(event)
	case "queue_update":
		steering, followUp := make([]string, 0), make([]string, 0)
		for _, queued := range event.Queue {
			if queued.Mode == "follow_up" {
				followUp = append(followUp, queued.Text)
			} else {
				steering = append(steering, queued.Text)
			}
		}
		_ = a.server.write(map[string]any{"type": "queue_update", "steering": steering, "followUp": followUp})
	case "compaction_start":
		a.server.setCompacting(true)
		reason := "manual"
		if event.Auto {
			reason = "threshold"
		}
		_ = a.server.write(map[string]any{"type": "compaction_start", "reason": reason})
	case "compaction_end":
		a.server.setCompacting(false)
		reason := "manual"
		if event.Auto {
			reason = "threshold"
		}
		_ = a.server.write(map[string]any{"type": "compaction_end", "reason": reason, "aborted": false, "willRetry": false})
	case "delegation_usage":
		_ = a.server.write(map[string]any{"type": "delegation_usage", "usage": rpcDelegationUsage(event.DelegationUsage)})
	case "provider_retry":
		_ = a.server.write(map[string]any{"type": "provider_retry", "attempt": event.Attempt, "maxAttempts": event.MaxAttempts, "delayMs": event.DelayMS, "error": event.Text})
	case "error":
		_ = a.server.write(map[string]any{"type": "error", "error": event.Text})
	}
}

func (a *eventAdapter) Finish(err error) {
	if err != nil && a.current != nil && !a.current.ended {
		a.finishStreamBlock()
		message := a.emptyAssistantMessage()
		message["stopReason"] = stopReason(err.Error(), nil)
		message["errorMessage"] = err.Error()
		a.current.message, a.current.ended = message, true
		_ = a.server.write(map[string]any{"type": "message_end", "message": message})
		a.messages = append(a.messages, message)
	}
	a.flushTurn()
	event := map[string]any{"type": "agent_end", "messages": a.messages, "willRetry": false}
	if err != nil {
		event["error"] = err.Error()
	}
	_ = a.server.write(event)
}

func (a *eventAdapter) Settled() { _ = a.server.write(map[string]any{"type": "agent_settled"}) }

func (a *eventAdapter) streamDelta(eventType, delta string) {
	if a.current == nil {
		return
	}
	blockType := "text"
	if eventType == "thinking_delta" {
		blockType = "thinking"
	}
	if a.current.streamType != blockType {
		a.finishStreamBlock()
		a.current.streamType = blockType
		a.current.streamIndex++
		a.current.streamText = ""
		a.messageUpdate(map[string]any{"type": blockType + "_start", "contentIndex": a.current.streamIndex})
	}
	a.current.streamText += delta
	a.messageUpdate(map[string]any{"type": blockType + "_delta", "contentIndex": a.current.streamIndex, "delta": delta})
}

func (a *eventAdapter) finishStreamBlock() {
	if a.current == nil || a.current.streamType == "" {
		return
	}
	a.messageUpdate(map[string]any{
		"type": a.current.streamType + "_end", "contentIndex": a.current.streamIndex,
		"content": a.current.streamText,
	})
	a.current.streamType, a.current.streamText = "", ""
}

func (a *eventAdapter) messageUpdate(delta map[string]any) {
	_ = a.server.write(map[string]any{
		"type": "message_update", "usage": rpcUsage(nil), "assistantMessageEvent": delta,
	})
}

func (a *eventAdapter) endAssistant(event agent.Event) {
	if a.current == nil || a.current.ended {
		return
	}
	a.finishStreamBlock()
	message := a.assistantMessage(event.Message, event.Usage, event.StopReason, a.current.startedAt)
	a.current.message, a.current.ended = message, true
	_ = a.server.write(map[string]any{"type": "message_end", "message": message})
	a.messages = append(a.messages, message)
}

func (a *eventAdapter) endTool(event agent.Event) {
	content := ""
	isError := false
	details := any(nil)
	if event.Result != nil {
		content, isError, details = event.Result.Content, event.Result.IsError, event.Result.Details
	}
	result := map[string]any{
		"role": "toolResult", "toolCallId": event.ToolCallID, "toolName": event.ToolName,
		"content": []map[string]any{{"type": "text", "text": content}},
		"isError": isError, "timestamp": time.Now().UnixMilli(),
	}
	if details != nil {
		result["details"] = details
	}
	_ = a.server.write(map[string]any{"type": "tool_execution_end", "toolCallId": event.ToolCallID, "toolName": event.ToolName, "result": map[string]any{"content": result["content"], "details": details}, "isError": isError})
	_ = a.server.write(map[string]any{"type": "message_start", "message": result})
	_ = a.server.write(map[string]any{"type": "message_end", "message": result})
	if a.current != nil {
		a.current.toolResults = append(a.current.toolResults, result)
	}
	a.messages = append(a.messages, result)
}

func (a *eventAdapter) flushTurn() {
	if a.current == nil {
		return
	}
	if !a.current.ended {
		a.finishStreamBlock()
		message := a.emptyAssistantMessage()
		a.current.message, a.current.ended = message, true
		_ = a.server.write(map[string]any{"type": "message_end", "message": message})
		a.messages = append(a.messages, message)
	}
	_ = a.server.write(map[string]any{"type": "turn_end", "message": a.current.message, "toolResults": a.current.toolResults})
	a.current = nil
}

func (a *eventAdapter) emptyAssistantMessage() map[string]any {
	return map[string]any{
		"role": "assistant", "content": []any{}, "api": a.state.API,
		"provider": a.state.Provider, "model": a.state.Model, "usage": rpcUsage(nil),
		"stopReason": "stop", "timestamp": time.Now().UnixMilli(),
	}
}

func (a *eventAdapter) assistantMessage(message *model.Message, usage *agent.Usage, reason string, timestamp int64) map[string]any {
	content := make([]any, 0)
	if message != nil {
		for _, block := range message.Content {
			switch block.Type {
			case "text":
				content = append(content, map[string]any{"type": "text", "text": block.Text})
			case "thinking", "reasoning":
				content = append(content, map[string]any{"type": "thinking", "thinking": block.Text})
			case "tool_use", "function_call":
				arguments := any(map[string]any{})
				if len(block.Arguments) != 0 {
					_ = json.Unmarshal(block.Arguments, &arguments)
				}
				content = append(content, map[string]any{"type": "toolCall", "id": block.ID, "name": block.Name, "arguments": arguments})
			}
		}
	}
	return map[string]any{
		"role": "assistant", "content": content, "api": a.state.API,
		"provider": a.state.Provider, "model": a.state.Model, "usage": rpcUsage(usage),
		"stopReason": stopReason(reason, message), "timestamp": timestamp,
	}
}

func rpcUsage(usage *agent.Usage) map[string]any {
	providerInput, providerOutput, cacheRead, cacheWrite, reasoning := 0, 0, 0, 0, 0
	totalCost, costKnown := 0.0, false
	var providerCost, estimatedCost any
	costSource, pricingVersion := "", ""
	if usage != nil {
		providerInput, providerOutput = usage.InputTokens, usage.OutputTokens
		cacheRead, cacheWrite, reasoning = usage.CacheReadTokens, usage.CacheWriteTokens, usage.ReasoningTokens
		if usage.CostUSD != nil {
			totalCost, costKnown = *usage.CostUSD, true
		}
		if usage.ProviderCostUSD != nil {
			providerCost = *usage.ProviderCostUSD
		}
		if usage.EstimatedCostUSD != nil {
			estimatedCost = *usage.EstimatedCostUSD
		}
		costSource, pricingVersion = usage.CostSource, usage.PricingVersion
	}
	providerTokens := providerInput + providerOutput + cacheRead + cacheWrite
	return map[string]any{
		"input": providerInput, "output": providerOutput, "cacheRead": cacheRead, "cacheWrite": cacheWrite,
		"reasoning":      reasoning,
		"providerTokens": providerTokens,
		"totalTokens":    providerTokens,
		"cost": map[string]float64{
			"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": totalCost,
		},
		"costKnown":        costKnown,
		"costSource":       costSource,
		"providerCostUSD":  providerCost,
		"estimatedCostUSD": estimatedCost,
		"pricingVersion":   pricingVersion,
	}
}

func rpcDelegationUsage(usage *delegation.Usage) map[string]any {
	if usage == nil {
		return map[string]any{"turns": 0, "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "reasoning": 0, "costUSD": nil, "wallMs": 0, "calls": 0, "totalTokens": 0}
	}
	var cost any
	if usage.CostUSD != nil {
		cost = *usage.CostUSD
	}
	return map[string]any{
		"turns": usage.Turns, "input": usage.InputTokens, "output": usage.OutputTokens,
		"cacheRead": usage.CacheReadTokens, "cacheWrite": usage.CacheWriteTokens,
		"reasoning": usage.ReasoningTokens, "costUSD": cost,
		"wallMs": usage.WallMS, "calls": usage.Calls, "totalTokens": usage.TotalTokens(),
	}
}

func stopReason(reason string, message *model.Message) string {
	if message != nil {
		for _, block := range message.Content {
			if block.Type == "tool_use" || block.Type == "function_call" {
				return "toolUse"
			}
		}
	}
	reason = strings.ToLower(reason)
	switch {
	case strings.Contains(reason, "cancel"), strings.Contains(reason, "abort"):
		return "aborted"
	case strings.Contains(reason, "length"), strings.Contains(reason, "max_token"):
		return "length"
	case strings.Contains(reason, "error"):
		return "error"
	default:
		return "stop"
	}
}
