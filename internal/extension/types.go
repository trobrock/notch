package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/trobrock/notch/internal/model"
)

const MaxToolResultBytes = 50 * 1024

// LimitToolResult bounds untrusted extension and MCP result text while keeping
// both the beginning and end, where summaries and terminal errors commonly live.
func LimitToolResult(result ToolResult) ToolResult {
	if len(result.Content) <= MaxToolResultBytes {
		return result
	}
	head := MaxToolResultBytes * 65 / 100
	tail := MaxToolResultBytes - head
	result.Content = result.Content[:validUTF8Prefix(result.Content, head)] +
		fmt.Sprintf("\n\n[tool output truncated: %d bytes total]\n\n", len(result.Content)) +
		result.Content[len(result.Content)-validUTF8Suffix(result.Content, tail):]
	return result
}

func validUTF8Prefix(value string, limit int) int {
	if limit >= len(value) {
		return len(value)
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return limit
}
func validUTF8Suffix(value string, limit int) int {
	if limit >= len(value) {
		return len(value)
	}
	for limit > 0 && !utf8.ValidString(value[len(value)-limit:]) {
		limit--
	}
	return limit
}

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
	// FollowUp delivers a synthetic user message when the active agent becomes
	// idle, or starts a new prompt when it is already idle.
	FollowUp(message string) error
	// Handoff delivers a synthetic user message after the active run. When
	// fresh is true, conversation context is durably reset first.
	Handoff(message string, fresh bool) error
	// SetActiveTools replaces the model-visible tool set. Nil restores all
	// registered tools; an empty non-nil slice disables every tool.
	SetActiveTools(names []string) error
	// SetStatus publishes a short keyed status for persistent UI display. An
	// empty value removes the status. Headless hosts may expose it as an event.
	SetStatus(key, value string)
	// SetPanel publishes a keyed, non-interactive panel. Empty title and lines
	// remove it. Hosts without a panel UI may ignore or expose it as an event.
	SetPanel(key, title string, lines []string)
}

type namedHook struct {
	source  string
	handler HookHandler
}

type Registry struct {
	mu          sync.RWMutex
	tools       map[string]Tool
	activeTools map[string]bool
	commands    map[string]Command
	hooks       map[string][]namedHook
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

// RestrictTools removes every tool not named in names. It returns requested
// names which were not registered. An empty list disables all tools.
func (r *Registry) RestrictTools(names []string) []string {
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.tools {
		if !allowed[name] {
			delete(r.tools, name)
		}
	}
	var missing []string
	for name := range allowed {
		if _, ok := r.tools[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// RemoveTools disables named tools and returns names which were not registered.
func (r *Registry) RemoveTools(names []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var missing []string
	for _, name := range names {
		if _, ok := r.tools[name]; !ok {
			missing = append(missing, name)
			continue
		}
		delete(r.tools, name)
	}
	sort.Strings(missing)
	return missing
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

func (r *Registry) SetActiveTools(names []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if names == nil {
		r.activeTools = nil
		return nil
	}
	active := make(map[string]bool, len(names))
	var missing []string
	for _, name := range names {
		if _, ok := r.tools[name]; !ok {
			missing = append(missing, name)
			continue
		}
		active[name] = true
	}
	r.activeTools = active
	sort.Strings(missing)
	return missing
}

func (r *Registry) ActiveToolNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.activeTools == nil {
		return nil
	}
	names := make([]string, 0, len(r.activeTools))
	for name := range r.activeTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) Tool(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if ok && r.activeTools != nil && !r.activeTools[name] {
		return Tool{}, false
	}
	return t, ok
}

func (r *Registry) Tools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for name, tool := range r.tools {
		if r.activeTools != nil && !r.activeTools[name] {
			continue
		}
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

// RunHooksBestEffort invokes every registered handler even when an earlier
// handler fails. It is intended for shutdown paths where one broken extension
// must not prevent the remaining extensions from cleaning up.
func (r *Registry) RunHooksBestEffort(ctx context.Context, name string, event map[string]any) (map[string]any, error) {
	r.mu.RLock()
	hooks := append([]namedHook(nil), r.hooks[name]...)
	r.mu.RUnlock()
	if event == nil {
		event = map[string]any{}
	}
	var hookErrors []error
	for _, hook := range hooks {
		result, err := hook.handler(ctx, event)
		if err != nil {
			hookErrors = append(hookErrors, fmt.Errorf("%s hook from %s: %w", name, hook.source, err))
			continue
		}
		for key, value := range result {
			event[key] = value
		}
	}
	return event, errors.Join(hookErrors...)
}
