---
name: notch-extension
description: Build, test, install, and debug Notch Lua extensions and executable JSON-RPC plugins. Use whenever the user asks to add a Notch tool, slash command, hook, or extension.
---
# Build Notch extensions

Use this skill for model-callable tools, slash commands, and lifecycle hooks. Keep optional workflow policy outside Notch core when the extension APIs are sufficient.

## Choose the smallest extension type

- Use **Lua** for concise tools, commands, and hooks with no extra runtime or complex concurrency.
- Use an **executable plugin** for substantial state, concurrency, streaming subprocesses, external libraries, or implementation in Go/Python/Rust/another language.
- Use **MCP** when a suitable server already exists or the tool should be shared with other agents.
- Change Notch core only for a generic mechanism that extensions cannot express cleanly.

Extensions are trusted and unsandboxed. They run with the user's privileges. Keep tool schemas narrow, validate every argument, honor cancellation, cap output, and avoid shell interpolation.

## Locations and discovery

Default extension directories are:

```text
~/.notch/extensions
<cwd>/.notch/extensions
```

`$NOTCH_HOME/extensions` replaces the user path. `extension_dirs` may replace the complete list. Lua discovery reads direct `.lua` files. Executable plugin discovery recursively finds `plugin.json` manifests. Restart Notch after adding or changing an extension.

Tool and command names are global. Duplicate names are rejected rather than overriding built-ins or earlier extensions. Use a clear, stable prefix for project-specific tools.

## Package and share extensions

Use `notch extensions init --name NAME DIRECTORY` to scaffold `notch-package.json` and an exported `extensions/` directory. A package manifest has `schema_version: 1`, a stable lowercase `name`, a semantic `version`, and one or more relative `extensions` directories. Keep package content ready to run; installation deliberately runs no build or post-install scripts.

Install and manage trusted packages with:

```text
notch extensions validate [--json] [DIRECTORY]
notch extensions install [--ref REF] [--subdir PATH] SOURCE
notch extensions list [--json]
notch extensions update [--force] [NAME...]
notch extensions remove NAME
```

Sources may be `github:owner/repository`, a raw GitHub URL, a generic HTTPS/SSH Git URL, or a local directory. GitHub and local sources are handled natively; generic Git invokes `git`. Installed packages are copied under `$NOTCH_HOME/packages` or `~/.notch/packages`, locked to an exact commit or tree digest, and loaded after direct extension directories. Never install a third-party package without reviewing it. See `docs/extension-packages.md` in the Notch repository for the full format and security rules.

## Lua extension API

Create a direct file such as `.notch/extensions/example.lua`:

```lua
notch.register_tool({
  name = "word_count",
  description = "Count whitespace-separated words",
  input_schema = {
    type = "object",
    properties = {
      text = { type = "string", description = "Text to count" },
    },
    required = { "text" },
    additionalProperties = false,
  },
  execute = function(args, update)
    update("counting")
    local _, count = string.gsub(args.text, "%S+", "")
    return {
      content = tostring(count),
      details = { unit = "words" },
    }
  end,
})

notch.register_command({
  name = "hello",
  description = "Print a greeting",
  execute = function(argument_string)
    return "hello " .. argument_string
  end,
})

notch.on("before_agent_start", function(event)
  return { system_prompt = event.system_prompt .. "\nBe concise." }
end)
```

Registration is valid only while the Lua file initially loads. Tool `args` are JSON converted to Lua values. `update(message)` emits tool progress. A tool may return a string, `nil`, or:

```lua
{ content = "text", is_error = false, details = { key = "value" } }
```

Host operations are:

```lua
local cwd = notch.cwd()
local run = notch.exec("git", {"status", "--short"})
-- run.stdout, run.stderr, run.exit_code; run.code aliases exit_code

local value = notch.ui.input("Name", "default")
local choice = notch.ui.select("Choose", {"one", "two"})
local draft = notch.ui.editor_text()
notch.ui.set_editor_text(draft .. "\nnext prompt")
notch.ui.notify("Finished", "success")
notch.ui.set_status("tasks", "tasks 1/3")
notch.ui.set_panel("tasks", "Tasks", {"● Implement", "○ Test"})

notch.session.append("example-state", {action = "add"})
local entries = notch.session.entries("example-state")
```

Session entries store extension-owned JSON data in the active durable session. Use a stable package-specific kind; calls fail when sessions are disabled. Editor access is fullscreen-only and should be used by interactive commands, not model tools or hooks.

