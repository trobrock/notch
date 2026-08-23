package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/trobrock/notch/internal/model"
)

type ToolResult struct {
	Content string         `json:"content"`
	IsError bool           `json:"is_error,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

type ToolHandler func(ctx context.Context, args json.RawMessage, onUpdate func(string)) (ToolResult, error)

type Tool struct {
	Definition model.ToolDefinition
	Execute    ToolHandler
	Source     string
}

type HookHandler func(ctx context.Context, event map[string]any) (map[string]any, error)

type CommandHandler func(ctx context.Context, args string) (string, error)

type Command struct {
	Name        string
	Description string
	Execute     CommandHandler
	Source      string
}

// Host contains privileged operations available to extensions. Implementations may
// restrict these methods per extension in the future.
type Host interface {
	CWD() string
	Exec(ctx context.Context, command string, args []string) (stdout, stderr string, exitCode int, err error)
	Input(ctx context.Context, prompt, placeholder string) (string, error)
	Select(ctx context.Context, prompt string, options []string) (string, error)
	Notify(message, level string)
}

type namedHook struct {
	source  string
	handler HookHandler
}

type Registry struct {
	mu       sync.RWMutex
	tools    map[string]Tool
	commands map[string]Command
	hooks    map[string][]namedHook
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, commands: map[string]Command{}, hooks: map[string][]namedHook{}}
}

func (r *Registry) RegisterTool(tool Tool) error {
	if tool.Definition.Name == "" || tool.Execute == nil {
		return errors.New("tool requires a name and execute function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.tools[tool.Definition.Name]; ok {
		return fmt.Errorf("tool %q already registered by %s", tool.Definition.Name, old.Source)
	}
	r.tools[tool.Definition.Name] = tool
	return nil
}

func (r *Registry) RegisterCommand(command Command) error {
	if command.Name == "" || command.Execute == nil {
		return errors.New("command requires a name and execute function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.commands[command.Name]; ok {
		return fmt.Errorf("command %q already registered by %s", command.Name, old.Source)
	}
	r.commands[command.Name] = command
	return nil
}

func (r *Registry) On(event, source string, handler HookHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks[event] = append(r.hooks[event], namedHook{source: source, handler: handler})
}

func (r *Registry) Tool(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Tools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		out = append(out, tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Definition.Name < out[j].Definition.Name })
	return out
}

func (r *Registry) Definitions() []model.ToolDefinition {
	tools := r.Tools()
	out := make([]model.ToolDefinition, len(tools))
	for i := range tools {
		out[i] = tools[i].Definition
	}
	return out
}

func (r *Registry) Command(name string) (Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.commands[name]
	return c, ok
}

func (r *Registry) Commands() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Command, 0, len(r.commands))
	for _, command := range r.commands {
		out = append(out, command)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RunHooks passes one mutable event through handlers in registration order.
// Every handler sees the merged result of all earlier handlers.
func (r *Registry) RunHooks(ctx context.Context, name string, event map[string]any) (map[string]any, error) {
	r.mu.RLock()
	hooks := append([]namedHook(nil), r.hooks[name]...)
	r.mu.RUnlock()
	if event == nil {
		event = map[string]any{}
	}
	for _, hook := range hooks {
		result, err := hook.handler(ctx, event)
		if err != nil {
			return event, fmt.Errorf("%s hook from %s: %w", name, hook.source, err)
		}
		for key, value := range result {
			event[key] = value
		}
	}
	return event, nil
}
