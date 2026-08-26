# Migrating from Pi

Notch takes inspiration from Pi's coding-agent workflow, but it is not fully wire-, config-, session-, or extension-compatible. Treat migration as moving selected resources and behavior, not replacing the executable while keeping the same state directory.

## Decide whether the MVP fits

Notch is a good fit when you want a single Go binary, native Anthropic, OpenAI/Codex Responses, and OpenRouter access, local Ollama, built-in coding tools, simple persistent sessions, and extensions without a required npm runtime.

Stay on Pi, or run both during migration, if you depend on features outside Notch's smaller interface and workflow surface. Notch now mirrors Pi's core conversation presentation, themes, thinking controls, `/new`, and context compaction. Remaining gaps include:

- no Markdown tables/images, general mouse interaction beyond text selection, configurable keybindings, inline mode, or tool-output expand/collapse;
- no branching/session-tree navigation;
- only the core Pi RPC state/prompt/event subset, not the full command surface;
- provider OAuth is limited to `openai-codex`, `anthropic`, and `openrouter`;
- MCP supports tools plus authorization-code OAuth for standards-compliant Streamable HTTP servers, but not resources, prompts, elicitation, sampling, or app UI;
- no runtime for Pi TypeScript extensions;
- a smaller hook, command, and extension-host API;
- MCP tools only, rather than the broader MCP capability surface.

For headless integrations, `notch --mode rpc` supports Pi-style `get_state`, prompt acceptance, streaming message/tool events, steering, follow-ups, abort, and request IDs. Existing clients that use model/session switching, direct RPC bash, extension dialogs, images, retries, or branch commands must be adapted. See [RPC mode](rpc.md).

There is no automatic migration of the complete Pi installation. A one-time credential import is available, but sessions, config, resources, model registries, and extensions remain separate.

## Install separately

Build or install Notch without changing Pi:

```sh
go install github.com/trobrock/notch/cmd/notch@latest
notch --init
```

This creates `~/.notch` (or `$NOTCH_HOME`) and does not modify Pi's files. Run it in one project first and keep the two session stores separate.

## Translate provider configuration

Notch supports four provider values:

- `openai-codex`, using ChatGPT Plus/Pro OAuth and the native Codex Responses endpoint;
- `openrouter`, using `OPENROUTER_API_KEY` or OAuth and OpenRouter Chat Completions;
- `anthropic`, using the native Anthropic Messages API with `ANTHROPIC_API_KEY` or Claude Pro/Max OAuth;
- `openai`, using the native OpenAI **Responses** API with `OPENAI_API_KEY`, or a configured local endpoint.

Notch does not import Pi provider/model registries. It maintains its own embedded/provider-refreshed registry. Select a model in `~/.notch/config.json`, project `.notch/config.json`, with flags, or through fullscreen `/model`. See [providers and authentication](providers.md) for adapter details and current real-service verification status.

```json
{
  "provider": "anthropic",
  "model": "claude-sonnet-4-5",
  "max_tokens": 8192,
  "theme": "dark",
  "thinking_level": "medium",
  "compaction": {
    "enabled": true,
    "reserveTokens": 16384,
    "keepRecentTokens": 20000
  }
}
```

```sh
export ANTHROPIC_API_KEY=...
notch -p anthropic -m claude-sonnet-4-5
```

