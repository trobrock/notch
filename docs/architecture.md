# Architecture

Notch is one Go program with a deliberately small provider-independent agent loop. The only linked third-party runtime library is GopherLua; provider and MCP clients use Go's HTTP, JSON, process, and I/O packages directly. A normal installation is a single native executable and does not need npm or Node.js.

## Startup and component flow

`cmd/notch/main.go` is the composition root. Startup proceeds in this order:

1. Parse CLI flags and determine the current directory and home directory.
2. Load defaults, user config, project config, then apply CLI overrides.
3. Create configured extension, skill, prompt, and session directories.
4. Create the terminal and extension registry; register built-in tools.
5. Discover and start executable plugins, then load top-level Lua files.
6. If the MCP config file exists, connect configured servers and register their tools.
7. Load skills and prompt templates and add their catalog summary to the system prompt.
8. Resolve the selected provider's environment or stored credential (refreshing expiring OAuth when needed), create the provider, create or resume a session, and construct the agent.
9. Run one prompt or enter the line-oriented input loop.

Plugin, Lua, and MCP load failures are generally shown as warnings so startup can continue. A failure while connecting a configured MCP set causes that set to be closed rather than leaving partial MCP connections active. Duplicate tool or command names are rejected; built-ins therefore cannot be silently replaced.

## Packages

- `internal/config`: layered JSON configuration and directory defaults.
- `internal/model`: provider-neutral messages, content blocks, tool definitions, requests, and responses.
- `internal/provider/anthropic`: native streaming Anthropic Messages client.
- `internal/provider/openai`: native streaming OpenAI Responses client; custom base URLs enable compatible servers such as Ollama.
- `internal/provider/codex`: configures the Responses client for ChatGPT's native Codex endpoint.
- `internal/provider/openrouter`: native streaming OpenRouter Chat Completions client.
- `internal/credentials`: mode-0600 provider credential store and one-time Pi import.
- `internal/oauth`: browser/loopback login and token refresh for supported providers.
- `internal/agent`: serialized model/tool loop and event emission.
- `internal/tools`: built-in filesystem, search, edit, and shell tools.
- `internal/session`: durable append-only JSONL conversation storage.
- `internal/resources`: discovery and expansion of Markdown skills/templates.
- `internal/extension`: common registry plus executable JSON-RPC plugin host.
- `internal/luaext`: embedded Lua manager and Notch Lua API.
- `internal/mcp`: MCP initialization, tool discovery/calling, stdio, and Streamable HTTP transports.
- `internal/ui`: dependency-free terminal I/O and extension host operations.

## Agent loop

The agent stores a provider-neutral ordered message list. Calls to an agent are mutex-serialized, so interactive history and the session file cannot interleave.

For each user prompt:

1. Append and persist the user message.
2. Run `before_agent_start`; an extension may replace `system_prompt`.
3. send the complete history, system prompt, tool schemas, model, and output-token limit to the provider.
4. Stream text deltas to the event renderer.
5. Append and persist the complete assistant response.
6. If there are tool calls, run each call in response order, append all results as one user message, and request another model turn.
7. If there are no calls, run `agent_end`; a non-empty `follow_up` result adds a synthetic user message and continues. Otherwise the prompt is complete.

A prompt is capped at 50 model turns internally. Tool calls in one assistant response execute sequentially, not concurrently. There is currently no context compaction, token-budget pruning, branching, or rewind. Resuming loads the entire saved message history.

`tool_call` hooks can deny a call or replace its arguments. `tool_execution_start` and `tool_execution_end` surround execution. See [extensions.md](extensions.md) for hook payloads.

## Provider adapters

The model layer represents text, tool use, and tool results as typed blocks. Each provider translates those blocks to its native API rather than routing through a generic SDK:

