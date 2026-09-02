---
name: notch-config
description: Configure Notch providers, models, thinking, themes, compaction, sessions, resources, MCP servers, and authentication. Use when the user asks to set up or change Notch itself.
---
# Configure Notch

Use this skill when changing Notch's own runtime setup. Prefer the smallest scoped change and preserve unrelated settings.

## Workspace trust and configuration precedence

Notch loads project inputs from the active worktree's canonical Git root and keys trust by the repository's canonical Git common directory, so one trust decision applies to all linked worktrees. Outside Git, it uses the canonical current directory for both. Project `.notch` and `.agents` inputs are loaded only after one-time persisted trust. An interactive run prompts only when supported project inputs exist; a noninteractive untrusted run skips them. Use `--trust-workspace` to persist trust in automation and `--safe` to bypass project inputs for one invocation. Trust is not per-command approval: enabled tools and extensions execute automatically.

Notch resolves values in this order, with later layers winning:

1. compiled defaults;
2. `$XDG_CONFIG_HOME/notch/config.json`, or `~/.config/notch/config.json` when unset;
3. `<workspace-root>/.notch/config.json`, only when trusted;
4. `NOTCH_PROVIDER`, `NOTCH_MODEL`, `NOTCH_EXPLORE_MODEL`, and `NOTCH_THINKING_LEVEL`;
5. CLI flags such as `--provider` and `--model`.

Use trusted project config for repository-specific behavior and user config for defaults shared by all projects. `base_url` is global-only. Auth, MCP-auth, session, and model-cache paths are fixed below `$XDG_DATA_HOME/notch` (default `~/.local/share/notch`) and JSON keys attempting to configure them are ignored. Standalone `notch login`, `logout`, `auth`, and `models` commands load global configuration only. Use environment variables for temporary shell or CI overrides. Provider, parent model, and explore model overrides are independent.

Before editing, read every existing applicable config file. Do not replace unrelated keys. Ask before changing user-global configuration when a project-local change would work.

## Main config shape

```json
{
  "provider": "openai-codex",
  "model": "gpt-5.6-sol",
  "explore_model": "openai-codex/gpt-5.4-mini",
  "base_url": "",
  "max_tokens": 8192,
  "theme": "dark",
  "thinking_level": "medium",
  "cache_retention": "short",
  "presets": {
    "f1": {"provider": "openai-codex", "model": "gpt-5.6-sol", "thinking_level": "high"}
  },
  "mouse": true,
  "context_window": 0,
  "model_refresh_hours": 24,
  "auto_update": true,
  "compaction": {
    "enabled": true,
    "reserveTokens": 16384,
    "keepRecentTokens": 20000
  },
  "system_prompt": "You are a coding agent.",
  "mcp_config": "/home/me/.config/notch/mcp.json",
  "extension_dirs": ["/home/me/.config/notch/extensions", "/work/project/.notch/extensions"],
  "skill_dirs": ["/home/me/.config/notch/skills", "/work/project/.notch/skills"],
  "prompt_dirs": ["/home/me/.config/notch/prompts", "/work/project/.notch/prompts"],
  "theme_dirs": ["/home/me/.config/notch/themes", "/work/project/.notch/themes"]
}
```

Empty scalar values in later files do not erase earlier values. A non-empty directory array replaces the complete earlier array; it is not appended. `context_window: 0` lets the provider/model default apply. `explore_model` (or `NOTCH_EXPLORE_MODEL`) is the provider-qualified default for `explore_codebase`. Explicit batch and per-task overrides still win; normally omit them. If the configured model is unavailable, use `list_models` for that provider and retry once with a returned closest-family/capability model rather than guessing an ID.

The model registry ships with an offline fallback and refreshes stale selected-provider data from provider model-list APIs on startup or when `/model` is opened. `model_refresh_hours` controls staleness; no polling timer runs. Use `notch models [provider]` to list cached/discovered models, `notch models --refresh [provider]` to force discovery, and `/model refresh` to force it in the fullscreen selector. The mode-0600 JSON cache has a fixed path below the XDG data root. Interactive release builds install newer verified stable releases at most once per day by default and relaunch immediately before opening a session; set `auto_update` to `false` in the global config to disable this. Project config cannot control executable updates.

Valid thinking levels are `off`, `minimal`, `low`, `medium`, `high`, and `xhigh`. `cache_retention` accepts `none`, `short`, or `long` and defaults to `short`; long requests extended provider retention where supported, while compaction summaries always disable cache writes. Built-in themes are `dark`, `dracula`, and `catppuccin-mocha`. `/thinking LEVEL` and `/theme NAME` change only the running process. Fullscreen `presets` map `f1` through `f9` to provider/model/thinking combinations; omitted fields preserve current values, global and trusted project maps merge by key, and applying one changes only the running process. `mouse` defaults to `true`; set it to `false` to disable TUI mouse capture and restore terminal-native selection/scrolling.

