// Package goal implements bounded, durable single-agent goal mode.
package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

const (
	Source          = "official:goal"
	entryKind       = "goal_state"
	defaultMaxSteps = 8
	maxMaxSteps     = 50
	turnsPerStep    = 10
	noProgressLimit = 2
)

type snapshot struct {
	Version          int      `json:"version"`
	SessionID        string   `json:"session_id,omitempty"`
	ID               string   `json:"id"`
	Objective        string   `json:"objective,omitempty"`
	Checks           []string `json:"checks,omitempty"`
	Status           string   `json:"status"`
	MaxSteps         int      `json:"max_steps,omitempty"`
	Steps            int      `json:"steps,omitempty"`
	Turns            int      `json:"turns,omitempty"`
	NoProgress       int      `json:"no_progress,omitempty"`
	Progress         string   `json:"progress,omitempty"`
	ProgressEvidence string   `json:"progress_evidence,omitempty"`
	Completion       []proof  `json:"completion,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	CostSummary      string   `json:"cost_summary,omitempty"`
	ProgressRevision int      `json:"progress_revision,omitempty"`
	HandledRevision  int      `json:"handled_revision,omitempty"`
}

type proof struct {
	Check    string `json:"check"`
	Evidence string `json:"evidence"`
}

type state struct {
	host                extension.Host
	mu                  sync.Mutex
	sessionID           string
	goal                snapshot
	persisted           snapshot
	haltRun             bool
	completionRun       bool
	completionFinalTurn bool
	persistenceErr      error
}

func Register(registry *extension.Registry, host extension.Host) error {
	if registry == nil || host == nil {
		return errors.New("register goal: registry and host are required")
	}
	s := &state{host: host}
	batch := extension.Batch{
		Commands: []extension.Command{{
			Name: "goal", Description: "Run a bounded, durable single-agent goal", Source: Source,
			AllowWhileStreamingArgs: allowWhileStreaming, Execute: s.command,
		}},
		Tools: []extension.Tool{s.startTool(), s.progressTool(), s.completeTool(), s.pauseTool()},
		Hooks: []extension.HookRegistration{
			{Event: "session_start", Source: Source, Handler: s.sessionChange},
			{Event: "session_change", Source: Source, Handler: s.sessionChange},
			{Event: "before_agent_start", Source: Source, Handler: s.beforeAgent},
			{Event: "agent_end", Source: Source, Handler: s.agentEnd},
			{Event: "tool_denied", Source: Source, Handler: s.toolDenied},
			{Event: "run_end", Source: Source, Handler: s.runEnd},
			{Event: "session_shutdown", Source: Source, Handler: s.shutdown},
		},
	}
	_, err := registry.RegisterBatch(batch)
	return err
}

func (s *state) command(ctx context.Context, args string) (string, error) {
	args = strings.TrimSpace(args)
	s.mu.Lock()
	if s.persistenceErr != nil {
		err := s.persistenceErr
		status := s.statusLocked()
		s.mu.Unlock()
		if args == "" || args == "status" {
			return status, nil
		}
		return "", err
	}
	if err := s.refreshCurrentLocked(); err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()
	if args == "" || args == "status" {
		return s.status(), nil
	}
	name, rest := splitFirst(args)
	switch name {
	case "start":
		return s.start(ctx, rest)
	case "pause":
		return s.pause("paused by user")
	case "resume":
		return s.resume()
	case "clear":
		return s.clear()
	case "status":
		return s.status(), nil
	default:
		// /goal OBJECTIVE --check CHECK is a short form of /goal start ...
		return s.start(ctx, args)
	}
}

func (s *state) start(ctx context.Context, args string) (string, error) {
	objective, checks, maxSteps, err := parseStart(args)
	if err != nil {
		return "", err
	}
	if objective == "" {
		objective, err = s.host.Input(ctx, "Goal objective", "What should Notch accomplish?")
		if err != nil {
			return "", err
		}
		objective = strings.TrimSpace(objective)
	}
	if len(checks) == 0 {
		value, inputErr := s.host.Input(ctx, "Completion checks (one per line)", "Each line must be proven before completion")
		if inputErr != nil {
			return "", inputErr
		}
		checks = nonemptyLines(value)
	}
	if objective == "" || len(checks) == 0 {
		return "", errors.New("goal requires an objective and at least one completion check")
	}

	return s.startGoal(objective, checks, maxSteps, true)
}

func (s *state) startGoal(objective string, checks []string, maxSteps int, followUp bool) (string, error) {
	objective = strings.TrimSpace(objective)
	checks = append([]string(nil), checks...)
	for index := range checks {
		checks[index] = strings.TrimSpace(checks[index])
		if checks[index] == "" {
			return "", errors.New("goal completion checks cannot be empty")
		}
	}
	if objective == "" || len(checks) == 0 {
		return "", errors.New("goal requires an objective and at least one completion check")
	}
	if maxSteps < 1 || maxSteps > maxMaxSteps {
		return "", fmt.Errorf("max steps must be between 1 and %d", maxMaxSteps)
	}

	s.mu.Lock()
	if s.goal.Status == "active" {
		s.mu.Unlock()
		return "", errors.New("a goal is already active; pause or clear it first")
	}
	s.goal = snapshot{Version: 1, ID: strconv.FormatInt(time.Now().UnixNano(), 36), Objective: objective,
		Checks: checks, Status: "active", MaxSteps: maxSteps}
	err := s.persistLocked()
	s.updateStatusLocked()
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	if followUp {
		if err := s.host.FollowUp(startMessage(objective, checks)); err != nil {
			_, _ = s.pause("could not start: " + err.Error())
			return "", err
		}
	}
	return fmt.Sprintf("Goal started with %d completion check(s) and a %d-step limit.", len(checks), maxSteps), nil
}

func parseStart(args string) (string, []string, int, error) {
	fields := strings.Fields(args)
	maxSteps := defaultMaxSteps
	var objectiveWords []string
	var checks []string
	for index := 0; index < len(fields); {
		switch fields[index] {
		case "--check":
			index++
			start := index
			for index < len(fields) && fields[index] != "--check" && fields[index] != "--max-steps" {
				index++
			}
			if start == index {
				return "", nil, 0, errors.New("--check requires text")
			}
			checks = append(checks, strings.Join(fields[start:index], " "))
		case "--max-steps":
			if index+1 >= len(fields) {
				return "", nil, 0, errors.New("--max-steps requires a number")
			}
			n, err := strconv.Atoi(fields[index+1])
			if err != nil || n < 1 || n > maxMaxSteps {
				return "", nil, 0, fmt.Errorf("--max-steps must be between 1 and %d", maxMaxSteps)
			}
			maxSteps, index = n, index+2
		default:
			objectiveWords = append(objectiveWords, fields[index])
			index++
		}
	}
	return strings.Join(objectiveWords, " "), checks, maxSteps, nil
}

func (s *state) pause(reason string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.goal.Status != "active" {
		return "Goal is not active.", nil
	}
	s.goal.Status, s.goal.Reason = "paused", reason
	s.haltRun = true
	if err := s.persistLocked(); err != nil {
		return "", err
	}
	s.updateStatusLocked()
	return "Goal paused. Use /goal resume to continue.", nil
}

func (s *state) resume() (string, error) {
	s.mu.Lock()
	if s.goal.Status != "paused" {
		s.mu.Unlock()
		return "", errors.New("no paused goal to resume")
	}
	if s.goal.Steps >= s.goal.MaxSteps {
		s.mu.Unlock()
		return "", errors.New("goal step limit is exhausted; clear and start a new goal")
	}
	s.goal.Status, s.goal.Reason = "active", ""
	s.haltRun = false
	objective, checks := s.goal.Objective, append([]string(nil), s.goal.Checks...)
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.updateStatusLocked()
	s.mu.Unlock()
	if err := s.host.FollowUp(startMessage(objective, checks)); err != nil {
		_, _ = s.pause("could not resume: " + err.Error())
		return "", err
	}
	return "Goal resumed within its remaining step budget.", nil
}

func (s *state) clear() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.goal.Status == "" || s.goal.Status == "cleared" {
		return "No goal to clear.", nil
	}
	wasActive := s.goal.Status == "active"
	s.goal.Status, s.goal.Reason = "cleared", "cleared by user"
	if wasActive {
		s.haltRun = true
	}
	if err := s.persistLocked(); err != nil {
		return "", err
	}
	s.updateStatusLocked()
	return "Goal cleared. Its history remains in the append-only session.", nil
}

func (s *state) status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *state) statusLocked() string {
	if s.goal.Status == "" || s.goal.Status == "cleared" {
		return "No goal is set."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Goal %s (%d/%d steps, %d/%d model turns): %s", s.goal.Status, s.goal.Steps, s.goal.MaxSteps, s.goal.Turns, s.goal.MaxSteps*turnsPerStep, s.goal.Objective)
	for _, check := range s.goal.Checks {
		fmt.Fprintf(&b, "\n- [ ] %s", check)
	}
	if s.goal.Progress != "" {
		fmt.Fprintf(&b, "\nProgress: %s", s.goal.Progress)
	}
	if s.goal.Reason != "" {
		fmt.Fprintf(&b, "\nReason: %s", s.goal.Reason)
	}
	if s.goal.CostSummary != "" {
		fmt.Fprintf(&b, "\nBudget: %s", s.goal.CostSummary)
	}
	if len(s.goal.Completion) != 0 {
		b.WriteString("\nCompletion evidence:")
		for _, item := range s.goal.Completion {
			fmt.Fprintf(&b, "\n- %s: %s", item.Check, item.Evidence)
		}
	}
	return b.String()
}

func (s *state) beforeAgent(_ context.Context, event map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.haltRun {
		s.haltRun = false
		return map[string]any{"stop": true, "stop_reason": "goal_paused"}, nil
	}
	if s.persistenceErr != nil {
		return map[string]any{"stop": true, "stop_reason": "goal_state_error"}, nil
	}
	if err := s.refreshCurrentLocked(); err != nil {
		if s.goal.Status == "active" {
			return nil, err
		}
		return nil, nil
	}
	if s.completionRun {
		if s.completionFinalTurn {
			s.completionFinalTurn = false
			return nil, nil
		}
		return map[string]any{"stop": true, "stop_reason": "goal_completed"}, nil
	}
	if s.goal.Status != "active" {
		return nil, nil
	}
	if s.goal.Turns >= s.goal.MaxSteps*turnsPerStep {
		s.goal.Status, s.goal.Reason = "paused", "goal model-turn limit reached"
		if err := s.persistLocked(); err != nil {
			s.goal.Status, s.goal.Reason = "paused", "goal model-turn limit reached; state could not be persisted: "+err.Error()
			s.persistenceErr = err
		}
		s.updateStatusLocked()
		return map[string]any{"stop": true, "stop_reason": "goal_turn_limit"}, nil
	}
	s.goal.Turns++
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	base, _ := event["system_prompt"].(string)
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n<goal_mode>\nYou are working on an opt-in bounded goal. Work on the objective, not on this control text. Human messages have priority. Call report_goal_progress once per work cycle with concrete new evidence. Do not claim completion in prose. Call complete_goal only after every completion check is met with concise evidence. If blocked by permissions or missing input, call pause_goal with the exact reason.\nObjective: ")
	b.WriteString(s.goal.Objective)
	b.WriteString("\nCompletion checks:")
	for _, check := range s.goal.Checks {
		b.WriteString("\n- ")
		b.WriteString(check)
	}
	fmt.Fprintf(&b, "\nBudget: work step %d of %d; model turn %d of %d. Existing Notch turn and cost limits also apply. A cost limit can overshoot by one in-flight model call, and unknown pricing stops cost-limited runs.\n</goal_mode>", s.goal.Steps+1, s.goal.MaxSteps, s.goal.Turns, s.goal.MaxSteps*turnsPerStep)
	return map[string]any{"system_prompt": b.String()}, nil
}

func (s *state) agentEnd(_ context.Context, _ map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.goal.Status != "active" {
		return nil, nil
	}
	s.goal.Steps++
	if s.goal.ProgressRevision > s.goal.HandledRevision {
		s.goal.HandledRevision, s.goal.NoProgress = s.goal.ProgressRevision, 0
	} else {
		s.goal.NoProgress++
	}
	switch {
	case s.goal.Steps >= s.goal.MaxSteps:
		s.goal.Status, s.goal.Reason = "paused", "goal step limit reached"
	case s.goal.NoProgress >= noProgressLimit:
		s.goal.Status, s.goal.Reason = "paused", "stopped after repeated no progress"
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	s.updateStatusLocked()
	if s.goal.Status != "active" {
		return nil, nil
	}
	return map[string]any{"follow_up": "Continue the active goal. Use the completion checks, report only concrete new progress, and call complete_goal only with evidence for every check."}, nil
}

func (s *state) toolDenied(_ context.Context, event map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.goal.Status != "active" && !(s.goal.Status == "completed" && s.completionRun) {
		return nil, nil
	}
	name, _ := event["name"].(string)
	reason, _ := event["reason"].(string)
	intendedReason := "blocked: tool " + name + " was denied"
	if reason != "" {
		intendedReason += ": " + reason
	}
	s.goal.Status, s.goal.Reason = "paused", intendedReason
	s.completionRun, s.completionFinalTurn = false, false
	s.haltRun = true
	if err := s.persistLocked(); err != nil {
		s.goal.Status, s.goal.Reason = "paused", intendedReason+"; state could not be persisted: "+err.Error()
		s.persistenceErr = err
		s.updateStatusLocked()
		return nil, nil
	}
	s.updateStatusLocked()
	return nil, nil
}

func (s *state) runEnd(_ context.Context, event map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.haltRun = false
	s.completionRun, s.completionFinalTurn = false, false
	if s.goal.Status == "" || s.goal.Status == "cleared" {
		return nil, nil
	}
	stop, _ := event["stop_reason"].(string)
	changed := false
	if s.goal.Status == "active" && stop != "end_turn" {
		s.goal.Status = "paused"
		s.goal.Reason = map[string]string{"canceled": "paused after abort", "max_turns": "Notch turn limit reached", "max_cost": "Notch cost limit reached"}[stop]
		if s.goal.Reason == "" {
			s.goal.Reason = "run stopped: " + stop
		}
		changed = true
	}
	known, _ := event["cost_known"].(bool)
	if !known {
		s.goal.CostSummary = "cost unknown; requested cost ceilings cannot be enforced"
		changed = true
	} else if cost, ok := event["total_cost_usd"].(*float64); ok && cost != nil {
		s.goal.CostSummary = fmt.Sprintf("run cost $%.6f; limits are checked after each model call and may overshoot in flight", *cost)
		changed = true
	}
	if !changed {
		return nil, nil
	}
	if err := s.persistLocked(); err != nil {
		s.goal.Status = "paused"
		s.goal.Reason = "run stopped but goal state could not be persisted: " + err.Error()
		s.persistenceErr = err
		s.updateStatusLocked()
		return nil, nil
	}
	s.updateStatusLocked()
	return nil, nil
}

func (s *state) shutdown(_ context.Context, _ map[string]any) (map[string]any, error) {
	_, err := s.pause("paused when Notch stopped")
	return nil, err
}

func (s *state) sessionChange(_ context.Context, event map[string]any) (map[string]any, error) {
	id, _ := event["session_id"].(string)
	entries, err := s.host.SessionEntries(entryKind)
	if err != nil {
		// Session persistence is optional. Keep Notch usable, but /goal start will
		// return the host's persistence error when the user explicitly opts in.
		if id == "" {
			s.mu.Lock()
			s.sessionID, s.goal, s.persisted = "", snapshot{}, snapshot{}
			s.haltRun, s.persistenceErr = false, nil
			s.completionRun, s.completionFinalTurn = false, false
			s.updateStatusLocked()
			s.mu.Unlock()
			return nil, nil
		}
		return nil, err
	}
	var loaded snapshot
	if len(entries) != 0 {
		if err := json.Unmarshal(entries[len(entries)-1], &loaded); err != nil {
			return nil, fmt.Errorf("load goal state: %w", err)
		}
	}
	s.mu.Lock()
	if s.sessionID == id && s.goal.ID != "" && s.goal.ID == loaded.ID && s.goal.Status == "active" {
		// A delayed asynchronous session_change must not overwrite an explicit
		// resume or a goal started after the session was already installed.
		s.updateStatusLocked()
		s.mu.Unlock()
		return nil, nil
	}
	s.sessionID = id
	s.haltRun, s.persistenceErr = false, nil
	s.completionRun, s.completionFinalTurn = false, false
	loaded.SessionID = id
	s.goal, s.persisted = loaded, cloneSnapshot(loaded)
	if s.goal.Status == "active" {
		s.goal.Status, s.goal.Reason = "paused", "restored paused; use /goal resume"
		err = s.persistLocked()
	}
	s.updateStatusLocked()
	s.mu.Unlock()
	return nil, err
}

func (s *state) startTool() extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "start_goal",
		Description: "Start a bounded /goal only when the user explicitly asks for goal mode. Use completion checks that match the user's requested outcome.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"objective": map[string]any{"type": "string"},
			"checks":    map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}},
			"max_steps": map[string]any{"type": "integer", "minimum": 1, "maximum": maxMaxSteps, "description": "Work-step limit. Defaults to 8."},
		}, "required": []any{"objective", "checks"}}}, Execute: func(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		var input struct {
			Objective string   `json:"objective"`
			Checks    []string `json:"checks"`
			MaxSteps  int      `json:"max_steps"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return extension.ToolResult{}, err
		}
		if input.MaxSteps == 0 {
			input.MaxSteps = defaultMaxSteps
		}
		content, err := s.startGoal(input.Objective, input.Checks, input.MaxSteps, false)
		if err != nil {
			return extension.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		return extension.ToolResult{Content: content + " Begin work now and use the goal progress, completion, or pause tools as needed."}, nil
	}}
}

