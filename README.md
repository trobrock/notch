# Notch

Notch is a small coding agent distributed as a single Go binary. It has native adapters for Anthropic Messages, OpenAI Responses, ChatGPT's Codex Responses endpoint, and OpenRouter Chat Completions; it streams model output, executes tools, and stores conversations as append-only JSONL sessions. The OpenAI provider also works with a local Ollama server exposing its OpenAI-compatible Responses endpoint.

Notch is an MVP, not a drop-in reimplementation of [Pi](https://github.com/badlogic/pi-mono). It favors a dependency-light native core and portable extension protocols over a full-screen UI or a JavaScript runtime.

## What is included

- Native Anthropic Messages, OpenAI and Codex Responses, and OpenRouter Chat Completions providers (no provider SDKs)
- Provider OAuth for ChatGPT Plus/Pro (`openai-codex`), Claude Pro/Max (`anthropic`), and OpenRouter
- API-key access for Anthropic, OpenAI, and OpenRouter, plus local Ollama through OpenAI Responses
- Built-in `read`, `write`, `edit`, `bash`, `grep`, `find`, and `ls` tools
- Streaming interactive and one-shot operation, including JSONL events
- Durable JSONL sessions and `--continue`
- Markdown skills and prompt templates
- In-process Lua extensions
- Executable, line-delimited JSON-RPC 2.0 plugins in any language
- Native MCP client support over stdio and Streamable HTTP

See [providers and authentication](docs/providers.md), [architecture](docs/architecture.md), [extension API](docs/extensions.md), [migration from Pi](docs/migration-from-pi.md), and the [migration plan for the reviewed Pi extensions](docs/current-pi-extension-plan.md).

## Status and deliberate gaps

Notch currently has a basic line-oriented terminal UI. It does **not** yet have conversation compaction, session branching/tree navigation, or MCP OAuth. MCP HTTP credentials can only be supplied as static headers. Provider OAuth is implemented for `openai-codex`, `anthropic`, and `openrouter`, but sessions and configuration are not automatically imported from Pi; credentials have an explicit one-time import command. Existing Pi TypeScript extensions do not run in Notch and must be ported to Lua or the executable JSON-RPC protocol.

Tools and extensions run with the user's privileges. There is no sandbox or tool-approval UI yet.

## Build and install

Building Notch requires Go 1.22 or newer.

```sh
git clone https://github.com/trobrock/notch.git
cd notch
make test
make build
./bin/notch --version
```

Or install directly into `GOBIN`/`$GOPATH/bin`:

```sh
go install github.com/trobrock/notch/cmd/notch@latest
```

The resulting Notch executable needs no Node.js, npm, or runtime package installation. Executable extensions and stdio MCP servers may, of course, require their own runtimes.

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
  --print string          run one prompt and exit
  --continue              continue the most recently modified session
  --no-session            do not create or update a session
  --json                  emit JSONL agent events
  --mcp-config string     path to MCP JSON config
  --init                  create the Notch directories and starter config
  --version               print the version
```

Authentication commands are:

```text
notch login PROVIDER
notch logout PROVIDER
notch auth status
notch auth import-pi [path]
```

`login` supports `openai-codex`, `anthropic`, and `openrouter`. Positional words become a one-shot prompt when `--print` is absent:

```sh
notch --print "Explain internal/agent"
notch --json "Run the tests and report failures" | jq -c .
notch --continue
notch --no-session
notch --mcp-config ./mcp.json
```

Interactive commands are `/help`, `/tools`, `/skills`, `/exit`, and `/quit`, plus commands registered by extensions. Skills are invoked as `/skill:name arguments`; prompt templates use `/name arguments`.

`--continue` selects the latest session globally in the configured session directory, not necessarily one created in the current working directory. `--json` changes event rendering, but interactive input remains the same line interface.

## Configuration

Configuration is JSON. Notch starts with defaults, then merges:

1. `$NOTCH_HOME/config.json`, when `NOTCH_HOME` is set, otherwise `~/.notch/config.json`
2. `<working-directory>/.notch/config.json`
3. CLI overrides for provider, model, and MCP config

Later non-empty scalar values replace earlier ones. Non-empty directory arrays replace the complete earlier array; they are not appended. API keys can come from environment variables; OAuth credentials are kept separately from config in the protected auth store.

```json
{
  "provider": "anthropic",
  "model": "claude-sonnet-4-5",
  "base_url": "",
  "max_tokens": 8192,
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
  "session_dir": "/home/me/.notch/sessions"
}
```

The paths above illustrate the defaults; actual home and working-directory paths are resolved at startup. `NOTCH_HOME` relocates user config, user resources, user extensions, the default MCP file, and sessions. Project resources remain under `<cwd>/.notch`. Configured resource directories are created automatically.

Provider credentials:

- `openai-codex`: ChatGPT Plus/Pro OAuth from `notch login openai-codex`.
- `openrouter`: `OPENROUTER_API_KEY` or `notch login openrouter`.
- `anthropic`: `ANTHROPIC_API_KEY` or Claude Pro/Max OAuth from `notch login anthropic`.
- `openai`: `OPENAI_API_KEY`, unless `base_url` points to a local service such as Ollama.

OAuth credentials are stored in `~/.notch/auth.json` (or `$NOTCH_HOME/auth.json`) with mode `0600` and refreshed when close to expiry. Full details and current real-service verification notes are in [docs/providers.md](docs/providers.md).

## Skills and prompt templates

Skills may be either `skills/name.md` or `skills/name/SKILL.md`. Prompt templates are top-level `.md` files in a prompt directory. User directories load first and project directories load later, so a project resource with the same declared name wins.

```markdown
---
name: review
description: Review a change for correctness
---
Review the current changes with this focus: $ARGUMENTS
```

Put that file at `.notch/prompts/review.md` and run `/review concurrency`. Front matter supports scalar `name` and `description` fields (including simple quoted or `|`/`>` values), not arbitrary YAML. Every `$ARGUMENTS` occurrence is replaced. Loaded resource names and descriptions are also added to the system prompt.

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

A server must specify exactly one of `command` or `url`. `enabled` defaults to true. Remote tools are exposed as `mcp__<server>__<tool>`. Notch performs the MCP 2025-06-18 handshake, follows paginated `tools/list`, and calls tools over stdio or HTTP responses in JSON or SSE form. Resource/prompt MCP capabilities and OAuth are not implemented yet.

## Sessions and JSON output

Unless `--no-session` is used, every new invocation creates a mode-0600 JSONL file under the configured session directory. The first record is metadata (format version, ID, time, CWD, provider, model); subsequent records contain complete user, assistant, and tool-result messages. Each append is synced before continuing.

`--json` emits one JSON object per line. Current event types are `turn_start`, `text_delta`, `turn_end` (with token usage), `tool_start`, `tool_update`, `tool_end` (with a result), and `error`. The JSON stream is an event interface, not the on-disk session format.

## Extensions

Place `.lua` files or executable-plugin directories below either:

- `~/.notch/extensions` (or `$NOTCH_HOME/extensions`)
- `<cwd>/.notch/extensions`

Lua files are loaded directly from each extension directory. Executable manifests named `plugin.json` are discovered recursively. Extensions can register model tools, interactive slash commands, and agent hooks. Full examples and the wire protocol are in [docs/extensions.md](docs/extensions.md).