Tool exposure is controlled per process with `--tools read,grep`, `--exclude-tools bash,write`, `--no-builtin-tools`, or `--no-tools`. The strict allowlist applies across built-in, extension, and MCP tools; unknown names fail startup. These are CLI controls rather than persistent config keys.

Custom themes are direct JSON files in `theme_dirs`, defaulting to user and project `.notch/themes` directories. Each file has an optional `name`, optional `base` (default `dark`), optional `vars`, and a `colors` object whose final values are `#RRGGBB`. Project files load after user files. Preserve unrelated roles by using a base and changing only requested colors. See `docs/themes.md` or the `examples/themes/rose-pine.json` shape before authoring one; invalid roles, colors, variables, and inheritance cycles cause that theme to be skipped.

## Providers and authentication

Supported provider names are:

- `openai-codex` for a ChatGPT subscription;
- `anthropic` for Anthropic API keys (`ANTHROPIC_API_KEY`);
- `anthropic-claude-code` for a Claude Pro/Max OAuth subscription;
- `openrouter`;
- `openai` for OpenAI Responses-compatible APIs, including local Ollama setups.

Model IDs are passed through to the provider. Do not guess that an account has access to a model; preserve a known working model unless the user asks to change it.

Use:

```sh
notch login openai-codex
notch login anthropic-claude-code
notch login openrouter
notch logout PROVIDER
```

API-key alternatives are `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `OPENROUTER_API_KEY`. OAuth credentials are stored in `$XDG_DATA_HOME/notch/auth.json` or `~/.local/share/notch/auth.json` when unset with mode `0600`.

Never put secrets in `config.json`, project files, examples, or version control. Do not print access or refresh tokens. Notch configures credentials separately from normal config.

For a local OpenAI-compatible endpoint, set `provider` to `openai`, set the global-only `base_url` in user config to the server root, and choose an installed tool-capable model. Clear a stale provider-specific `base_url` when switching back to a hosted provider.

## Skills and commands

Notch discovers shared resources without creating their directories:

- `~/.agents/skills` and trusted `<workspace-root>/.agents/skills`;
- `~/.agents/commands` and trusted `<workspace-root>/.agents/commands`.

It also reads configured `skill_dirs` and `prompt_dirs`, defaulting to user paths and, when trusted, project `.notch` directories. Skills are either direct Markdown files or `name/SKILL.md` directories. Command templates are direct Markdown files. Trusted project `.agents` resources have final precedence; untrusted/noninteractive and `--safe` runs omit them.

A command template may use:

```markdown
---
description: Review changes
argument-hint: "[focus]"
---
Review the changes. Focus: $ARGUMENTS
```

## MCP

The default MCP file is `$XDG_CONFIG_HOME/notch/mcp.json` or `~/.config/notch/mcp.json` when unset:

```json
{
  "mcpServers": {
    "local": {
      "command": "/absolute/path/to/server",
      "args": ["--root", "/work"],
      "env": {"LOG_LEVEL": "warn", "TOKEN": "${LOCAL_MCP_TOKEN}"}
    },
    "remote": {
      "url": "https://example.test/mcp",
      "auth": "oauth"
    }
  }
}
```

The MCP file is static configuration under the config root, so do not store literal secrets in `env` or HTTP `headers`. Both objects support `${NAME}` references that resolve from Notch's environment when the file loads; an unset or malformed reference is an error, and `$$` produces a literal `$`. Use the `env` object to explicitly select every stdio-server secret or other non-baseline variable. Stdio MCP children receive only a minimal inherited environment (`PATH`, home/user, temporary-directory, locale, terminal, and SSH-agent basics, plus required Windows process variables); provider keys, token variables, and CI secrets are not inherited. Resolved `env` values are added to that baseline. Remote HTTP headers support the same strict interpolation. Prefer `"auth": "oauth"` for compatible protected servers, then run `notch mcp login NAME`; status and logout are available through `notch mcp status` and `notch mcp logout NAME`. OAuth credentials are globally stored at mode `0600` and bound to the exact server URL. Notch supports stdio and Streamable HTTP tools, but not MCP resources, prompts, elicitation, sampling, or app UI.

## Sessions

Sessions default to `~/.local/share/notch/sessions` and are durable JSONL files. Use `--continue` for the latest valid session from the current working directory, `--resume ID` for a specific session, `/resume` for the fullscreen picker, and `--no-session` for no persistence. A malformed unterminated final record is treated as a torn write and truncated only when every preceding record is valid; damaged sessions are skipped by list/latest discovery instead of hiding healthy sessions. The session path is fixed below the XDG data root; select a separate absolute `XDG_DATA_HOME` to isolate it.

## Validation

After editing:

1. parse JSON with `jq . FILE` when available;
2. check file permissions when credentials or auth paths are involved;
3. launch `notch --help` or a no-session smoke test;
4. report which layer was changed and whether a restart is required.

Use `notch --init` only to create starter files. Do not run it over an existing setup without inspecting what already exists.
