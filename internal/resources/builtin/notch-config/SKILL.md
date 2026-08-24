---
name: notch-config
description: Configure Notch providers, models, thinking, themes, compaction, sessions, resources, MCP servers, and authentication. Use when the user asks to set up or change Notch itself.
---
# Configure Notch

Use this skill when changing Notch's own runtime setup. Prefer the smallest scoped change and preserve unrelated settings.

## Configuration precedence

Notch resolves values in this order, with later layers winning:

1. compiled defaults;
2. `$NOTCH_HOME/config.json`, or `~/.notch/config.json` when `NOTCH_HOME` is unset;
3. `<cwd>/.notch/config.json`;
4. `NOTCH_PROVIDER`, `NOTCH_MODEL`, and `NOTCH_THINKING_LEVEL`;
5. CLI flags such as `--provider` and `--model`.

Use project config for repository-specific behavior and user config for defaults shared by all projects. Use environment variables for temporary shell or CI overrides. Provider and model overrides are independent.

Before editing, read every existing applicable config file. Do not replace unrelated keys. Ask before changing user-global configuration when a project-local change would work.

## Main config shape

```json
{
  "provider": "openai-codex",
  "model": "gpt-5.6-sol",
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
  "system_prompt": "You are a coding agent.",
  "mcp_config": "/home/me/.notch/mcp.json",
  "extension_dirs": ["/home/me/.notch/extensions", "/work/project/.notch/extensions"],
  "skill_dirs": ["/home/me/.notch/skills", "/work/project/.notch/skills"],
  "prompt_dirs": ["/home/me/.notch/prompts", "/work/project/.notch/prompts"],
  "theme_dirs": ["/home/me/.notch/themes", "/work/project/.notch/themes"],
  "session_dir": "/home/me/.notch/sessions"
}
```

Empty scalar values in later files do not erase earlier values. A non-empty directory array replaces the complete earlier array; it is not appended. `context_window: 0` lets the provider/model default apply.

The model registry ships with an offline fallback and refreshes stale selected-provider data from provider model-list APIs on startup or when `/model` is opened. `model_refresh_hours` controls staleness; no polling timer runs. Use `notch models [provider]` to list cached/discovered models, `notch models --refresh [provider]` to force discovery, and `/model refresh` to force it in the fullscreen selector. `model_cache` relocates the mode-0600 JSON cache.

Valid thinking levels are `off`, `minimal`, `low`, `medium`, `high`, and `xhigh`. Built-in themes are `dark`, `dracula`, and `catppuccin-mocha`. `/thinking LEVEL` and `/theme NAME` change only the running process.

Tool exposure is controlled per process with `--tools read,grep`, `--exclude-tools bash,write`, `--no-builtin-tools`, or `--no-tools`. The strict allowlist applies across built-in, extension, and MCP tools; unknown names fail startup. These are CLI controls rather than persistent config keys.

Custom themes are direct JSON files in `theme_dirs`, defaulting to user and project `.notch/themes` directories. Each file has an optional `name`, optional `base` (default `dark`), optional `vars`, and a `colors` object whose final values are `#RRGGBB`. Project files load after user files. Preserve unrelated roles by using a base and changing only requested colors. See `docs/themes.md` or the `examples/themes/rose-pine.json` shape before authoring one; invalid roles, colors, variables, and inheritance cycles cause that theme to be skipped.

## Providers and authentication

Supported provider names are:

- `openai-codex` for a ChatGPT subscription;
- `anthropic` for Anthropic API keys or Claude Pro/Max OAuth;
- `openrouter`;
- `openai` for OpenAI Responses-compatible APIs, including local Ollama setups.

Model IDs are passed through to the provider. Do not guess that an account has access to a model; preserve a known working model unless the user asks to change it.

Use:

```sh
notch login openai-codex
notch login anthropic
notch login openrouter
notch logout PROVIDER
```

API-key alternatives are `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `OPENROUTER_API_KEY`. OAuth credentials are stored in `$NOTCH_HOME/auth.json` or `~/.notch/auth.json` with mode `0600`.

Never put secrets in `config.json`, project files, examples, or version control. Do not print access or refresh tokens. Notch configures credentials separately from normal config.

For a local OpenAI-compatible endpoint, set `provider` to `openai`, set `base_url` to the server root, and choose an installed tool-capable model. Clear a stale provider-specific `base_url` when switching back to a hosted provider.

## Skills and commands

Notch discovers shared resources without creating their directories:

- `~/.agents/skills` and `<cwd>/.agents/skills`;
- `~/.agents/commands` and `<cwd>/.agents/commands`.

It also reads configured `skill_dirs` and `prompt_dirs`, defaulting to user and project `.notch` directories. Skills are either direct Markdown files or `name/SKILL.md` directories. Command templates are direct Markdown files. Project `.agents` resources have final precedence.

A command template may use:

```markdown
---
description: Review changes
argument-hint: "[focus]"
---
Review the changes. Focus: $ARGUMENTS
```

## MCP

The default MCP file is `$NOTCH_HOME/mcp.json` or `~/.notch/mcp.json`:

```json
{
  "mcpServers": {
    "local": {
      "command": "/absolute/path/to/server",
      "args": ["--root", "/work"],
      "env": {"LOG_LEVEL": "warn"}
    },
    "remote": {
      "url": "https://example.test/mcp",
      "headers": {"Authorization": "Bearer REDACTED"}
    }
  }
}
```

Use the `env` object for stdio-server secrets. Remote HTTP headers are currently literal and do not expand environment variables, so avoid writing a token unless the user explicitly accepts a protected local config file. Notch supports stdio and Streamable HTTP tools. MCP OAuth, resources, prompts, and app UI are not currently built in.

## Sessions

Sessions default to `~/.notch/sessions` and are durable JSONL files. Use `--continue` for the latest, `--resume ID` for a specific session, `/resume` for the fullscreen picker, and `--no-session` for no persistence. Changing `session_dir` isolates future session discovery.

## Validation

After editing:

1. parse JSON with `jq . FILE` when available;
2. check file permissions when credentials or auth paths are involved;
3. launch `notch --help` or a no-session smoke test;
4. report which layer was changed and whether a restart is required.

Use `notch --init` only to create starter files. Do not run it over an existing setup without inspecting what already exists.