func (s *state) progressTool() extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "report_goal_progress",
		Description: "Record concrete new progress for the active /goal. Repeating the same summary does not count as progress.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"summary": map[string]any{"type": "string"}, "evidence": map[string]any{"type": "string"},
		}, "required": []any{"summary"}}}, Execute: func(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		var input struct {
			Summary  string `json:"summary"`
			Evidence string `json:"evidence"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return extension.ToolResult{}, err
		}
		input.Summary, input.Evidence = strings.TrimSpace(input.Summary), strings.TrimSpace(input.Evidence)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.goal.Status != "active" {
			return extension.ToolResult{Content: "No active goal.", IsError: true}, nil
		}
		if input.Summary == "" {
			return extension.ToolResult{Content: "Progress summary is required.", IsError: true}, nil
		}
		if normalized(input.Summary) == normalized(s.goal.Progress) {
			return extension.ToolResult{Content: "Repeated progress does not count. Do new work or explain the block.", IsError: true}, nil
		}
		s.goal.Progress, s.goal.ProgressEvidence = input.Summary, input.Evidence
		s.goal.ProgressRevision++
		if err := s.persistLocked(); err != nil {
			return extension.ToolResult{}, err
		}
		s.updateStatusLocked()
		return extension.ToolResult{Content: "Progress recorded."}, nil
	}}
}

func (s *state) completeTool() extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "complete_goal",
		Description: "Complete the active /goal only with evidence for every configured completion check.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"evidence": map[string]any{
			"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"check": map[string]any{"type": "string"}, "evidence": map[string]any{"type": "string"}},
				"required": []any{"check", "evidence"}}}}, "required": []any{"evidence"}}}, Execute: func(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		var input struct {
			Evidence []proof `json:"evidence"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return extension.ToolResult{}, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.goal.Status != "active" {
			return extension.ToolResult{Content: "No active goal.", IsError: true}, nil
		}
		if s.goal.ProgressRevision == 0 {
			return extension.ToolResult{Content: "Completion rejected: record concrete progress before completion.", IsError: true}, nil
		}
		byCheck := make(map[string]string, len(input.Evidence))
		for _, item := range input.Evidence {
			byCheck[strings.TrimSpace(item.Check)] = strings.TrimSpace(item.Evidence)
		}
		proofs := make([]proof, 0, len(s.goal.Checks))
		for _, check := range s.goal.Checks {
			evidence := byCheck[check]
			if evidence == "" {
				return extension.ToolResult{Content: "Completion rejected: missing evidence for check: " + check, IsError: true}, nil
			}
			proofs = append(proofs, proof{Check: check, Evidence: evidence})
		}
		s.goal.Status, s.goal.Reason, s.goal.Completion = "completed", "all checks reported with evidence", proofs
		if err := s.persistLocked(); err != nil {
			return extension.ToolResult{}, err
		}
		s.completionRun, s.completionFinalTurn = true, true
		s.updateStatusLocked()
		return extension.ToolResult{Content: "Goal completed. Give the user a concise result and the recorded evidence."}, nil
	}}
}

