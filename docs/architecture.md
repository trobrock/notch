# Architecture

Notch is one Go program with a deliberately small provider-independent agent loop. The only linked third-party runtime library is GopherLua; provider and MCP clients use Go's HTTP, JSON, process, and I/O packages directly. A normal installation is a single native executable and does not need npm or Node.js.

## Startup and component flow

`cmd/notch/main.go` is the composition root. Startup proceeds in this order:

1. Dispatch standalone authentication, model-list, version, and upgrade commands; otherwise parse agent flags and determine the current directory and home directory.
2. Load defaults, user config, and project config; apply `NOTCH_PROVIDER`, `NOTCH_MODEL`, and `NOTCH_THINKING_LEVEL`; then apply CLI overrides.
3. Create configured extension, skill, prompt, theme, and session directories.
4. Load built-in and custom theme JSON, select the configured theme, then create the terminal and extension registry and register built-in tools.
5. Discover and start executable plugins, then load top-level Lua files.
6. If the MCP config file exists, connect configured servers and register their tools.
7. Load skills and prompt templates and add their catalog summary to the system prompt.
8. Resolve the selected provider's environment or stored credential (refreshing expiring OAuth when needed), create the provider, refresh its model cache in the background when stale, create or resume a session, and construct the agent.
9. Run RPC mode, one prompt, the fullscreen TUI when both interactive streams are terminals, or the line-oriented fallback loop.

Malformed custom themes and plugin, Lua, and MCP load failures are generally shown as warnings so startup can continue. A failure while connecting a configured MCP set causes that set to be closed rather than leaving partial MCP connections active. Duplicate tool or command names are rejected; built-ins therefore cannot be silently replaced.

## Packages

- `internal/config`: layered JSON configuration and directory defaults.
- `internal/model`: provider-neutral messages, content blocks, tool definitions, requests, responses, and model-list metadata.
- `internal/modelregistry`: embedded model fallback plus atomic, endpoint-scoped, stale-on-demand provider cache.
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
- `internal/ui`: dependency-free line-oriented terminal I/O and fallback extension host operations.
- `internal/tui`: fullscreen event loop, multiline editor, layout, input parser, and differential terminal renderer.
- `internal/rpc`: strict JSONL command server, Pi-compatible event adapter, asynchronous prompt control, and headless extension host.
- `internal/upgrade`: GitHub release lookup, semantic-version comparison, checksum verification, constrained archive extraction, and atomic executable replacement.

## Agent loop

The agent stores a provider-neutral ordered message list. Calls to an agent are mutex-serialized, so interactive history and the session file cannot interleave.

For each user prompt:

1. Append and persist the user message.
2. Run `before_agent_start`; an extension may replace `system_prompt`.
3. Compact older context first if the configured automatic threshold has been reached, then send the effective history, system prompt, tool schemas, model, output-token limit, and current thinking level to the provider.
4. Stream answer and provider-supplied thinking deltas to the event renderer.
5. Append and persist the complete assistant response.
6. If there are tool calls, run each call in response order and append all results as one user message.
7. At each safe turn boundary, atomically take one queued steering message before continuing the normal tool-call chain. Steering can redirect the next model turn without interrupting an in-flight request or tool.
8. When the run would otherwise settle, run `agent_end`; a non-empty hook `follow_up` continues immediately. Otherwise atomically take one queued user follow-up. If neither exists, mark the run idle and complete.

A prompt, including steering and follow-up turns, is capped at 50 model turns internally. Queue state and an atomic effective-message count remain independent of the long-held conversation mutex, allowing RPC/TUI status and queued messages to stay responsive while provider or tool work is active. Tool calls in one assistant response execute sequentially, not concurrently. Context compaction summarizes old messages and retains recent complete turns; durable compaction records restore that effective context on resume. There is no branching, rewind, or session-tree navigation. See [compaction.md](compaction.md) for thresholds and persistence.

`tool_call` hooks can deny a call or replace its arguments. `tool_execution_start` and `tool_execution_end` surround execution. See [extensions.md](extensions.md) for hook payloads.

## Provider adapters

The model layer represents text, tool use, and tool results as typed blocks. Each provider translates those blocks to its native API rather than routing through a generic SDK:

- Anthropic sends `POST <base_url>/v1/messages`, requests SSE, and maps Anthropic `tool_use`/`tool_result` blocks. It supports API-key and Claude Pro/Max OAuth request modes.
- OpenAI sends `POST <base_url>/v1/responses`, requests SSE, and maps Responses `function_call`/`function_call_output` items.
- Codex reuses the native Responses translation but sends to ChatGPT's `/backend-api/codex/responses` path with the OAuth account scope required by the Codex backend.
- OpenRouter sends streaming requests to `<base_url>/chat/completions` and translates Chat Completions messages and tool calls.

Answer text and provider-supplied thinking summaries are streamed to the terminal; tool arguments are assembled from deltas before execution. Completed assistant content, thinking blocks, and token usage are retained. OpenAI Responses/Codex reasoning summaries, Anthropic thinking blocks and signatures, and OpenRouter reasoning deltas use the same provider-neutral stream event. All adapters receive the provider-neutral thinking level: OpenAI Responses, Codex, and OpenRouter translate it to native reasoning effort, while Anthropic uses adaptive effort on supported models or a bounded thinking budget on older models. Provider/model support still varies and services may clamp or reject a setting. API keys come from environment variables, while OAuth login supports `openai-codex`, `anthropic`, and `openrouter`. Stored credentials live in `~/.notch/auth.json` (or `$NOTCH_HOME/auth.json`) and refresh near expiry when the provider issues refresh tokens. There is no provider discovery, automatic model selection, fallback, retry policy, or rate-limit scheduler yet. See [providers.md](providers.md) for authentication precedence and verification notes.

