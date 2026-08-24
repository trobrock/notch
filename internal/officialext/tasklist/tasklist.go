// Package tasklist implements Notch's official update_task_list extension.
package tasklist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
)

const (
	Source    = "official:task-list"
	ToolName  = "update_task_list"
	statusKey = "tasks"
	maxTasks  = 100
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

type Task struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
}

type Summary struct {
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	InProgress int `json:"inProgress"`
	Completed  int `json:"completed"`
	Cancelled  int `json:"cancelled"`
}

type inputTask struct {
	ID       string `json:"id,omitempty"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
}

type input struct {
	Todos []inputTask `json:"todos"`
}

type tracker struct {
	host extension.Host
}

// Register registers update_task_list and its lifecycle cleanup hook.
func Register(registry *extension.Registry, host extension.Host) error {
	if registry == nil || host == nil {
		return errors.New("register task list: registry and host are required")
	}
	t := &tracker{host: host}
	if err := registry.RegisterTool(t.tool()); err != nil {
		return fmt.Errorf("register %s: %w", ToolName, err)
	}
	registry.On("session_shutdown", Source, func(context.Context, map[string]any) (map[string]any, error) {
		host.SetStatus(statusKey, "")
		host.SetPanel(statusKey, "", nil)
		return nil, nil
	})
	return nil
}

func (t *tracker) tool() extension.Tool {
	return extension.Tool{
		Source: Source,
		Definition: model.ToolDefinition{
			Name:        ToolName,
			Description: "Create or update the task list for complex, multi-step work. Send the complete current list every time, keep stable IDs, and normally keep exactly one task in progress.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"todos": map[string]any{
						"type": "array", "maxItems": maxTasks,
						"description": "The complete current task list. Send the full list every time.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":       map[string]any{"type": "string", "description": "Stable task ID. Omit to derive one from content."},
								"content":  map[string]any{"type": "string", "minLength": 1, "description": "Short task description."},
								"status":   map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "cancelled"}},
								"priority": map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
							},
							"required": []string{"content", "status"}, "additionalProperties": false,
						},
					},
				},
				"required": []string{"todos"}, "additionalProperties": false,
			},
		},
		Execute: t.execute,
	}
}

func (t *tracker) execute(_ context.Context, raw json.RawMessage, _ func(string)) (extension.ToolResult, error) {
	args, err := decode(raw)
	if err != nil {
		return extension.ToolResult{}, err
	}
	tasks, err := normalize(args.Todos)
	if err != nil {
		return extension.ToolResult{}, err
	}
	summary := count(tasks)

	if summary.Pending > 0 || summary.InProgress > 0 {
		t.host.SetStatus(statusKey, fmt.Sprintf("tasks %d/%d", summary.Completed, summary.Total))
		t.host.SetPanel(statusKey, fmt.Sprintf("Tasks %d/%d", summary.Completed, summary.Total), panelLines(tasks))
	} else {
		t.host.SetStatus(statusKey, "")
		t.host.SetPanel(statusKey, "", nil)
	}
	return extension.ToolResult{
		Content: render(tasks),
		Details: map[string]any{
			"todos": tasks, "summary": summary,
			"updatedAt": time.Now().UTC().Format(time.RFC3339Nano),
		},
	}, nil
}

func decode(raw json.RawMessage) (input, error) {
	var args input
	if len(raw) == 0 {
		return args, errors.New("arguments are required")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return args, fmt.Errorf("decode arguments: %w", err)
	}
	if args.Todos == nil {
		return args, errors.New("todos is required")
	}
	if len(args.Todos) > maxTasks {
		return args, fmt.Errorf("todos must contain at most %d tasks", maxTasks)
	}
	return args, nil
}

func normalize(inputs []inputTask) ([]Task, error) {
	used := make(map[string]bool, len(inputs))
	tasks := make([]Task, len(inputs))
	for i, item := range inputs {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			return nil, fmt.Errorf("task %d content must not be empty", i+1)
		}
		if !validStatus(item.Status) {
			return nil, fmt.Errorf("task %d has invalid status %q", i+1, item.Status)
		}
		if item.Priority != "" && !validPriority(item.Priority) {
			return nil, fmt.Errorf("task %d has invalid priority %q", i+1, item.Priority)
		}
		base := strings.TrimSpace(item.ID)
		if base == "" {
			base = content
		}
		id := uniqueID(base, used, fmt.Sprintf("task-%d", i+1))
		tasks[i] = Task{ID: id, Content: content, Status: item.Status, Priority: item.Priority}
	}
	return tasks, nil
}

func uniqueID(value string, used map[string]bool, fallback string) string {
	id := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(value), "-"), "-")
	if len(id) > 32 {
		id = strings.TrimRight(id[:32], "-")
	}
	if id == "" {
		id = fallback
	}
	base := id
	for suffix := 2; used[id]; suffix++ {
		id = fmt.Sprintf("%s-%d", base, suffix)
	}
	used[id] = true
	return id
}

func validStatus(value string) bool {
	return value == "pending" || value == "in_progress" || value == "completed" || value == "cancelled"
}

func validPriority(value string) bool { return value == "high" || value == "medium" || value == "low" }

func count(tasks []Task) Summary {
	summary := Summary{Total: len(tasks)}
	for _, task := range tasks {
		switch task.Status {
		case "pending":
			summary.Pending++
		case "in_progress":
			summary.InProgress++
		case "completed":
			summary.Completed++
		case "cancelled":
			summary.Cancelled++
		}
	}
	return summary
}

func panelLines(tasks []Task) []string {
	const limit = 14
	count := len(tasks)
	if count > limit {
		count = limit
	}
	lines := make([]string, 0, count+1)
	for _, task := range tasks[:count] {
		icon := "○"
		switch task.Status {
		case "in_progress":
			icon = "●"
		case "completed":
			icon = "✓"
		case "cancelled":
			icon = "⊘"
		}
		priority := ""
		if task.Priority == "high" {
			priority = "! "
		} else if task.Priority == "medium" {
			priority = "• "
		}
		lines = append(lines, fmt.Sprintf("%s %s%s", icon, priority, task.Content))
	}
	if len(tasks) > count {
		lines = append(lines, fmt.Sprintf("… %d more", len(tasks)-count))
	}
	return lines
}

func render(tasks []Task) string {
	if len(tasks) == 0 {
		return "Task list cleared."
	}
	lines := make([]string, len(tasks))
	for i, task := range tasks {
		priority := ""
		if task.Priority != "" {
			priority = " " + task.Priority
		}
		lines[i] = fmt.Sprintf("- [%s] %s:%s %s", task.Status, task.ID, priority, task.Content)
	}
	return strings.Join(lines, "\n")
}
