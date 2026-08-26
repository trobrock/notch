// Package luaext loads Lua extensions and bridges them to the extension registry.
package luaext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	lua "github.com/yuin/gopher-lua"
)

// Manager owns the Lua states created for extension files. A Manager must be
// closed when it is no longer needed.
type Manager struct {
	mu       sync.Mutex
	registry *extension.Registry
	host     extension.Host
	states   []*luaState
	closed   bool
}

type luaState struct {
	mu     sync.Mutex
	L      *lua.LState
	source string
	lease  *extension.Registration
	closed bool
}

type toolDecl struct {
	definition model.ToolDefinition
	fn         *lua.LFunction
}

type commandDecl struct {
	name, description string
	fn                *lua.LFunction
}

type hookDecl struct {
	event string
	fn    *lua.LFunction
}

type declarations struct {
	tools    []toolDecl
	commands []commandDecl
	hooks    []hookDecl
	loading  bool
}

// NewManager creates a Lua extension manager using registry and host.
func NewManager(registry *extension.Registry, host extension.Host) *Manager {
	return &Manager{registry: registry, host: host}
}

// New is an alias for NewManager.
func New(registry *extension.Registry, host extension.Host) *Manager {
	return NewManager(registry, host)
}

// LoadDirs loads all files with a .lua suffix in dirs. Directories are
// processed in the supplied order and files within each directory are sorted
// by name. A file is run before the next file is loaded.
func (m *Manager) LoadDirs(dirs ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("lua extension manager is closed")
	}
	if m.registry == nil {
		return errors.New("lua extension manager has no registry")
	}

	start := len(m.states)
	rollback := func(err error) error {
		m.closeStatesLocked(start)
		return err
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			// Configured discovery roots are optional and commonly absent when a
			// user or project has no Lua extensions.
			continue
		}
		if err != nil {
			return rollback(fmt.Errorf("read Lua extension directory %q: %w", dir, err))
		}
		// os.ReadDir is documented to sort, but sorting here makes that part of
		// this package's behavior independently of the filesystem helper.
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".lua" {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			state, decls, err := m.loadFile(path)
			if err != nil {
				return rollback(err)
			}
			if err := m.commit(state, decls); err != nil {
				state.L.Close()
				state.closed = true
				return rollback(fmt.Errorf("load Lua extension %q: %w", path, err))
			}
			m.states = append(m.states, state)
		}
	}
	return nil
}

func (m *Manager) loadFile(path string) (*luaState, *declarations, error) {
	L := lua.NewState()
	state := &luaState{L: L, source: path}
	decls := &declarations{loading: true}
	installAPI(L, decls, m.host)
	if err := L.DoFile(path); err != nil {
		L.Close()
		state.closed = true
		return nil, nil, fmt.Errorf("load Lua extension %q: %w", path, err)
	}
	decls.loading = false
	return state, decls, nil
}

func (m *Manager) commit(state *luaState, decls *declarations) error {
	tools := make([]extension.Tool, 0, len(decls.tools))
	commands := make([]extension.Command, 0, len(decls.commands))
	hooks := make([]extension.HookRegistration, 0, len(decls.hooks))
	for _, d := range decls.tools {
		d := d
		tools = append(tools, extension.Tool{
			Definition: d.definition,
			Source:     state.source,
			Execute: func(ctx context.Context, args json.RawMessage, onUpdate func(string)) (extension.ToolResult, error) {
				result, err := state.callTool(ctx, d.fn, args, onUpdate)
				if err != nil {
					return extension.ToolResult{}, fmt.Errorf("Lua tool %q from %s: %w", d.definition.Name, state.source, err)
				}
				return result, nil
			},
		})
	}
	for _, d := range decls.commands {
		d := d
		commands = append(commands, extension.Command{
			Name: d.name, Description: d.description, Source: state.source,
			Execute: func(ctx context.Context, args string) (string, error) {
				value, err := state.call(ctx, d.fn, lua.LString(args))
				if err != nil {
					return "", fmt.Errorf("Lua command %q: %w", d.name, err)
				}
				if value == lua.LNil {
					return "", nil
				}
				if s, ok := value.(lua.LString); ok {
					return string(s), nil
				}
				return "", fmt.Errorf("Lua command %q returned %s, want string or nil", d.name, value.Type())
			},
		})
	}
	for _, d := range decls.hooks {
		d := d
		hooks = append(hooks, extension.HookRegistration{Event: d.event, Source: state.source, Handler: func(ctx context.Context, event map[string]any) (map[string]any, error) {
			return state.callHook(ctx, d.event, d.fn, event)
		}})
	}
	lease, err := m.registry.RegisterBatch(extension.Batch{Tools: tools, Commands: commands, Hooks: hooks})
	if err != nil {
		return err
	}
	state.lease = lease
	return nil
}