func (s *state) pauseTool() extension.Tool {
	return extension.Tool{Source: Source, Definition: model.ToolDefinition{Name: "pause_goal",
		Description: "Pause the active /goal when permissions, required input, or another concrete blocker prevents progress.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"reason": map[string]any{"type": "string"},
		}, "required": []any{"reason"}}}, Execute: func(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
		var input struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return extension.ToolResult{}, err
		}
		input.Reason = strings.TrimSpace(input.Reason)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.goal.Status != "active" {
			return extension.ToolResult{Content: "No active goal.", IsError: true}, nil
		}
		if input.Reason == "" {
			return extension.ToolResult{Content: "A concrete blocker reason is required.", IsError: true}, nil
		}
		s.goal.Status, s.goal.Reason = "paused", "blocked: "+input.Reason
		s.haltRun = true
		if err := s.persistLocked(); err != nil {
			return extension.ToolResult{}, err
		}
		s.updateStatusLocked()
		return extension.ToolResult{Content: "Goal paused. Tell the user what is needed to resume."}, nil
	}}
}

func (s *state) persistLocked() error {
	copy := cloneSnapshot(s.goal)
	copy.SessionID = s.sessionID
	if err := s.host.AppendSessionEntry(s.sessionID, entryKind, copy); err != nil {
		s.goal = cloneSnapshot(s.persisted)
		return err
	}
	s.goal, s.persisted = cloneSnapshot(copy), cloneSnapshot(copy)
	s.persistenceErr = nil
	return nil
}

