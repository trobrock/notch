package luaext

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/trobrock/notch/internal/extension"
	"github.com/trobrock/notch/internal/model"
	lua "github.com/yuin/gopher-lua"
)

func installAPI(L *lua.LState, decls *declarations, host extension.Host) {
	notch := L.NewTable()

	L.SetField(notch, "register_tool", L.NewFunction(func(L *lua.LState) int {
		if !decls.loading {
			L.RaiseError("register_tool may only be called while an extension is loading")
		}
		t := L.CheckTable(1)
		name := requiredStringField(L, t, "name")
		description := optionalStringField(L, t, "description")
		schemaValue := L.GetField(t, "input_schema")
		if schemaValue == lua.LNil { // "schema" is accepted as a convenient alias.
			schemaValue = L.GetField(t, "schema")
		}
		var schema map[string]any
		if schemaValue == lua.LNil {
			schema = map[string]any{}
		} else {
			converted, err := luaToGo(schemaValue)
			if err != nil {
				L.RaiseError("invalid input_schema: %v", err)
			}
			var ok bool
			schema, ok = converted.(map[string]any)
			if !ok {
				L.RaiseError("input_schema must be a table with string keys")
			}
		}
		fn, ok := L.GetField(t, "execute").(*lua.LFunction)
		if !ok {
			L.RaiseError("tool %q requires an execute function", name)
		}
		decls.tools = append(decls.tools, toolDecl{
			definition: model.ToolDefinition{Name: name, Description: description, InputSchema: schema},
			fn:         fn,
		})
		return 0
	}))

	L.SetField(notch, "register_command", L.NewFunction(func(L *lua.LState) int {
		if !decls.loading {
			L.RaiseError("register_command may only be called while an extension is loading")
		}
		t := L.CheckTable(1)
		name := requiredStringField(L, t, "name")
		fn, ok := L.GetField(t, "execute").(*lua.LFunction)
		if !ok {
			L.RaiseError("command %q requires an execute function", name)
		}
		decls.commands = append(decls.commands, commandDecl{
			name: name, description: optionalStringField(L, t, "description"), fn: fn,
		})
		return 0
	}))

	L.SetField(notch, "on", L.NewFunction(func(L *lua.LState) int {
		if !decls.loading {
			L.RaiseError("on may only be called while an extension is loading")
		}
		event := L.CheckString(1)
		if event == "" {
			L.ArgError(1, "event must not be empty")
		}
		fn := L.CheckFunction(2)
		decls.hooks = append(decls.hooks, hookDecl{event: event, fn: fn})
		return 0
	}))

	L.SetField(notch, "follow_up", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.follow_up is unavailable: extension host is nil")
		}
		if err := host.FollowUp(L.CheckString(1)); err != nil {
			L.RaiseError("follow_up: %v", err)
		}
		return 0
	}))

	L.SetField(notch, "cwd", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.cwd is unavailable: extension host is nil")
		}
		L.Push(lua.LString(host.CWD()))
		return 1
	}))

	L.SetField(notch, "exec", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.exec is unavailable: extension host is nil")
		}
		command := L.CheckString(1)
		args := []string{}
		if L.GetTop() >= 2 && L.Get(2) != lua.LNil {
			t := L.CheckTable(2)
			for i := 1; i <= t.Len(); i++ {
				value := t.RawGetInt(i)
				s, ok := value.(lua.LString)
				if !ok {
					L.ArgError(2, fmt.Sprintf("argument %d must be a string", i))
				}
				args = append(args, string(s))
			}
		}
		stdout, stderr, exitCode, err := host.Exec(luaContext(L), command, args)
		if err != nil {
			L.RaiseError("exec %q: %v", command, err)
		}
		result := L.NewTable()
		L.SetField(result, "stdout", lua.LString(stdout))
		L.SetField(result, "stderr", lua.LString(stderr))
		L.SetField(result, "exit_code", lua.LNumber(exitCode))
		// code is retained as a concise alias useful to small Lua scripts.
		L.SetField(result, "code", lua.LNumber(exitCode))
		L.Push(result)
		return 1
	}))

	sessionAPI := L.NewTable()
	L.SetField(sessionAPI, "append", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.session.append is unavailable: extension host is nil")
		}
		kind := L.CheckString(1)
		if L.CheckAny(2) == lua.LNil {
			L.ArgError(2, "data must not be nil")
		}
		data, err := luaToGo(L.CheckAny(2))
		if err != nil {
			L.RaiseError("session append data: %v", err)
		}
		if err := host.AppendSessionEntry(kind, data); err != nil {
			L.RaiseError("session append: %v", err)
		}
		return 0
	}))
	L.SetField(sessionAPI, "entries", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.session.entries is unavailable: extension host is nil")
		}
		entries, err := host.SessionEntries(L.CheckString(1))
		if err != nil {
			L.RaiseError("session entries: %v", err)
		}
		result := L.NewTable()
		for i, raw := range entries {
			var value any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				L.RaiseError("session entry %d: %v", i+1, err)
			}
			converted, err := goToLua(L, value)
			if err != nil {
				L.RaiseError("session entry %d: %v", i+1, err)
			}
			result.Append(converted)
		}
		L.Push(result)
		return 1
	}))
	L.SetField(notch, "session", sessionAPI)

	ui := L.NewTable()
	L.SetField(ui, "input", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.ui.input is unavailable: extension host is nil")
		}
		prompt := L.CheckString(1)
		placeholder := L.OptString(2, "")
		value, err := host.Input(luaContext(L), prompt, placeholder)
		if err != nil {
			L.RaiseError("input: %v", err)
		}
		L.Push(lua.LString(value))
		return 1
	}))
	L.SetField(ui, "select", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.ui.select is unavailable: extension host is nil")
		}
		prompt := L.CheckString(1)
		t := L.CheckTable(2)
		options := make([]string, 0, t.Len())
		for i := 1; i <= t.Len(); i++ {
			v, ok := t.RawGetInt(i).(lua.LString)
			if !ok {
				L.ArgError(2, fmt.Sprintf("option %d must be a string", i))
			}
			options = append(options, string(v))
		}
		value, err := host.Select(luaContext(L), prompt, options)
		if err != nil {
			L.RaiseError("select: %v", err)
		}
		L.Push(lua.LString(value))
		return 1
	}))
	L.SetField(ui, "notify", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.ui.notify is unavailable: extension host is nil")
		}
		host.Notify(L.CheckString(1), L.OptString(2, "info"))
		return 0
	}))
	L.SetField(ui, "editor_text", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.ui.editor_text is unavailable: extension host is nil")
		}
		value, err := host.EditorText(luaContext(L))
		if err != nil {
			L.RaiseError("editor text: %v", err)
		}
		L.Push(lua.LString(value))
		return 1
	}))
	L.SetField(ui, "set_editor_text", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.ui.set_editor_text is unavailable: extension host is nil")
		}
		if err := host.SetEditorText(luaContext(L), L.CheckString(1)); err != nil {
			L.RaiseError("set editor text: %v", err)
		}
		return 0
	}))
	L.SetField(ui, "set_status", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.ui.set_status is unavailable: extension host is nil")
		}
		host.SetStatus(L.CheckString(1), L.OptString(2, ""))
		return 0
	}))
	L.SetField(ui, "set_panel", L.NewFunction(func(L *lua.LState) int {
		if host == nil {
			L.RaiseError("notch.ui.set_panel is unavailable: extension host is nil")
		}
		key, title := L.CheckString(1), L.OptString(2, "")
		lines := []string{}
		if L.GetTop() >= 3 && L.Get(3) != lua.LNil {
			t := L.CheckTable(3)
			for i := 1; i <= t.Len(); i++ {
				line, ok := t.RawGetInt(i).(lua.LString)
				if !ok {
					L.ArgError(3, fmt.Sprintf("line %d must be a string", i))
				}
				lines = append(lines, string(line))
			}
		}
		host.SetPanel(key, title, lines)
		return 0
	}))
	L.SetField(notch, "ui", ui)
	L.SetGlobal("notch", notch)
}

