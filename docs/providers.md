# Providers and authentication

Notch uses native Go HTTP adapters rather than provider SDKs. Select an adapter with `--provider`/`-p` or the `provider` configuration field.

| Provider | API | Authentication |
| --- | --- | --- |
| `openai-codex` | ChatGPT backend's native Codex Responses endpoint | ChatGPT Plus/Pro OAuth |
| `openrouter` | OpenRouter OpenAI-compatible Chat Completions | `OPENROUTER_API_KEY` or OpenRouter OAuth |
| `anthropic` | Native Anthropic Messages | `ANTHROPIC_API_KEY` or Claude Pro/Max OAuth |
| `openai` | Native OpenAI Responses | `OPENAI_API_KEY`, or no key for a configured local server such as Ollama |

Model IDs are passed through to the selected service. Availability is determined by the account and provider; a documented example is not a guarantee that a model is enabled for every account.

## OAuth commands

```sh
notch login openai-codex
notch login anthropic
notch login openrouter

notch logout PROVIDER
notch auth status
```

`notch login PROVIDER` opens a browser authorization flow and also prints the authorization URL. Login is supported for `openai-codex`, `anthropic`, and `openrouter`; the regular `openai` provider uses an API key instead. `logout` removes that provider's stored credential. `auth status` reports credentials in Notch's store (not API keys currently supplied through the environment).

Notch stores credentials at `~/.notch/auth.json`, or `$NOTCH_HOME/auth.json` when `NOTCH_HOME` is set. The file is written with mode `0600`; when Notch must create its parent directory, it uses mode `0700`. Treat the file as a secret: it can contain access and refresh tokens. OAuth credentials that have a refresh token and are within five minutes of expiry are refreshed automatically before use, and the replacement is saved. OpenRouter's OAuth exchange produces a non-expiring API key rather than a refreshable token.

## Importing credentials from Pi

A one-time import can copy Pi's provider credentials into Notch:

```sh
# Default source: ~/.pi/agent/auth.json
notch auth import-pi

# Or provide another Pi auth file.
notch auth import-pi /path/to/auth.json
```

The import merges credentials into Notch's own auth store and preserves the ChatGPT account ID needed by Codex. It does not create ongoing synchronization and does not import sessions, config, model registries, or extensions. The import is performed directly by the Notch binary, so Pi and Node.js do not need to be installed or running. After verifying the imported login, Pi's file can remain untouched; use `notch logout PROVIDER` to remove the Notch copy.

## Provider details

### `openai-codex`

Use this provider for a ChatGPT Plus or Pro subscription:

```sh
notch login openai-codex
notch --provider openai-codex --model gpt-5.6-terra
```

This is not the public OpenAI API adapter. It sends native Responses requests to ChatGPT's Codex endpoint and uses the OAuth account scope captured during login. A real subscription request producing text and successfully calling the built-in `read` tool has been tested after temporarily importing a Pi credential. That verifies the provider path and the one-time import path; it does not imply every model or account tier has been tested.

### `openrouter`

Use either an environment key or browser login:

```sh
export OPENROUTER_API_KEY=...
notch --provider openrouter --model anthropic/claude-sonnet-4.5

# Alternative:
notch login openrouter
notch --provider openrouter --model anthropic/claude-sonnet-4.5
```

`OPENROUTER_API_KEY` takes precedence over a stored OpenRouter login. This adapter uses OpenRouter's streaming Chat Completions API, including tool calls and tool results; it does not route through the Responses adapter. A real request producing text and successfully calling `read` has been tested with `OPENROUTER_API_KEY`. The OAuth implementation has automated coverage, but that real-service test used the environment key.

### `anthropic`

Use an API key or Claude subscription OAuth:

```sh
export ANTHROPIC_API_KEY=sk-ant-...
notch --provider anthropic --model claude-sonnet-4-5

# Alternative for Claude Pro/Max:
notch login anthropic
notch --provider anthropic --model claude-sonnet-4-5
```

The adapter sends native streaming Messages requests and supports Anthropic tool-use blocks. When a stored Anthropic OAuth credential exists, Notch uses it in preference to `ANTHROPIC_API_KEY`; remove it with `notch logout anthropic` to return to the environment key.

Anthropic OAuth login, refresh, request headers, and provider behavior have implementation and automated tests. A locally imported Anthropic token was stale, however, so a real Claude Pro/Max subscription call has **not yet been verified**. Do not interpret the automated coverage as a successful production subscription test.

### `openai` and local Ollama

The `openai` adapter calls the public native Responses API:

```sh
export OPENAI_API_KEY=sk-...
notch --provider openai --model gpt-5
```

With a non-empty `base_url`, it calls `<base_url>/v1/responses` and does not require `OPENAI_API_KEY`. This is the adapter used for local Ollama:

```json
{
  "provider": "openai",
  "model": "qwen3.5:9b",
  "base_url": "http://localhost:11434"
}
```

The Ollama release and model must support the Responses endpoint and the tool-calling behavior required by the prompt. A Chat Completions-only local server is not sufficient for this adapter.
