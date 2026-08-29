# Extensions

Installed Notch binaries include `/skill:notch-extension`, an on-demand guide for choosing, building, validating, packaging, and installing the extension formats described here. `/skill:notch-config` covers extension directory and related runtime configuration. To share or install versioned extensions, see [extension packages](extension-packages.md).

Notch has two first-class extension formats:

- **Lua files** run inside the Notch process using embedded GopherLua.
- **Executable plugins** run as child processes and speak newline-delimited JSON-RPC 2.0 over stdin/stdout.

Both register the same three concepts: model-callable tools, interactive slash commands, and agent hooks. Executable plugins are the portable option for Go, Python, Rust, shell, or any other language; they do not imply a Node/npm runtime.

Extensions are trusted. Neither format is sandboxed, and host operations execute with the Notch user's privileges. There are no per-command approvals: after an extension is loaded, its operations and model-requested tools execute automatically. Project extensions are loaded only for a trusted workspace.

## Discovery and ordering

Default extension directories are:

```text
~/.config/notch/extensions
<workspace-root>/.notch/extensions  # trusted workspaces only
```

`XDG_CONFIG_HOME` relocates the first path and must be absolute when set. `extension_dirs` can replace the list in config. Project `.notch`/`.agents` inputs require one-time persisted repository trust, shared across linked worktrees through Git's common directory, and are skipped by untrusted noninteractive and `--safe` runs; `--trust-workspace` is the explicit automation opt-in. Directories exported by packages installed through `notch extensions install` are appended after the configured direct directories.

Notch recursively discovers files named `plugin.json` for executable plugins. It also loads `.lua` files located directly in each configured extension directory; Lua discovery is not recursive. Manifest paths and Lua filenames are sorted. User directories precede project directories by default.

Tool and command names are global. A duplicate is rejected rather than overriding a built-in or an earlier extension. Lua and executable plugins register each extension's complete tool/command/hook set atomically, and closing an extension unregisters that batch. Executable plugin failures are reported independently. A Lua load failure rolls back every Lua file loaded by that `LoadDirs` call, so no partial Lua registrations remain.

## Lua API

Create `.notch/extensions/example.lua`:

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
  execute = function(args)
    return "hello " .. args
  end,
})

notch.on("before_agent_start", function(event)
  return { system_prompt = event.system_prompt .. "\nBe concise." }
end)
```

Restart Notch, inspect it with `/tools` or `/help`, and invoke `/hello Ada`. Registered tools are offered to the model automatically.

### Registration functions

Registration is only valid while the Lua file is initially loading.

```lua
notch.register_tool({
  name = "required-name",
  description = "optional",
  input_schema = { type = "object" }, -- `schema` is also accepted
  execute = function(args, update) ... end,
})
```

`args` is converted from JSON into Lua values. `update(message)` emits a `tool_update` event. A tool may return:

- a string, used as `content`;
- `nil`, an empty successful result; or
- `{ content = string, is_error = boolean, details = table }` (all fields optional).

```lua
notch.register_command({
  name = "required-name",
  description = "optional",
  execute = function(argument_string)
    return "output" -- string or nil
  end,
})

notch.on("event_name", function(event_table)
  return { changed = "value" } -- table or nil
end)
```

Hooks receive a table and return fields to merge into that event before the next hook. JSON-compatible Lua values are supported. Each file has its own Lua state, and calls into one state are serialized.

### Host API

```lua
local cwd = notch.cwd()
local run = notch.exec("git", {"status", "--short"})
-- run.stdout, run.stderr, run.exit_code; run.code aliases exit_code

local value = notch.ui.input("Name", "default")
local choice = notch.ui.select("Choose", {"one", "two"})
local draft = notch.ui.editor_text()
notch.ui.set_editor_text(draft .. "\nnext prompt")
notch.ui.notify("Finished", "success")
notch.follow_up("Continue with the result")
notch.ui.set_status("tasks", "tasks 1/3") -- empty value clears it
notch.ui.set_panel("tasks", "Tasks", {"● Implement", "○ Test"}) -- empty title/lines clears it