func cloneSnapshot(value snapshot) snapshot {
	value.Checks = append([]string(nil), value.Checks...)
	value.Completion = append([]proof(nil), value.Completion...)
	return value
}

func (s *state) refreshCurrentLocked() error {
	currentID := s.host.SessionID()
	entries, err := s.host.SessionEntries(entryKind)
	if err != nil {
		return err
	}
	var loaded snapshot
	if len(entries) != 0 {
		if err := json.Unmarshal(entries[len(entries)-1], &loaded); err != nil {
			return fmt.Errorf("load goal state: %w", err)
		}
	}
	if len(entries) == 0 {
		s.sessionID, s.goal, s.persisted = currentID, snapshot{}, snapshot{}
		return nil
	}
	changedSession := currentID != s.sessionID
	loaded.SessionID = currentID
	s.sessionID, s.goal, s.persisted = currentID, loaded, cloneSnapshot(loaded)
	if changedSession && s.goal.Status == "active" {
		s.goal.Status, s.goal.Reason = "paused", "restored paused; use /goal resume"
		if err := s.persistLocked(); err != nil {
			return err
		}
		s.updateStatusLocked()
	}
	return nil
}

func (s *state) updateStatusLocked() {
	if s.goal.Status == "" || s.goal.Status == "cleared" {
		s.host.SetStatus("goal", "")
		return
	}
	s.host.SetStatus("goal", fmt.Sprintf("goal:%s %d/%d", s.goal.Status, s.goal.Steps, s.goal.MaxSteps))
}

func allowWhileStreaming(args string) bool {
	name, _ := splitFirst(strings.TrimSpace(args))
	return name == "" || name == "status" || name == "pause" || name == "clear"
}

func startMessage(objective string, checks []string) string {
	var b strings.Builder
	b.WriteString("Start the active goal now.\n\nObjective: ")
	b.WriteString(objective)
	b.WriteString("\n\nCompletion checks:")
	for _, check := range checks {
		b.WriteString("\n- ")
		b.WriteString(check)
	}
	return b.String()
}

func splitFirst(value string) (string, string) {
	if index := strings.IndexAny(value, " \t\n"); index >= 0 {
		return value[:index], strings.TrimSpace(value[index:])
	}
	return value, ""
}
func nonemptyLines(value string) []string {
	var result []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}
func normalized(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