func (s *luaState) callTool(ctx context.Context, fn *lua.LFunction, raw json.RawMessage, onUpdate func(string)) (extension.ToolResult, error) {
	var args any = map[string]any{}
	if len(raw) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&args); err != nil {
			return extension.ToolResult{}, fmt.Errorf("decode arguments for Lua tool: %w", err)
		}
	}
	// Conversion is done while holding the state lock because creating tables
	// mutates the LState.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return extension.ToolResult{}, errors.New("Lua extension is closed")
	}
	lv, err := goToLua(s.L, args)
	if err != nil {
		return extension.ToolResult{}, fmt.Errorf("encode arguments for Lua tool: %w", err)
	}
	update := s.L.NewFunction(func(L *lua.LState) int {
		if onUpdate != nil {
			onUpdate(L.CheckString(1))
		}
		return 0
	})
	value, err := s.callLocked(ctx, fn, lv, update)
	if err != nil {
		return extension.ToolResult{}, fmt.Errorf("Lua tool: %w", err)
	}
	return toolResult(value)
}

func (s *luaState) call(ctx context.Context, fn *lua.LFunction, args ...lua.LValue) (lua.LValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("Lua extension is closed")
	}
	return s.callLocked(ctx, fn, args...)
}

func (s *luaState) callHook(ctx context.Context, event string, fn *lua.LFunction, input map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("Lua extension is closed")
	}
	lv, err := goToLua(s.L, input)
	if err != nil {
		return nil, fmt.Errorf("encode event for Lua hook %q: %w", event, err)
	}
	value, err := s.callLocked(ctx, fn, lv)
	if err != nil {
		return nil, err
	}
	if value == lua.LNil {
		return nil, nil
	}
	converted, err := luaToGo(value)
	if err != nil {
		return nil, fmt.Errorf("decode result from Lua hook %q: %w", event, err)
	}
	result, ok := converted.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Lua hook %q returned %T, want table or nil", event, converted)
	}
	return result, nil
}

func (s *luaState) callLocked(ctx context.Context, fn *lua.LFunction, args ...lua.LValue) (lua.LValue, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base := s.L.GetTop()
	defer s.L.SetTop(base)
	s.L.SetContext(ctx)
	defer s.L.RemoveContext()
	if err := s.L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, args...); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return s.L.Get(-1), nil
}

// closeStatesLocked unregisters and closes states from start onward. m.mu must
// be held. Leases are closed before state locks so registry lookups can no
// longer start new calls while Close waits for in-flight calls to finish.
func (m *Manager) closeStatesLocked(start int) {
	states := append([]*luaState(nil), m.states[start:]...)
	m.states = m.states[:start]
	for i := len(states) - 1; i >= 0; i-- {
		state := states[i]
		_ = state.lease.Close()
		state.lease = nil
		state.mu.Lock()
		if !state.closed {
			state.closed = true
			state.L.Close()
		}
		state.mu.Unlock()
	}
}

// Close closes all Lua states and unregisters their handlers. Close is safe to
// call more than once.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	m.closeStatesLocked(0)
	return nil
}