notch.session.append("example-state", {action = "add", value = "durable"})
local entries = notch.session.entries("example-state")
```

`notch.session.append` stores extension-owned JSON data in the current append-only session; `notch.session.entries` returns matching records from the current logical conversation (records before the latest `/new` reset are omitted). Use a stable, package-specific kind to avoid accidental sharing; core record kinds are reserved. These calls fail when session persistence is disabled. Fullscreen `/resume` switches subsequent calls to the resumed session and emits `session_change`; fullscreen `/new` switches to a fresh session and emits the same hook.

`notch.ui.editor_text` and `notch.ui.set_editor_text` access the fullscreen prompt composer. They fail in line/RPC modes and while another extension prompt is active. Use them from interactive commands rather than agent hooks or model tools.

`notch.session.append` / `entries` are available to Lua and executable plugins in every run mode when session persistence is enabled. Editor access is fullscreen-only.

`notch.exec` executes an argv array in Notch's working directory; it does not invoke a shell. It honors cancellation, terminates the child process group where supported, and retains at most 1 MiB from each of stdout and stderr while continuing to drain the process. A failed start or non-zero exit raises a Lua error with the current host implementation. UI levels are display labels rather than a fixed enum. `notch.follow_up` asks the host to deliver a synthetic user follow-up. Keyed statuses persist in the fullscreen footer until replaced or cleared. Keyed panels provide bounded, non-interactive content above the composer; publishing an empty title and lines clears one. Line mode ignores statuses and panels, while RPC mode emits corresponding events. Lua execution observes agent cancellation, including tight-running Lua code.

Extension prompts are rendered as dedicated question blocks rather than ordinary notices. Selection prompts show a bold question, a highlighted current option, descriptions on separate muted lines, keyboard hints, and a filter only after typing. Up/Down changes selection, Enter confirms, typing filters labels and descriptions, Backspace/Ctrl-U edits the filter, and Escape/Ctrl-C cancels. Free-form prompts clearly show their placeholder and submit/newline/cancel controls.

## Hooks

The current agent emits these hook names and fields:

- `agent_start`: emitted immediately before each provider request with `model` and `turn`.
- `agent_error`: emitted when a provider request fails with `message`, `model`, and `turn`.
- `context`: receives the trimmed model-request `messages` and `turn`; returning `messages` replaces that request context without changing durable history.
- `session_before_compact`: receives `auto`, `instructions`, usage, old messages, and recent messages. It may replace `instructions` or return a non-empty `summary` to supply the compaction summary directly.

### `session_start`

Runs once after the initial session and agent are ready, before any interactive or one-shot work starts:

```json
{"cwd":"/work","provider":"anthropic","model":"claude-sonnet-4-5","thinking_level":"medium","mode":"tui","resumed":false,"session_id":"...","session_file":"..."}
```

`mode` is `tui`, `line`, `print`, `json`, or `rpc`. The session fields are omitted with `--no-session`. Return fields are ignored; an error aborts startup.

### `session_change`

Runs asynchronously in fullscreen mode after `/new` installs a fresh session or `/resume` installs another saved session. It receives the new `session_id` and `session_file` when persistence is enabled. This hook is intended for extension UI/state resynchronization; startup still uses `session_start`. Because it is asynchronous, commands may run before a slow hook finishes.

### `session_shutdown`

Runs once before extensions are closed on normal exit, cancellation, and handled termination:

```json
{"cwd":"/work","provider":"anthropic","model":"claude-sonnet-4-5","thinking_level":"medium","mode":"tui","resumed":false,"session_id":"...","session_file":"...","reason":"exit"}
```

`reason` is `exit` or `canceled`. Shutdown gets a fresh bounded context even when the main run context was canceled. Every shutdown handler is attempted if another handler fails; errors are reported as warnings. `SIGKILL` and process crashes cannot run cleanup hooks.

### `before_agent_start`

Runs before every model turn.

```json
{"system_prompt":"...","model":"model-id","turn":0}
```

Returning `system_prompt` as a string replaces the prompt for that turn.

### `tool_call`

Runs immediately before looking up or executing a model tool call.

```json
{"name":"read","id":"call-id","arguments":{"path":"README.md"}}
```

Return `{"denied":true,"reason":"..."}` to produce an error tool result without execution. Return an `arguments` value to replace the call arguments. If multiple hooks modify fields, later hooks see prior changes.

### `tool_execution_start`

Runs after the tool is found and immediately before its handler:

```json
{"name":"read","id":"call-id"}
```

Return fields are currently ignored, but an error aborts normal handling for that call.

### `tool_execution_end`

Runs after execution:

```json
{"name":"read","id":"call-id","content":"...","is_error":false}
```

Return fields are currently ignored. A hook error replaces the tool result with an error.

### `monitor_start` and `monitor_end`

The official background monitor emits `monitor_start` after a process starts and
`monitor_end` when it exits or is cancelled. Both hooks receive the changed
monitor plus a snapshot of monitors that are still running:

```json
{
  "monitor": {
    "id": "mon-1",
    "name": "tests",
    "command": "make test",
    "trigger": "exit",
    "status": "running",
    "started_at": "2026-05-20T12:00:00Z"
  },
  "active": true,
  "monitors": [{"id":"mon-1","name":"tests","command":"make test","trigger":"exit","status":"running","started_at":"2026-05-20T12:00:00Z"}]
}
```

On `monitor_end`, `monitor.status` is `completed`, `failed`, or `cancelled` and
also includes `completed_at` and `exit_code`. `active` and `monitors` describe
the remaining running monitors, so extensions can publish external activity
state without maintaining their own monitor registry. These observational hooks
are best-effort: failures are reported as warnings and do not change monitor
behavior.

### `agent_end`

Runs when a model response has no tool calls:

```json
{"stop_reason":"end_turn","turn":0}
```

Return a non-empty string field `follow_up` to append it as a synthetic user message and continue the loop.

Other hook names may be registered, but they do nothing unless core code or another registry caller emits them. Lifecycle hooks describe the Notch process's initial session; fullscreen `/new` changes the conversation session without restarting the process lifecycle.

## Executable plugin manifest

A plugin is a directory containing `plugin.json` and its program. For example:

```text
.notch/extensions/example-plugin/
  plugin.json
  plugin.py