The built-in themes are `dark`, `dracula`, and `catppuccin-mocha`. Custom Pi-style JSON themes can be copied to `~/.notch/themes` or a trusted `<workspace-root>/.notch/themes`; Notch reads the semantic roles it renders and ignores known Pi-only roles. Add `"base": "dark"` when the file does not define every Notch role. Thinking levels are `off|minimal|low|medium|high|xhigh` and are transmitted by every provider adapter, although model support varies. In the fullscreen UI, `/theme`, `/thinking`, and `Shift-Tab` change runtime state only. See [themes](themes.md), [TUI controls](tui.md#commands-and-thinking-level), and [compaction](compaction.md).

For OpenAI-compatible local service, set `provider` to `openai` and `base_url` to the API origin. Notch always calls `<base_url>/v1/responses`, not Chat Completions. The tested Ollama route is:

```json
{
  "provider": "openai",
  "model": "gpt-oss:20b",
  "base_url": "http://localhost:11434"
}
```

```sh
ollama serve
ollama pull gpt-oss:20b
notch "List the available tools, then summarize this project"
```

No `OPENAI_API_KEY` is required when `base_url` is non-empty. Model names and tool/Responses support depend on the local Ollama installation.

For supported subscription/OAuth credentials, log in directly or perform a one-time import from Pi:

```sh
notch login openai-codex       # ChatGPT Plus/Pro
notch login anthropic          # Claude Pro/Max
notch login openrouter
notch auth status

# Copies ~/.pi/agent/auth.json by default; an explicit path is optional.
notch auth import-pi [path]
```

The import copies credentials into `~/.notch/auth.json` (or `$NOTCH_HOME/auth.json`), which is mode `0600`; it does not modify Pi's file or require a Node.js runtime. It is a one-time merge, not synchronization. Expiring OAuth credentials are refreshed automatically near expiry. Use `notch logout PROVIDER` to remove a stored credential.

## Move skills

Notch always provides `/skill:notch-config` and `/skill:notch-extension` from the binary. Disk skills with those names override the bundled versions. Project resources are considered only after one-time persisted trust at the canonical Git root; noninteractive untrusted and `--safe` runs skip project `.notch` and `.agents` inputs.

Notch reads shared Agent Skills directly from `~/.agents/skills` and `<project>/.agents/skills`, alongside its native locations:

```text
~/.notch/skills
<project>/.notch/skills
```

Resources that already live under `.agents/skills` do not need to be copied. Notch currently checks the startup working directory rather than walking ancestor directories. Copy other Pi-only skills you want to test. Notch recognizes either:

```text
skills/review.md
skills/review/SKILL.md
```

A simple Pi-style Markdown skill with `name` and `description` front matter is usually easy to reuse:

```markdown
---
name: review
description: Review changes for correctness
---
Review the changes. Additional focus: $ARGUMENTS
```

Invoke it as `/skill:review concurrency`. Verify advanced front matter: Notch parses only `name` and `description`, using a deliberately small YAML-like parser. Other metadata has no effect. Resource directories load in configured order, and the project resource wins when declared names collide.

## Move prompt templates

Notch reads top-level command/template `.md` files from both shared and native locations:

```text
~/.agents/commands
<project>/.agents/commands
~/.notch/prompts
<project>/.notch/prompts
```

Templates use limited `name`, `description`, and `argument-hint` front matter and replace every `$ARGUMENTS` marker. A file declaring `name: explain` is invoked as `/explain optional text`.

Check command-name conflicts after copying. Built-in commands are handled first, registered extension commands next, and prompt templates after that. All are visible in the `/` completion menu.

## Port extensions rather than copying them

Pi TypeScript extensions cannot be copied into `.notch/extensions`: Notch embeds no Node.js, JavaScript, or TypeScript runtime, and its API is not Pi's API. Choose one of these ports:

1. **Lua** for compact scripts. Translate tool, command, and event registration to `notch.register_tool`, `notch.register_command`, and `notch.on`.
2. **Executable JSON-RPC plugin** to retain TypeScript/Node or use any other language. The plugin can still be launched with a command such as `node dist/plugin.js`, but that runtime is then the plugin's own dependency, not a Notch dependency.

A TypeScript plugin port might have this manifest after its own build step:

```json
{
  "name": "my-ported-extension",
  "command": ["node", "dist/plugin.js"],
  "enabled": true
}
```

The program must implement Notch's line-delimited JSON-RPC protocol. It cannot import Pi extension types and expect compatibility. In particular:

- return tool/command/hook declarations from `initialize`;
- handle `tool.execute`, `command.execute`, and `hook.handle`;
- use `tool.update` for progress;
- use `host.cwd`, `host.exec`, and `host.ui.*` instead of Pi context APIs;
- honor `$/cancelRequest` where possible;
- write protocol JSON only to stdout and logs to stderr.

The complete Lua and executable APIs are in [extensions.md](extensions.md).

After porting, add a `notch-package.json` manifest and publish the ready-to-run files in Git. Consumers can then use `notch extensions install github:owner/repository` and `notch extensions update` without npm. Pi `package.json` metadata and npm package dependencies are not imported; see [extension packages](extension-packages.md) for Notch's manifest and source rules.

### Hook mapping

Port behavior to the nearest current Notch hook:

| Need | Notch hook/result |
|---|---|
| Amend system instructions before a turn | `before_agent_start`, return `system_prompt` |
| Inspect, rewrite, or deny a tool call | `tool_call`, return `arguments` or `denied`/`reason` |
| Observe tool boundaries | `tool_execution_start`, `tool_execution_end` |
| Request another turn after completion | `agent_end`, return `follow_up` |

Pi events with no equivalent need to be redesigned or deferred. Notch has native session reset and compaction, but does not expose session mutation, model switching, custom rendering, branching, or context replacement/compaction through the current extension API.

### Tool behavior differences

Review extension assumptions against Notch's built-ins:

- paths are resolved from the process startup CWD, but absolute paths are allowed;
- `edit` performs one exact unique replacement;
- `grep` uses Go regular expressions and filepath globs, not necessarily ripgrep semantics;
- `bash` uses the system shell and combines stdout/stderr;
- `notch.exec`/`host.exec` uses an argv array without a shell;
- built-in unbounded output is capped at 50 KiB;
- there is no approval prompt or sandbox before execution.

Avoid registering names already used by built-ins. Notch rejects collisions instead of replacing tools.

## Move MCP servers

Notch has a native MCP client, so MCP integrations generally belong in `~/.notch/mcp.json` rather than in an extension:

```json
{
  "mcpServers": {
    "local": {
      "command": "my-mcp-server",
      "args": ["--stdio"],
      "env": {"TOKEN": "..."}
    },
    "remote": {
      "url": "https://example.test/mcp",
      "auth": "oauth"
    }
  }
}
```

Notch supports stdio and Streamable HTTP with JSON or SSE responses. It performs the MCP handshake and imports advertised tools as `mcp__local__tool_name`. `enabled` defaults to true; set it to false to skip a server.

For a remote server that advertises standards-compliant OAuth metadata, set `"auth": "oauth"` and run `notch mcp login remote`. Notch performs metadata discovery, dynamic client registration, S256 PKCE through a loopback browser callback, resource binding, and token refresh; `notch mcp status` reports login state. OAuth credentials stay in Notch's separate global mode-0600 store and are bound to the exact configured URL. Static `headers` remain available for API-key servers.

For stdio servers, pass secrets explicitly in each server's `env` object because Notch supplies only a minimal child environment and does not inherit provider credentials or typical CI secrets. Notch does not currently consume MCP resources or prompts.

Use `--mcp-config path/to/file.json` to test a project-specific file before making it the default.

## Sessions do not migrate

Pi session files cannot be resumed by Notch. Notch's own files are append-only version-1 JSONL under `~/.notch/sessions` by default. Start a fresh Notch conversation and, if context is needed, paste or generate a summary from the Pi session.

```sh
notch                 # create a new session
notch --continue      # resume the latest Notch session
notch --resume ID     # resume a specific Notch session
notch --no-session    # save nothing
```

In the fullscreen UI, `/new` creates and switches to a distinct durable session and clears conversation context, transcript, and submitted-input history. `/resume` selects an older session and restores its effective context, transcript, and submitted-input history. With `--no-session`, `/new` performs the reset only in memory. `/compact [instructions]` persists a summary and retained recent context in Notch sessions; automatic compaction is enabled by default. See [compaction](compaction.md).

`--continue` means the most recently modified valid file in the configured global session directory, regardless of project. `--resume` and fullscreen `/resume` select existing valid sessions, but there is still no branching or session-tree navigation beyond the flat selectors. `session_dir` is global-only; use separate global configs or `NOTCH_HOME` values when automation needs isolated session stores.

## Suggested migration checklist

1. Install Notch and run `notch --init`.
2. Configure one provider login/API key or the tested local Ollama Responses route.
3. Run `go test ./...` in a disposable project through a one-shot prompt and review tool behavior.
4. Copy one skill/template at a time and verify slash expansion.
5. Reconfigure MCP stdio/HTTP servers and authorize OAuth servers with `notch mcp login NAME`.
6. Inventory Pi TypeScript extensions and classify each as Lua, executable plugin, or currently unsupported.
7. Test hooks and tool safety with `notch --no-session`.
8. Keep Pi available for old sessions, branches/session trees, custom themes, and richer Markdown UI workflows until Notch's limits are acceptable.
