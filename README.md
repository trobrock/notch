# Notch

[![CI](https://github.com/trobrock/notch/actions/workflows/ci.yml/badge.svg)](https://github.com/trobrock/notch/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/trobrock/notch)](https://github.com/trobrock/notch/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Notch is a small coding agent distributed as a single Go binary. It has native adapters for Anthropic Messages, OpenAI Responses, ChatGPT's Codex Responses endpoint, and OpenRouter Chat Completions; it streams model output, executes tools, and stores conversations as append-only JSONL sessions. The OpenAI provider also works with a local Ollama server exposing its OpenAI-compatible Responses endpoint.

Notch is an MVP, not a drop-in reimplementation of [Pi](https://github.com/badlogic/pi-mono). It favors a dependency-light native core and portable extension protocols and does not embed a JavaScript runtime.

## What is included

- Native Anthropic Messages, OpenAI and Codex Responses, and OpenRouter Chat Completions providers (no provider SDKs)
- Provider OAuth for ChatGPT Plus/Pro (`openai-codex`), Claude Pro/Max (`anthropic`), and OpenRouter
- API-key access for Anthropic, OpenAI, and OpenRouter, plus local Ollama through OpenAI Responses
- Built-in `read`, `write`, `edit`, `bash`, `grep`, `find`, and `ls` tools with strict allow/exclude controls
- Pi-style fullscreen TUI with searchable provider/model selection, themed Markdown, tool cards, streamed thinking summaries or a fallback thinking indicator, and a multiline composer, plus a line-oriented fallback for pipes
- Streaming interactive and one-shot operation, including JSONL events and a Pi-compatible RPC subset
- Durable JSONL sessions, `--continue`, `/new`, and persisted conversation compaction
- Embedded self-configuration and extension-authoring skills, plus Markdown skills and prompt templates
- In-process Lua extensions
- Executable, line-delimited JSON-RPC 2.0 plugins in any language
- Decentralized extension packages installable and upgradeable from GitHub, Git, or local directories
- Native MCP client support over stdio and Streamable HTTP

See the [fullscreen TUI guide](docs/tui.md), [RPC mode](docs/rpc.md), [themes](docs/themes.md), [compaction](docs/compaction.md), [providers and authentication](docs/providers.md), [releases and upgrades](docs/releases.md), [architecture](docs/architecture.md), [extension API](docs/extensions.md), [extension packages](docs/extension-packages.md), [migration from Pi](docs/migration-from-pi.md), and the [migration plan for the reviewed Pi extensions](docs/current-pi-extension-plan.md).

## Status and deliberate gaps

Notch has a Pi-style fullscreen terminal UI with a multiline composer, transcript scrolling, Markdown-aware rendering, themes, thinking controls, context compaction, and extension prompts, plus a line-oriented fallback for redirection and automation. It does **not** yet have session branching/tree navigation or MCP OAuth. The TUI supports app-owned mouse-wheel scrollback and drag selection with `Ctrl-Y` copy (including tmux when mouse forwarding is enabled), and mouse capture can be disabled to restore terminal-native selection. It does not yet provide configurable keybindings, inline (non-alternate-screen) mode, or tool-output expand/collapse. Its Markdown support covers common prose and code constructs, but not terminal table layout, image display, or extensions such as task lists and strikethrough. Themes can be built in or loaded from simple semantic JSON files. MCP HTTP credentials can only be supplied as static headers. Provider OAuth is implemented for `openai-codex`, `anthropic`, and `openrouter`, but sessions and configuration are not automatically imported from Pi; credentials have an explicit one-time import command. Existing Pi TypeScript extensions do not run in Notch and must be ported to Lua or the executable JSON-RPC protocol.

Tools, extensions, plugins, and MCP servers run with the user's privileges. Notch has no sandbox or per-command approval prompts: once a workspace is trusted, enabled project code and model-requested tools execute automatically. See [Workspace trust](#workspace-trust) and [SECURITY.md](SECURITY.md).

## Build and install

Building Notch requires Go 1.23 or newer.

```sh
git clone https://github.com/trobrock/notch.git
cd notch
make test
make build
./bin/notch --version
```

Prebuilt archives for Linux, macOS, and Windows on amd64 and arm64 are attached to each [GitHub release](https://github.com/trobrock/notch/releases). Verify an archive against the release's `checksums.txt`, unpack it, and place `notch` on `PATH`. Or install directly into `GOBIN`/`$GOPATH/bin`:

```sh
go install github.com/trobrock/notch/cmd/notch@latest
```

Notch can report detailed build metadata and replace a writable installation with a verified release:

```sh
notch version
notch upgrade --check
notch upgrade
```

See [releases and upgrades](docs/releases.md) for exact-version installs, downgrade rules, checksums, and package-manager installations. The resulting Notch executable needs no Node.js, npm, or runtime package installation. Executable extensions and stdio MCP servers may, of course, require their own runtimes.

## Quick start

Create the standard directories and starter config:

```sh
notch --init
```

For Anthropic:

```sh
export ANTHROPIC_API_KEY=sk-ant-...
notch --provider anthropic --model claude-sonnet-4-5
```

For OpenAI:

```sh
export OPENAI_API_KEY=sk-...
notch --provider openai --model gpt-5
```

For a ChatGPT Plus/Pro subscription or OpenRouter:

```sh
notch login openai-codex
notch --provider openai-codex --model gpt-5.6-terra

# Alternatively, use OpenRouter OAuth or OPENROUTER_API_KEY.
notch login openrouter
notch --provider openrouter --model anthropic/claude-sonnet-4.5
```

Claude Pro/Max OAuth is available with `notch login anthropic`; API-key authentication remains available as shown above. See [providers and authentication](docs/providers.md) for authentication precedence, credential storage, test status, and the Pi import command.

A tested local setup uses Ollama's OpenAI-compatible Responses endpoint. Start Ollama, install a model that supports the Responses/tool-calling behavior you need, and point Notch at the server:

```sh
ollama serve
# In another terminal; substitute an installed tool-capable model.
ollama pull qwen3.5:9b
mkdir -p ~/.notch
cat > ~/.notch/config.json <<'JSON'
{
  "provider": "openai",
  "model": "qwen3.5:9b",
  "base_url": "http://localhost:11434"
}
JSON
notch "Inspect this repository and summarize it."
```

When `base_url` is set, Notch does not require `OPENAI_API_KEY`. It sends requests to `<base_url>/v1/responses`; the model and Ollama release must support that endpoint and the features used by the prompt. The setup above is the tested path, rather than the older Chat Completions endpoint.

## CLI

```text
notch [flags] [prompt words...]

  --provider, -p string   openai-codex, openrouter, anthropic, or openai
  --model, -m string      model ID
  --thinking string       off, minimal, low, medium, high, or xhigh
  --print string          run one prompt and exit
  --system-prompt string  override the configured system prompt
  --system-prompt-file    read the system prompt override from a file
  --continue              continue the most recently modified session
  --no-session            do not create or update a session
  --json                  emit JSONL agent events
  --no-tui                use the line-oriented interface
  --mode rpc              run Pi-compatible JSONL RPC mode
  --rpc                   shorthand for --mode rpc
  --tools, -t string      strict comma-separated tool allowlist
  --exclude-tools string  comma-separated tools to disable
  --safe                  skip project configuration, extensions, and resources
  --trust-workspace       persist trust for this workspace (automation/CI)
  --no-builtin-tools      disable built-in tools
  --no-extensions         disable official and configured extensions
  --no-resources          disable skills and prompt templates
  --no-tools              disable all model tools
  --mcp-config string     path to MCP JSON config
  --init                  create the Notch directories and starter config
  --version               print the version
```

Version commands are:

```text
notch version [--json]
notch upgrade [--check] [--version VERSION] [--force]
```

Authentication commands are:

```text
notch login PROVIDER
notch logout PROVIDER
notch auth status
notch auth import-pi [path]
```

Extension package commands are:

```text
notch extensions init [--name NAME] [DIRECTORY]
notch extensions validate [--json] [DIRECTORY]
notch extensions install [--ref REF] [--subdir PATH] SOURCE
notch extensions list [--json]
notch extensions update [--force] [NAME...]
notch extensions remove NAME
```

`login` supports `openai-codex`, `anthropic`, and `openrouter`. Positional words become a one-shot prompt when `--print` is absent:

```sh
notch --print "Explain internal/agent"
notch --json "Run the tests and report failures" | jq -c .
printf '%s\n' '{"id":"s","type":"get_state"}' | notch --mode rpc --no-session --tools read,grep
notch models openrouter
notch models --json --all
notch models --refresh anthropic
notch --continue
notch --resume 20260823T221401
notch --no-session
notch --mcp-config ./mcp.json
```

Fullscreen interactive commands include `/help`, `/model [refresh]`, `/tools`, `/skills`, `/thinking [LEVEL]`, `/theme [NAME]`, `/compact [instructions]`, `/plan [on|off|status]`, `/new`, `/resume`, `/clear`, `/exit`, and `/quit`, plus commands registered by extensions. Typing `/` opens a filtered command menu with descriptions; Up/Down selects an entry and Tab or Enter completes it. Skills are invoked as `/skill:name arguments`; prompt templates use `/name arguments`. See the [TUI guide](docs/tui.md) for runtime command behavior.

`--mode rpc` exposes asynchronous Pi-style command responses and streaming events over strict JSONL; see [RPC mode](docs/rpc.md). Tool flags apply to every run mode after built-in, extension, and MCP registration.

An interactive invocation uses the fullscreen TUI only when both stdin and stdout are terminals. `--no-tui`, `--json`, and one-shot prompts use the line-oriented path; redirected input/output does too unless RPC mode was selected explicitly. The fullscreen UI runs in the terminal's alternate screen. While a model is active, Enter queues steering for the next safe turn boundary and Alt-Enter queues a follow-up for after the run would otherwise settle; pending messages remain visible above the composer until delivered. It mirrors Pi's presentation: padded full-width user background boxes, plain assistant prose, Markdown styling, provider-supplied thinking summaries with a static fallback indicator, and tool cards with state-colored backgrounds and pending/success/error icons. Transcript entries use consistent blank-row spacing; tool arguments are compact, while output is visually barred and shortened when large. Rendering wraps by terminal display width (including Unicode), is cached by text, width, and theme where styling applies, and sanitizes untrusted text so model/tool content cannot inject terminal controls. See [the TUI guide](docs/tui.md) for details, keys, extension prompts, and fallback behavior.

`--continue` selects the latest session globally in the configured session directory. `--resume ID-OR-PREFIX` opens a specific session by ID, filename, unambiguous prefix, or path. Fullscreen `/resume` presents saved sessions with time, original directory, model, and prompt preview. Resumed requests use the current provider/model configuration while preserving the selected session's conversation context. Each completed provider response, including compaction summaries, appends a `usage` record with provider, model, input/output token counts, and stop reason to the session JSONL.

## Workspace trust

Project-controlled `.notch` and `.agents` inputs are loaded only for a trusted workspace. Notch identifies a Git worktree by its canonical Git root (or the canonical current directory outside Git), so one decision applies throughout the repository. If supported project inputs exist and the workspace is not already trusted, a run with terminal stdin and stdout prompts once and persists an accepted decision in `$NOTCH_HOME/trusted-workspaces.json` (default `~/.notch/trusted-workspaces.json`). Runs without project inputs do not prompt.

Noninteractive runs never prompt and skip project `.notch`/`.agents` config, MCP, extensions, skills, prompts, and themes unless trust was previously persisted. Use `--trust-workspace` to persist trust explicitly for automation or CI. Use `--safe` to bypass project trust and project inputs for that invocation; it cannot be combined with `--trust-workspace`. Global inputs and installed extension packages remain available. Trust is an execution boundary, not an approval workflow: Notch has no per-command approvals, and tools/extensions execute automatically after loading.

## Configuration

Configuration is JSON. For trusted workspaces, Notch starts with defaults, then merges:

1. `$NOTCH_HOME/config.json`, when `NOTCH_HOME` is set, otherwise `~/.notch/config.json`
2. `<workspace-root>/.notch/config.json` (trusted workspaces only)
3. `NOTCH_PROVIDER`, `NOTCH_MODEL`, and `NOTCH_THINKING_LEVEL`
4. CLI overrides such as `--provider`, `--model`, and `--thinking`

Later non-empty values replace earlier ones, so CLI flags take precedence over environment variables and environment variables take precedence over project and user config. Empty or whitespace-only values for the three runtime environment variables are ignored and fall back to the merged config files. Non-empty directory arrays replace the complete earlier array; they are not appended. `base_url`, `auth_file`, `session_dir`, and `model_cache` are security-sensitive global-only settings and are ignored in project config. API keys can come from environment variables; OAuth credentials are kept separately from config in the protected auth store. Standalone `notch login`, `logout`, `auth`, and `models` commands also load global configuration only.

```json
{
  "provider": "anthropic",
  "model": "claude-sonnet-4-5",
  "base_url": "",
  "max_tokens": 8192,
  "theme": "dark",
  "thinking_level": "medium",
  "context_window": 0,
  "model_cache": "/home/me/.notch/models.json",
  "model_refresh_hours": 24,
  "compaction": {
    "enabled": true,
    "reserveTokens": 16384,
    "keepRecentTokens": 20000
  },
  "system_prompt": "You are a coding agent. Help the user understand and modify their codebase.",
  "mcp_config": "/home/me/.notch/mcp.json",
  "extension_dirs": [
    "/home/me/.notch/extensions",
    "/work/project/.notch/extensions"
  ],
  "skill_dirs": [
    "/home/me/.notch/skills",
    "/work/project/.notch/skills"
  ],
  "prompt_dirs": [
    "/home/me/.notch/prompts",
    "/work/project/.notch/prompts"
  ],
  "theme_dirs": [
    "/home/me/.notch/themes",
    "/work/project/.notch/themes"
  ],
  "session_dir": "/home/me/.notch/sessions"
}
```

For example, a shell or per-command override can select a runtime without editing either config file:

```sh
NOTCH_PROVIDER=openai-codex NOTCH_MODEL=gpt-5.6-sol NOTCH_THINKING_LEVEL=high notch
```

Provider and model overrides are independent: if only one environment variable is set, the other value comes from the merged config. `notch models [provider]` lists provider-discovered or fallback models; add `--refresh` to bypass the cache, `--json` for a versioned machine-readable catalog, or `--all` for every supported provider. Fullscreen `/model` selects a provider and filters its models, while `/model refresh` forces discovery. Runtime selection changes subsequent turns and new sessions but does not edit config files. The paths above illustrate the defaults; actual home and workspace-root paths are resolved at startup. `NOTCH_HOME` relocates user config, user resources, user extensions, the default MCP file, and sessions. Trusted project resources remain under `<workspace-root>/.notch`. Only global configured resource directories are created before trust; trusted project directories may then be created. Notch also discovers shared skills in `~/.agents/skills` and trusted `<workspace-root>/.agents/skills`, plus command templates in `~/.agents/commands` and trusted `<workspace-root>/.agents/commands`; these shared directories are discovered but never created by Notch. `theme` and `thinking_level` can be set in either global or trusted project config. Direct JSON files in `theme_dirs` add or override themes; see [themes](docs/themes.md) for the small semantic schema. The mode-0600 model cache refreshes stale selected-provider data on startup or selector use without a polling timer; `model_refresh_hours` defaults to 24. `context_window: 0` means use the provider/model default; omit it for the same behavior. The compaction object deliberately uses Pi-compatible camelCase keys. See [themes](docs/themes.md), [thinking controls](docs/tui.md#commands-and-thinking-level), and [compaction](docs/compaction.md).

Provider credentials:

- `openai-codex`: ChatGPT Plus/Pro OAuth from `notch login openai-codex`.
- `openrouter`: `OPENROUTER_API_KEY` or `notch login openrouter`.
- `anthropic`: `ANTHROPIC_API_KEY` or Claude Pro/Max OAuth from `notch login anthropic`.
- `openai`: `OPENAI_API_KEY`, unless `base_url` points to a local service such as Ollama.

OAuth credentials are stored in `~/.notch/auth.json` (or `$NOTCH_HOME/auth.json`) with mode `0600` and refreshed when close to expiry. Full details and current real-service verification notes are in [docs/providers.md](docs/providers.md).

## Skills and prompt templates

Every binary includes two built-in skills:

- `/skill:notch-config` configures providers, models, authentication, themes, thinking, compaction, sessions, resources, and MCP;
- `/skill:notch-extension` builds and tests Lua extensions or executable JSON-RPC plugins and explains when MCP or a core change is more appropriate.

The built-ins require no source checkout or external documentation. A user or project skill declaring the same name overrides the bundled version, so the defaults remain customizable.

Additional skills may be either `skills/name.md` or `skills/name/SKILL.md`. Notch discovers them in `~/.agents/skills`, configured `skill_dirs` (defaulting to the user path and, for a trusted workspace, project `.notch/skills`), and trusted `<workspace-root>/.agents/skills`. Command/prompt templates are top-level `.md` files discovered from matching `.agents/commands` and configured prompt directories. Later locations win when names collide, so trusted project `.agents` resources have final precedence. Untrusted/noninteractive and `--safe` runs skip all project resources.

```markdown
---
name: review
description: Review a change for correctness
---
Review the current changes with this focus: $ARGUMENTS
```

Put that file at `.notch/prompts/review.md` or `.agents/commands/review.md` and run `/review concurrency`. Front matter supports scalar `name`, `description`, and `argument-hint` fields (including simple quoted or `|`/`>` values), not arbitrary YAML. `argument-hint` is shown in slash-command completion. Every `$ARGUMENTS` occurrence is replaced. Loaded resource names and descriptions are also added to the system prompt.

## MCP

The default MCP file is `~/.notch/mcp.json` (or `$NOTCH_HOME/mcp.json`). It is only loaded if the file exists.

```json
{
  "mcpServers": {
    "files": {
      "command": "/absolute/path/to/server",
      "args": ["--root", "/work"],
      "env": {"LOG_LEVEL": "warn"}
    },
    "remote": {
      "url": "https://mcp.example.test/mcp",
      "headers": {"Authorization": "Bearer static-token"}
    },
    "off": {
      "command": "some-server",
      "enabled": false
    }
  }
}
```

A server must specify exactly one of `command` or `url`. `enabled` defaults to true. Stdio children receive only a minimal inherited process environment plus variables explicitly supplied in that server's `env` object; provider credentials and typical CI secrets are not inherited automatically. Remote tools are exposed as `mcp__<server>__<tool>`. Notch performs the MCP 2025-06-18 handshake, follows paginated `tools/list`, and calls tools over stdio or HTTP responses in JSON or SSE form. Resource/prompt MCP capabilities and OAuth are not implemented yet.

## Sessions and JSON output

Unless `--no-session` is used, every new invocation creates a mode-0600 JSONL file under the configured session directory. The first record is metadata (format version, ID, time, CWD, provider, model); subsequent records contain messages, per-turn provider usage, and durable compaction records. Each append is synced before continuing. In the fullscreen UI, `/new` creates a distinct durable session and clears transcript context and input history; under `--no-session` it performs the same reset in memory. `/resume` switches to a selected saved session and restores its effective transcript, context, and submitted-input history.

`--json` emits one JSON object per line. Current event types include `turn_start`, `text_delta`, `thinking_delta`, `turn_end` (with token usage), `tool_start`, `tool_update`, `tool_end`, `queue_update`, `queue_delivered`, compaction events, and `error`. The `--json` stream is a one-way event interface, not the on-disk session format. Use `--mode rpc` for bidirectional state, prompt, queue, and abort commands.

## Extensions

Place `.lua` files or executable-plugin directories below either:

- `~/.notch/extensions` (or `$NOTCH_HOME/extensions`)
- `<workspace-root>/.notch/extensions` (trusted workspaces only)

Lua is a first-class extension format alongside executable plugins. Lua files are loaded directly from each extension directory; executable manifests named `plugin.json` are discovered recursively. Each extension's tools, commands, and hooks register atomically, and registrations are removed when that Lua state, plugin, or MCP connection closes, so failed loads do not leave partial entries behind. Extensions can register model tools, interactive slash commands, and agent hooks. Full examples and the wire protocol are in [docs/extensions.md](docs/extensions.md).

Use `notch extensions install SOURCE` to install a shareable package from GitHub, generic Git, or a local directory. Installed content and its exact commit/digest lock live under `$NOTCH_HOME` or `~/.notch`; updates and removals are atomic. See [extension packages](docs/extension-packages.md) for the manifest, source syntax, integrity checks, security model, and publishing workflow.

## Community

- Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.
- Follow the [Code of Conduct](CODE_OF_CONDUCT.md) in project spaces.
- Report vulnerabilities privately using [SECURITY.md](SECURITY.md).
- Notch is available under the [MIT License](LICENSE).
