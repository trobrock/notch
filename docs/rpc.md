# RPC mode

Notch RPC mode is a headless, Pi-compatible JSONL control interface for embedding Notch in editors, processes, and custom UIs.

```sh
notch --mode rpc [options]
# Equivalent shorthand:
notch --rpc [options]
```

The initial compatibility surface includes state/status queries, prompt submission, streaming events, steering and follow-up queues, abort, and tool discovery. It is intentionally smaller than Pi's complete RPC API; unsupported commands return a normal failed response rather than being ignored.

## Framing

Commands are JSON objects written to stdin, one per LF-terminated line. Responses and events are JSON objects written to stdout, one per line. CRLF input is accepted by stripping the trailing CR. A final command without LF is rejected. Commands are limited to 16 MiB.

Every command may include an `id` string, number, or JSON value. Its response preserves that ID:

```json
{"id":"state-1","type":"get_state"}
{"id":"state-1","type":"response","command":"get_state","success":true,"data":{}}
```

Diagnostics and startup failures go to stderr so stdout remains machine-readable.

## Commands

### `get_state`, `get_status`, or `status`

`get_state` is the Pi-compatible spelling. The aliases return the same data with their own command name.

```json
{"id":"state-1","type":"get_state"}
```

The response includes:

```json
{
  "type": "response",
  "command": "get_state",
  "success": true,
  "data": {
    "model": {
      "id": "gpt-5.6-sol",
      "name": "gpt-5.6-sol",
      "api": "openai-codex-responses",
      "provider": "openai-codex",
      "contextWindow": 272000,
      "maxTokens": 8192
    },
    "thinkingLevel": "medium",
    "isStreaming": false,
    "isCompacting": false,
    "steeringMode": "one-at-a-time",
    "followUpMode": "one-at-a-time",
    "autoCompactionEnabled": true,
    "messageCount": 0,
    "pendingMessageCount": 0,
    "sessionFile": "/home/me/.local/share/notch/sessions/example.jsonl",
    "sessionId": "example",
    "tools": ["bash", "edit", "find", "grep", "ls", "read", "write"]
  }
}
```

Session fields are omitted under `--no-session`. State queries remain responsive while a provider request or tool is active.

### `get_tools`

Returns each enabled model tool and its source:

```json
{"id":"tools-1","type":"get_tools"}
```

```json
{"id":"tools-1","type":"response","command":"get_tools","success":true,"data":{"tools":[{"name":"read","description":"Read a file","source":"builtin"}]}}
```

### `prompt`

Accepts a prompt and starts work asynchronously:

```json
{"id":"prompt-1","type":"prompt","message":"Inspect the repository"}
{"id":"prompt-1","type":"response","command":"prompt","success":true}
```

The success response is always written before `agent_start` or streaming events. Failure after acceptance is reported through events.

When an agent run is active, `prompt` requires a Pi-compatible `streamingBehavior`:

```json
{"type":"prompt","message":"Focus on the parser","streamingBehavior":"steer"}
{"type":"prompt","message":"Then summarize the changes","streamingBehavior":"followUp"}
```

`steer` is delivered at the next safe turn boundary after current tool results. `followUp` waits until the run would otherwise settle. Skill and command-template input is expanded before submission. Images and interactive extension UI requests are not supported in the initial RPC surface.

Dedicated queue commands are also accepted:

```json
{"type":"steer","message":"Focus on errors"}
{"type":"follow_up","message":"Then run the tests"}
```

### `abort`

Cancels the current provider/tool context. It succeeds even when no prompt is active:

```json
{"id":"abort-1","type":"abort"}
```

## Events

Notch emits the Pi event names needed by streaming clients:

- `agent_start`, `agent_end`, and `agent_settled`
- `turn_start` and `turn_end`
- `message_start`, `message_update`, and `message_end`
- `tool_execution_start`, `tool_execution_update`, and `tool_execution_end`
- `queue_update`
- `compaction_start` and `compaction_end`
- `error`

Text and thinking use Pi's delta envelope:

```json
{
  "type": "message_update",
  "usage": {"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0},
  "assistantMessageEvent": {
    "type": "text_delta",
    "contentIndex": 0,
    "delta": "Hello"
  }
}
```

`message_end.message` is authoritative. `turn_end` contains the assistant message and tool results. Tool calls use `toolCallId` for correlation. Provider token counts are included when the completed turn reports them; Notch currently reports zero RPC cost because provider pricing is not part of its model registry.

Failed commands use Pi's response envelope:

```json
{"id":"bad-1","type":"response","command":"prompt","success":false,"error":"prompt message is required"}
```

Malformed records use command `parse`.

## Tool restrictions

Tool policy is applied after built-ins, extensions, and MCP servers have registered, and before the agent is created:

```sh
# Strict allowlist across all tool sources.
notch --mode rpc --tools read,grep,find

# Disable selected tools.
notch --mode rpc --exclude-tools write,edit,bash

# Keep extension/MCP tools but do not register built-ins.
notch --mode rpc --no-builtin-tools

# Expose no model tools.
notch --mode rpc --no-tools
```

Shorthands compatible with Pi are `-t`, `-xt`, `-nbt`, and `-nt`. Unknown allowlisted or excluded names fail startup instead of silently weakening the requested policy. `--tools` is a strict allowlist across built-in, extension, and MCP tools. `--no-builtin-tools` only affects Notch's seven built-ins; it does not disable extension hooks, commands, or host privileges.

These flags work in fullscreen, line, JSON event, one-shot, and RPC modes. Workspace trust is resolved before mode selection: noninteractive RPC/JSON/one-shot runs skip untrusted project `.notch` and `.agents` inputs. Persist trust ahead of automation with `--trust-workspace`, or use `--safe` to force a global-only run. Neither flag adds per-command approvals; trusted execution remains automatic.

## Minimal Python client

```python
import json
import subprocess

agent = subprocess.Popen(
    ["notch", "--mode", "rpc", "--no-session", "--tools", "read,grep,find"],
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    text=True,
)

def send(command):
    agent.stdin.write(json.dumps(command) + "\n")
    agent.stdin.flush()

send({"id": "state", "type": "get_state"})
send({"id": "prompt", "type": "prompt", "message": "Summarize this repository"})

for line in agent.stdout:
    event = json.loads(line)
    if event.get("type") == "message_update":
        delta = event.get("assistantMessageEvent", {})
        if delta.get("type") == "text_delta":
            print(delta["delta"], end="", flush=True)
    if event.get("type") == "agent_settled":
        break
```

Close stdin or terminate the process when the client is done. Closing stdin cancels an active prompt and waits for it to settle before Notch exits.

## Current compatibility boundary

Notch does not yet implement Pi RPC model switching, session switching/forking, message/entry export, direct bash commands, extension UI dialogs, image input, retry controls, or compaction commands. The command/response envelope, request IDs, prompt queue behavior, core state fields, and core streaming event names above are the supported subset.