func luaContext(L *lua.LState) context.Context {
	if ctx := L.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func requiredStringField(L *lua.LState, table *lua.LTable, field string) string {
	value := L.GetField(table, field)
	s, ok := value.(lua.LString)
	if !ok || s == "" {
		L.RaiseError("field %q must be a non-empty string", field)
	}
	return string(s)
}

func optionalStringField(L *lua.LState, table *lua.LTable, field string) string {
	value := L.GetField(table, field)
	if value == lua.LNil {
		return ""
	}
	s, ok := value.(lua.LString)
	if !ok {
		L.RaiseError("field %q must be a string", field)
	}
	return string(s)
}

func toolResult(value lua.LValue) (extension.ToolResult, error) {
	if value == lua.LNil {
		return extension.ToolResult{}, nil
	}
	if s, ok := value.(lua.LString); ok {
		return extension.ToolResult{Content: string(s)}, nil
	}
	table, ok := value.(*lua.LTable)
	if !ok {
		return extension.ToolResult{}, fmt.Errorf("Lua tool returned %s, want string, table, or nil", value.Type())
	}
	result := extension.ToolResult{}
	if v := table.RawGetString("content"); v != lua.LNil {
		s, ok := v.(lua.LString)
		if !ok {
			return result, fmt.Errorf("Lua tool result content must be a string")
		}
		result.Content = string(s)
	}
	if v := table.RawGetString("is_error"); v != lua.LNil {
		b, ok := v.(lua.LBool)
		if !ok {
			return result, fmt.Errorf("Lua tool result is_error must be a boolean")
		}
		result.IsError = bool(b)
	}
	if v := table.RawGetString("details"); v != lua.LNil {
		converted, err := luaToGo(v)
		if err != nil {
			return result, fmt.Errorf("Lua tool result details: %w", err)
		}
		var ok bool
		result.Details, ok = converted.(map[string]any)
		if !ok {
			return result, fmt.Errorf("Lua tool result details must be a table with string keys")
		}
	}
	return result, nil
}

func luaToGo(value lua.LValue) (any, error) {
	switch value := value.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(value), nil
	case lua.LString:
		return string(value), nil
	case lua.LNumber:
		return float64(value), nil
	case *lua.LTable:
		isArray := value.Len() > 0
		if isArray {
			count := 0
			valid := true
			value.ForEach(func(k, _ lua.LValue) {
				count++
				n, ok := k.(lua.LNumber)
				if !ok || n < 1 || n != lua.LNumber(math.Trunc(float64(n))) || int(n) > value.Len() {
					valid = false
				}
			})
			isArray = valid && count == value.Len()
		}
		if isArray {
			out := make([]any, value.Len())
			for i := 1; i <= value.Len(); i++ {
				converted, err := luaToGo(value.RawGetInt(i))
				if err != nil {
					return nil, fmt.Errorf("index %d: %w", i, err)
				}
				out[i-1] = converted
			}
			return out, nil
		}
		out := make(map[string]any)
		var conversionErr error
		value.ForEach(func(k, v lua.LValue) {
			if conversionErr != nil {
				return
			}
			key, ok := k.(lua.LString)
			if !ok {
				conversionErr = fmt.Errorf("table key %s is not a string", k.String())
				return
			}
			converted, err := luaToGo(v)
			if err != nil {
				conversionErr = fmt.Errorf("field %q: %w", string(key), err)
				return
			}
			out[string(key)] = converted
		})
		return out, conversionErr
	default:
		return nil, fmt.Errorf("cannot convert Lua %s value", value.Type())
	}
}

