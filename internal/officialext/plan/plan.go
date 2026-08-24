// Package plan implements Notch's official plan mode extension.
package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

const Source = "official:plan"

var readOnly = []string{"ask_user_question", "exit_plan_mode", "explore_codebase", "find", "grep", "ls", "read", "run_subagent"}

type state struct {
	host         extension.Host
	mu           sync.Mutex
	enabled      bool
	approvedPlan string
}

func Register(registry *extension.Registry, host extension.Host) error {
	if registry == nil || host == nil {
		return errors.New("register plan: registry and host are required")
	}
	s := &state{host: host}
	if err := registry.RegisterCommand(extension.Command{Name: "plan", Description: "Toggle read-only plan mode", Source: Source, Execute: func(ctx context.Context, args string) (string, error) { return s.command(ctx, args) }}); err != nil {
		return err
	}
	if err := registry.RegisterTool(s.exitTool()); err != nil {
		return err
	}
	registry.On("before_agent_start", Source, s.before)
	registry.On("tool_call", Source, s.guard)
	registry.On("agent_end", Source, s.end)
	registry.On("session_shutdown", Source, func(context.Context, map[string]any) (map[string]any, error) { s.disable(); return nil, nil })
	return nil
}

func (s *state) command(_ context.Context, args string) (string, error) {
	switch strings.TrimSpace(args) {
	case "", "toggle":
		s.mu.Lock()
		enabled := !s.enabled
		s.mu.Unlock()
		if enabled {
			return s.enable()
		}
		s.disable()
		return "Plan mode disabled. Full tool access restored.", nil
	case "on":
		return s.enable()
	case "off":
		s.disable()
		return "Plan mode disabled. Full tool access restored.", nil
	case "status":
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.enabled {
			return "Plan mode is active.", nil
		}
		return "Plan mode is inactive.", nil
	default:
		return "", errors.New("usage: /plan [on|off|status]")
	}
}
func (s *state) enable() (string, error) {
	s.mu.Lock()
	s.enabled = true
	s.approvedPlan = ""
	s.mu.Unlock()
	if err := s.host.SetActiveTools(readOnly); err != nil {
		return "", err
	}
	s.host.SetStatus("plan", "plan")
	return "Plan mode enabled. Investigate with read-only tools, present a concrete plan, then call exit_plan_mode for approval.", nil
}
func (s *state) disable() {
	s.mu.Lock()
	s.enabled = false
	s.approvedPlan = ""
	s.mu.Unlock()
	_ = s.host.SetActiveTools(nil)
	s.host.SetStatus("plan", "")
}
func (s *state) isEnabled() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.enabled }

func (s *state) exitTool() extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "exit_plan_mode", Description: "Present the completed implementation plan for approval and choose fresh or current context implementation.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"plan": map[string]any{"type": "string", "minLength": 1, "description": "Complete implementation plan to approve."}}, "required": []string{"plan"}, "additionalProperties": false}}, Execute: func(ctx context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		if !s.isEnabled() {
			return extension.ToolResult{Content: "Plan mode is not active."}, nil
		}
		var input struct {
			Plan string `json:"plan"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil {
			return extension.ToolResult{}, err
		}
		input.Plan = strings.TrimSpace(input.Plan)
		if input.Plan == "" {
			return extension.ToolResult{}, errors.New("plan must not be empty")
		}
		choice, err := s.host.Select(ctx, "Plan complete. How should implementation start?", []string{"Implement in fresh context (recommended)", "Implement with current context", "Stay in plan mode"})
		if err != nil {
			return extension.ToolResult{}, err
		}
		if strings.HasPrefix(choice, "Stay") {
			return extension.ToolResult{Content: "User chose to stay in plan mode. Continue refining the plan."}, nil
		}
		fresh := strings.Contains(choice, "fresh context")
		s.mu.Lock()
		if fresh {
			s.approvedPlan = "[fresh]\n" + input.Plan
		} else {
			s.approvedPlan = input.Plan
		}
		s.mu.Unlock()
		return extension.ToolResult{Content: fmt.Sprintf("Plan approved for implementation (%s). Finish this turn now.", map[bool]string{true: "fresh context", false: "current context"}[fresh]), Details: map[string]any{"approved": true, "fresh": fresh, "plan": input.Plan}}, nil
	}}
}

func (s *state) before(_ context.Context, event map[string]any) (map[string]any, error) {
	if !s.isEnabled() {
		return nil, nil
	}
	prompt, _ := event["system_prompt"].(string)
	return map[string]any{"system_prompt": prompt + `\n\nPLAN MODE IS ACTIVE.
Investigate and produce a researched implementation plan before changes.
Do not modify files, run mutating commands, start servers, install packages, commit, or push.
Use read-only tools. Ask concise questions when requirements are ambiguous.
Your final plan must name key files, implementation steps, tests, risks, and open questions.
When complete, call exit_plan_mode with the full plan. Do not begin implementation.`}, nil
}
func (s *state) guard(_ context.Context, event map[string]any) (map[string]any, error) {
	if !s.isEnabled() {
		return nil, nil
	}
	name, _ := event["name"].(string)
	if name == "run_subagent" {
		arguments, _ := event["arguments"].(map[string]any)
		allowWrite, _ := arguments["allowWriteTools"].(bool)
		tools, _ := arguments["tools"].(string)
		if allowWrite || (tools != "" && !onlyReadOnlySubagentTools(tools)) {
			return map[string]any{"denied": true, "reason": "Plan mode blocks write-capable subagents until the plan is approved."}, nil
		}
	}
	for _, allowed := range readOnly {
		if name == allowed {
			return nil, nil
		}
	}
	return map[string]any{"denied": true, "reason": "Plan mode blocks " + name + " until the plan is approved."}, nil
}

func onlyReadOnlySubagentTools(value string) bool {
	allowed := map[string]bool{"read": true, "grep": true, "find": true, "ls": true}
	for _, name := range strings.Split(value, ",") {
		if !allowed[strings.TrimSpace(name)] {
			return false
		}
	}
	return true
}
func (s *state) end(_ context.Context, _ map[string]any) (map[string]any, error) {
	s.mu.Lock()
	plan := s.approvedPlan
	s.approvedPlan = ""
	if plan != "" {
		s.enabled = false
	}
	s.mu.Unlock()
	if plan == "" {
		return nil, nil
	}
	_ = s.host.SetActiveTools(nil)
	s.host.SetStatus("plan", "")
	message := "The plan has been approved. Execute it without replanning.\n\n## Approved plan\n" + plan
	// Details from exit_plan_mode are not available to agent_end, so the chosen
	// context mode is encoded alongside the plan in approvedPlan by execute.
	fresh := strings.HasPrefix(plan, "[fresh]\n")
	if fresh {
		message = "The plan has been approved. Execute it without replanning.\n\n## Approved plan\n" + strings.TrimPrefix(plan, "[fresh]\n")
	}
	return nil, s.host.Handoff(message, fresh)
}