Statuses are keyed footer values. Panels are keyed, non-interactive content above the composer. Replace either by reusing its key; clear a status with an empty value and a panel with an empty title and lines.

`notch.exec` executes an argv array in Notch's startup working directory; it does not invoke a shell. Non-zero exit currently raises a Lua error. UI input/select integrates with both the fullscreen modal UI and line fallback.

## Hooks

Supported emitted hooks are:

- `session_start`, `session_change`, `session_shutdown`, `agent_start`, `agent_error`, `before_agent_start`, `context`, `tool_call`, `tool_execution_start`, `tool_execution_end`, `agent_end`, and `session_before_compact`: lifecycle and request hooks.
- `session_change`: runs asynchronously in fullscreen mode after `/new` or `/resume` installs another session.
- `session_shutdown`: runs once before extensions close with the session-start fields plus `reason` (`exit` or `canceled`); all handlers are attempted with a fresh bounded context.
- `before_agent_start`: receives `system_prompt`, `model`, and `turn`; returning `system_prompt` replaces it for that turn.
- `tool_call`: receives `name`, `id`, and decoded `arguments`; return `denied=true` plus `reason`, or replacement `arguments`.
- `tool_execution_start`: receives `name` and `id`; return fields are ignored, while errors abort execution.
- `tool_execution_end`: receives `name`, `id`, `content`, and `is_error`; return fields are ignored, while errors replace the result.
- `agent_end`: receives `stop_reason` and `turn`; return non-empty `follow_up` to append a synthetic user message and continue.

Do not register invented hook names unless core emits them; registration alone does not create an event.

## Executable plugin

Create a directory:

```text
.notch/extensions/example-plugin/
  plugin.json
  plugin-program
```

Manifest:

```json
{
  "name": "example-plugin",
  "command": ["./plugin-program"],
  "enabled": true
}
```

The process uses newline-delimited JSON-RPC 2.0 over stdin/stdout. Stdout is protocol-only; send diagnostics to stderr. It must answer `initialize` with tool/command/hook declarations, then handle `tool.execute`, `command.execute`, and `hook.handle`. It may send `tool.update` notifications while a tool request is active. Host calls include `host.cwd`, `host.exec`, `host.ui.input`, `host.ui.select`, `host.ui.notify`, `host.ui.editor_text`, `host.ui.set_editor_text`, `host.session.append`, `host.session.entries`, `host.ui.set_status`, and `host.ui.set_panel`. Honor `$/cancelRequest` and terminate child work when its request context is canceled.

### Go SDK

A Go plugin can use `github.com/trobrock/notch/plugin`:

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "os"

    "github.com/trobrock/notch/plugin"
)

func main() {
    ext := plugin.Extension{Tools: []plugin.Tool{{
        Name: "example_echo",
        Description: "Echo validated text",
        InputSchema: map[string]any{
            "type": "object",
            "properties": map[string]any{"text": map[string]any{"type": "string"}},
            "required": []string{"text"},
            "additionalProperties": false,
        },
        Execute: func(ctx context.Context, raw json.RawMessage, progress plugin.Progress) (plugin.ToolResult, error) {
            var args struct { Text string `json:"text"` }
            if err := json.Unmarshal(raw, &args); err != nil { return plugin.ToolResult{}, err }
            progress("echoing")
            return plugin.ToolResult{Content: args.Text}, nil
        },
    }}}
    if err := plugin.Serve(context.Background(), ext); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

Build it into its manifest directory and use a relative command. The plugin SDK also exposes commands, hooks, host clients, progress, and cancellation through the request context.

## Implementation checklist

1. Inspect existing tools and commands with `/tools` and `/help` to avoid collisions.
2. Choose Lua unless requirements justify a process boundary.
3. Define a strict JSON Schema with descriptions, required fields, and `additionalProperties=false` where appropriate.
4. Validate paths, limits, enum values, and empty inputs again in the handler.
5. Use argv execution, not concatenated shell strings. If a shell is truly required, quote defensively and document why.
6. Emit short progress updates, not full unbounded logs.
7. Return useful error content without leaking secrets.
8. Test success, invalid input, cancellation, non-zero subprocess exit, and duplicate registration.
9. Restart Notch and verify discovery with `/tools` or `/help`.
10. Keep generated binaries, credentials, and local-only configuration out of version control unless explicitly intended.

When working inside the Notch source repository, consult `docs/extensions.md`, `plugin/plugin.go`, and `examples/extensions/hello-plugin` for the exact current API before changing protocol-level behavior.