func goToLua(L *lua.LState, value any) (lua.LValue, error) {
	switch value := value.(type) {
	case nil:
		return lua.LNil, nil
	case bool:
		return lua.LBool(value), nil
	case string:
		return lua.LString(value), nil
	case json.Number:
		n, err := value.Float64()
		if err != nil {
			return nil, err
		}
		return lua.LNumber(n), nil
	case float64:
		return lua.LNumber(value), nil
	case float32:
		return lua.LNumber(value), nil
	case int:
		return lua.LNumber(value), nil
	case int8:
		return lua.LNumber(value), nil
	case int16:
		return lua.LNumber(value), nil
	case int32:
		return lua.LNumber(value), nil
	case int64:
		return lua.LNumber(value), nil
	case uint:
		return lua.LNumber(value), nil
	case uint8:
		return lua.LNumber(value), nil
	case uint16:
		return lua.LNumber(value), nil
	case uint32:
		return lua.LNumber(value), nil
	case uint64:
		return lua.LNumber(value), nil
	case []any:
		t := L.NewTable()
		for _, item := range value {
			converted, err := goToLua(L, item)
			if err != nil {
				return nil, err
			}
			t.Append(converted)
		}
		return t, nil
	case map[string]any:
		t := L.NewTable()
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			converted, err := goToLua(L, value[key])
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", key, err)
			}
			t.RawSetString(key, converted)
		}
		return t, nil
	default:
		// Hook event maps may contain typed slices, maps, or structs. Normalize
		// any other JSON-compatible value before converting it to Lua.
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("cannot convert Go %T value: %w", value, err)
		}
		var normalized any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&normalized); err != nil {
			return nil, fmt.Errorf("cannot convert Go %T value: %w", value, err)
		}
		return goToLua(L, normalized)
	}
}