```

```json
{
  "name": "example-python",
  "command": ["python3", "plugin.py"],
  "enabled": true
}
```

`name` and a non-empty `command` array are required. **`enabled` must be explicitly `true`**; unlike MCP server config, omission disables the plugin. The child working directory is the manifest directory. It receives only a minimal inherited environment (`PATH`, home/user, temporary-directory, locale, terminal, and SSH-agent basics, plus required Windows process variables), intentionally excluding provider credentials, token variables, and typical CI secrets; plugin manifests do not add environment overrides. stderr is passed through to Notch's stderr, while stdout is reserved exclusively for protocol messages.

## Executable plugin protocol

Messages are one complete JSON object per line, encoded as JSON-RPC 2.0. Notch permits lines up to 16 MiB. Requests can be concurrent and responses may arrive out of order, so plugins must preserve IDs. Do not print logs to stdout.

### Initialization

Immediately after starting the process, Notch sends:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
```

The plugin responds with all declarations:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "tools": [{
      "name": "echo",
      "description": "Echo text",
      "input_schema": {
        "type": "object",
        "properties": {"text": {"type": "string"}},
        "required": ["text"]
      }
    }],
    "commands": [{"name": "say", "description": "Print text"}],
    "hooks": ["before_agent_start"]
  }
}
```

A hook declaration may also be `{"name":"before_agent_start"}` or `{"event":"before_agent_start"}`. Empty arrays may be omitted or returned as empty.

### Calls from Notch

Tool execution:

```json
{"jsonrpc":"2.0","id":2,"method":"tool.execute","params":{"name":"echo","args":{"text":"hi"}}}
```

Return either a string or a tool-result object:

```json
{"jsonrpc":"2.0","id":2,"result":{"content":"hi","is_error":false,"details":{"length":2}}}
```

While that call is pending, the plugin may send updates as notifications. Associate the update with the **JSON-RPC request ID**:

```json
{"jsonrpc":"2.0","method":"tool.update","params":{"id":2,"message":"working"}}
```

`request_id`/`requestId` and `content`/`update` aliases are also accepted.

Command execution:

```json
{"jsonrpc":"2.0","id":3,"method":"command.execute","params":{"name":"say","args":"hello world"}}
{"jsonrpc":"2.0","id":3,"result":"hello world"}
```

A command may instead return `{"output":"..."}` or `{"content":"..."}`.

Hook handling:

```json
{"jsonrpc":"2.0","id":4,"method":"hook.handle","params":{"name":"before_agent_start","event":{"system_prompt":"...","model":"...","turn":0}}}
{"jsonrpc":"2.0","id":4,"result":{"system_prompt":"... amended ..."}}
```

Hook results must be JSON objects (use `{}` for no change).

Use ordinary JSON-RPC errors when a call fails:

```json
{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"failed"}}
```

### Cancellation

If the context for an outstanding plugin request is canceled, Notch sends a notification and stops waiting:

```json
{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":2}}
```

The plugin should stop work if possible. A late response for that canceled ID is permitted and ignored.

### Calls from a plugin to Notch

A plugin may issue JSON-RPC requests to the host at any time and must handle the matching response.

Get the working directory:

```json
{"jsonrpc":"2.0","id":"host-1","method":"host.cwd","params":{}}
```

Run a program without a shell. Host execution is cancellation-aware, terminates the child process group where supported, and retains at most 1 MiB each of stdout and stderr while continuing to drain larger output:

```json
{"jsonrpc":"2.0","id":"host-2","method":"host.exec","params":{"command":"git","args":["status","--short"]}}
```

Successful result:

```json
{"jsonrpc":"2.0","id":"host-2","result":{"stdout":"","stderr":"","exit_code":0}}
```

With the current terminal host, a non-zero process exit is returned as a host JSON-RPC error rather than as a normal result.

Terminal interactions:

```json
{"jsonrpc":"2.0","id":"host-3","method":"host.ui.input","params":{"prompt":"Name","placeholder":"Ada"}}
{"jsonrpc":"2.0","id":"host-4","method":"host.ui.select","params":{"prompt":"Pick","options":["a","b"]}}
{"jsonrpc":"2.0","id":"host-5","method":"host.ui.notify","params":{"message":"Done","level":"info"}}
{"jsonrpc":"2.0","id":"host-6","method":"host.ui.editor_text","params":{}}
{"jsonrpc":"2.0","id":"host-7","method":"host.ui.set_editor_text","params":{"text":"next prompt"}}
{"jsonrpc":"2.0","id":"host-8","method":"host.session.append","params":{"kind":"example-state","data":{"action":"add"}}}
{"jsonrpc":"2.0","id":"host-9","method":"host.session.entries","params":{"kind":"example-state"}}
{"jsonrpc":"2.0","id":"host-10","method":"host.ui.set_status","params":{"key":"tasks","value":"tasks 1/3"}}
{"jsonrpc":"2.0","id":"host-11","method":"host.ui.set_panel","params":{"key":"tasks","title":"Tasks","lines":["● Implement","○ Test"]}}
```

Input, selection, editor-text, and session-entries return values; mutation and publication calls return `null`. Set-status replaces a persistent keyed footer value and an empty value clears it. Set-panel replaces bounded non-interactive content above the composer; empty title and lines clear it. In the fullscreen TUI input/select requests rendezvous with the event loop and are queued if another extension prompt is active; the line fallback uses ordinary prompts and ignores status/panel publication. See [tui.md](tui.md#extension-ui-integration). Host method failures use `-32602` for invalid parameters, `-32601` for unknown methods, and `-32000` for operation errors.

### Minimal Python plugin

This synchronous example is sufficient for one call at a time. Production plugins should dispatch calls and correlate concurrent IDs.

```python
#!/usr/bin/env python3
import json
import sys


def send(message):
    print(json.dumps(message, separators=(",", ":")), flush=True)


for line in sys.stdin:
    request = json.loads(line)
    method = request.get("method")
    ident = request.get("id")

    if method == "initialize":
        send({"jsonrpc": "2.0", "id": ident, "result": {
            "tools": [{
                "name": "echo",
                "description": "Echo text",
                "input_schema": {
                    "type": "object",
                    "properties": {"text": {"type": "string"}},
                    "required": ["text"],
                },
            }],
            "commands": [],
            "hooks": [],
        }})
    elif method == "tool.execute":
        text = request["params"]["args"]["text"]
        send({"jsonrpc": "2.0", "method": "tool.update",
              "params": {"id": ident, "message": "echoing"}})
        send({"jsonrpc": "2.0", "id": ident,
              "result": {"content": text}})
    elif method == "$/cancelRequest":
        pass
    elif ident is not None:
        send({"jsonrpc": "2.0", "id": ident,
              "error": {"code": -32601, "message": "method not found"}})
```

## Choosing a format

Use Lua as a first-class format for small, trusted, low-deployment-cost customizations. Use an executable plugin when you need a different language, process isolation for crashes, external libraries, or a protocol boundary shared with other tools. Porting a Pi TypeScript extension generally means translating its registrations and event handlers to one of these APIs; Notch does not embed a TypeScript/JavaScript runtime.