- Anthropic sends `POST <base_url>/v1/messages`, requests SSE, and maps Anthropic `tool_use`/`tool_result` blocks. It supports API-key and Claude Pro/Max OAuth request modes.
- OpenAI sends `POST <base_url>/v1/responses`, requests SSE, and maps Responses `function_call`/`function_call_output` items.
- Codex reuses the native Responses translation but sends to ChatGPT's `/backend-api/codex/responses` path with the OAuth account scope required by the Codex backend.
- OpenRouter sends streaming requests to `<base_url>/chat/completions` and translates Chat Completions messages and tool calls.

Only text is streamed to the terminal; tool arguments are assembled from deltas before execution. Completed assistant content and token usage are retained. API keys come from environment variables, while OAuth login supports `openai-codex`, `anthropic`, and `openrouter`. Stored credentials live in `~/.notch/auth.json` (or `$NOTCH_HOME/auth.json`) and refresh near expiry when the provider issues refresh tokens. There is no provider discovery, automatic model selection, fallback, retry policy, or rate-limit scheduler yet. See [providers.md](providers.md) for authentication precedence and verification notes.

Ollama uses the OpenAI adapter with `base_url` such as `http://localhost:11434`. Consequently it must expose `/v1/responses`; configuring an older Chat Completions-only server is insufficient.

## Tool registry

Every tool has a name, description, JSON input schema, source, and execution handler. The registry combines:

1. seven built-in tools,
2. executable plugin tools,
3. Lua tools,
4. MCP tools.

Registration order matters only for collision detection; definitions sent to providers are sorted by name. Commands and hooks share the same registry abstraction. Hooks of a given name execute in registration order, and each hook receives values merged from earlier hooks.

The built-ins resolve relative paths against Notch's startup working directory. Absolute paths are accepted; there is no repository-boundary jail. `bash` invokes `/bin/sh -c` (or `cmd.exe /C` on Windows). Potentially unbounded built-in output is capped at 50 KiB. `read` defaults to 2,000 lines. `edit` requires one exact unique occurrence.

## Sessions and resources

Sessions are version-1 JSONL files. Creation is exclusive, names combine UTC time and random bytes, and files are mode 0600. The metadata header includes the original CWD/provider/model; messages use the provider-neutral block representation. Appends and the initial header are synced to disk. `--continue` opens the most recently modified `.jsonl` file in `session_dir` and appends to it.

Sessions preserve conversation state, not process state: extension declarations, current config, system prompt, and MCP connections are rebuilt on every launch. When continuing, the provider and model used for new requests come from current config/CLI, even though the original values remain in the session header.

Resources are read once at startup. Skills accept top-level Markdown files and one-level `name/SKILL.md` directories. Templates accept top-level Markdown files. Later directories overwrite earlier resources by declared name. Their bodies are expanded into ordinary user input before it enters the agent.

## Extension boundaries

Lua extensions run in-process in isolated Lua states (one state per file). Calls into a state are serialized and inherit the request context for cancellation. They are trusted code and can call exposed host operations.

Executable plugins run as child processes with their working directory set to the manifest directory. Newline-delimited JSON-RPC 2.0 travels over stdin/stdout; stderr is inherited for diagnostics. This boundary is language-neutral but is not a security sandbox. The host can execute programs, read input, present a selection, notify the user, and report the working directory.

MCP is a separate client boundary. Stdio servers are child processes; HTTP servers support JSON and SSE responses plus MCP session IDs. Tools are namespaced to avoid cross-server collisions. Notch currently consumes MCP tools only. MCP resources, prompts, sampling, elicitation, and OAuth are outside the MVP.

## Terminal and event output

The default UI uses buffered line input and writes streamed assistant text to stdout. Status, tool starts, warnings, and errors generally go to stderr. It has no full-screen renderer, multiline editor, syntax highlighting, tool approval flow, or interactive session tree.

With `--json`, agent events are JSON-encoded to stdout as JSONL. Input and extension UI methods are still terminal-oriented, and startup warnings can still appear on stderr. Consumers should treat event additions as possible while the MVP API settles.