Ollama uses the OpenAI adapter with `base_url` such as `http://localhost:11434`. Consequently it must expose `/v1/responses`; configuring an older Chat Completions-only server is insufficient.

## Tool registry

Every tool has a name, description, JSON input schema, source, and execution handler. The registry combines:

1. seven built-in tools,
2. executable plugin tools,
3. Lua tools,
4. MCP tools.

Registration order matters only for collision detection; definitions sent to providers are sorted by name. Commands and hooks share the same registry abstraction. Hooks of a given name execute in registration order, and each hook receives values merged from earlier hooks.

Tool policy is applied after all configured sources register. `--tools` retains only an exact allowlist, `--exclude-tools` removes exact names, `--no-builtin-tools` skips built-in registration while preserving extension/MCP tools, and `--no-tools` clears the registry. Unknown requested names are startup errors.

The built-ins resolve relative paths against Notch's startup working directory. Absolute paths are accepted; there is no repository-boundary jail. `bash` invokes `/bin/sh -c` (or `cmd.exe /C` on Windows). Potentially unbounded built-in output is capped at 50 KiB. `read` defaults to 2,000 lines. `edit` requires one exact unique occurrence.

## Sessions and resources

Sessions are version-1 JSONL files. Creation is exclusive, names combine UTC time and random bytes, and files are mode 0600. The metadata header includes the original CWD/provider/model; messages use the provider-neutral block representation. Appends and the initial header are synced to disk. `--continue` opens the most recently modified `.jsonl` file; `--resume` resolves an exact path, ID, filename, or unambiguous ID prefix. The fullscreen `/resume` selector inspects valid sessions and orders them by modification time.

Sessions preserve conversation state, not process state: extension declarations, current config, system prompt, and MCP connections are rebuilt on every launch. Message, compaction, and reset records determine the effective context. When continuing, the provider and model used for new requests come from current config/CLI, even though the original values remain in the session header. Fullscreen `/new` creates and switches to a distinct session when persistence is enabled; `/resume` restores an existing session's effective messages and submitted-input history. In no-session mode `/new` resets only memory and resume is disabled.

Resources are read once at startup. The binary embeds `notch-config` and `notch-extension` skills so an installed Notch can configure itself and author extensions without a source checkout. Bundled skills are the lowest-precedence layer and may be overridden by a disk skill with the same declared name. In addition to configured Notch directories, discovery reads `~/.agents/skills`, `<cwd>/.agents/skills`, `~/.agents/commands`, and `<cwd>/.agents/commands` without creating those shared directories. Skills accept top-level Markdown files and one-level `name/SKILL.md` directories. Commands/templates accept top-level Markdown files and expose descriptions plus optional `argument-hint` metadata to slash completion. Later directories overwrite earlier resources by declared name. Their bodies are expanded into ordinary user input before it enters the agent.

## Extension boundaries

Lua extensions run in-process in isolated Lua states (one state per file). Calls into a state are serialized and inherit the request context for cancellation. They are trusted code and can call exposed host operations.

Executable plugins run as child processes with their working directory set to the manifest directory. Newline-delimited JSON-RPC 2.0 travels over stdin/stdout; stderr is inherited for diagnostics. This boundary is language-neutral but is not a security sandbox. The host can execute programs, read input, present a selection, notify the user, and report the working directory.

MCP is a separate client boundary. Stdio servers are child processes; HTTP servers support JSON and SSE responses plus MCP session IDs. Tools are namespaced to avoid cross-server collisions. Notch currently consumes MCP tools only. MCP resources, prompts, sampling, elicitation, and OAuth are outside the MVP.

## Terminal and event output

For an interactive invocation, the fullscreen TUI is selected only when both stdin and stdout are TTYs and `--no-tui` and `--json` are absent. It uses raw input and the alternate screen. Redirected or piped streams, `--no-tui`, JSON output, and one-shot prompts stay on the buffered line-oriented path. The Pi-style layout uses padded full-width user boxes, plain assistant text, status-colored tool boxes, thinking-colored editor rules, and a two-line footer for cwd/Git plus usage/context/provider/model/thinking. See [tui.md](tui.md) for behavior and keys.

The fullscreen event loop blocks on terminal input, agent/extension events, and `SIGWINCH`; there is no polling loop or idle render ticker. Answer and thinking deltas start a one-shot 33 ms timer to coalesce output while a response is streaming, and non-stream model events flush pending text in order. Transcript entries cache wrapping until their text or width changes. The screen compares frames and emits only changed rows, assembling each render into a single buffered write. These choices reduce idle work and terminal traffic, but they do not eliminate rebuilding layout state when an event actually requires a frame.

Extension `Input` and `Select` calls rendezvous with the fullscreen event loop and appear as modal transcript/composer interactions; requests are queued and honor cancellation. The line fallback presents the same host operations as ordinary prompts.

With `--json`, agent events are JSON-encoded to stdout as JSONL. Startup warnings can still appear on stderr. Consumers should treat event additions as possible while the MVP API settles. Current fullscreen gaps include mouse support, configurable keybindings, inline (non-alternate-screen) mode, tool-output expand/collapse, tool approval, and branching/session-tree navigation beyond the flat resume selector.
