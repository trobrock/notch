# Compaction and context

Notch can summarize older conversation context while preserving recent work. Automatic compaction is enabled by default, and manual compaction is available in the fullscreen UI:

```text
/compact
/compact Preserve exact test failures and unresolved file paths
```

Optional instructions add requirements to the fixed safety-focused summary prompt. Compaction sends older messages to the current provider for a summary (with no tools), inserts that summary as conversation context, and retains recent **complete** turns. Opaque provider replay signatures are excluded from the summary input. The embedded conversation JSON is capped at **512 KiB** so HTTP body limits cannot be reached merely because a long session contains large replay metadata; when necessary, Notch preserves the beginning and newest old context and inserts an explicit middle-omission marker. This cap applies to the embedded conversation data, not the provider's complete wire request.

The split is chosen at a normal user prompt so a tool result is not orphaned from its tool call. If there is not enough old context, Notch reports `nothing to compact`. Transient provider failures use the normal bounded retry and retry-after policy, but a summary request is never retried after any output has arrived.

For durable sessions, the summary and complete replacement context are appended as a synced JSONL `compaction` record. `--continue` reconstructs the effective context from that record, so both the summary and retained recent turns survive restart. Under `--no-session`, compaction changes context only in memory. Every started compaction emits one terminal event: failures and cancellations are reported as aborted and leave the effective conversation unchanged.

Sessions are loaded defensively. If the only malformed record is an unterminated final JSONL record and every preceding record is valid, Notch treats it as a torn write, truncates and syncs that tail, and resumes the valid prefix. A valid unterminated final record is normalized by appending a newline. Interior corruption and malformed newline-terminated records remain errors. Session listing, `/resume`, and latest-session selection skip damaged files so one bad session does not hide healthy sessions; directly resuming a damaged file still reports its error.

## Configuration

The config shape matches Pi and uses camelCase inside `compaction`:

```json
{
  "context_window": 272000,
  "compaction": {
    "enabled": true,
    "reserveTokens": 16384,
    "keepRecentTokens": 20000
  }
}
```

- `enabled` controls automatic compaction; `/compact` remains available when it is false.
- `reserveTokens` reserves room before the model's context limit. The current default is **16,384**.
- `keepRecentTokens` is the target amount of recent context retained whole after summarization. The current default is **20,000**; a single complete recent turn may exceed it.
- `context_window` overrides the inferred model context size and is useful for local or unrecognized models.

Project copies of these settings apply only in a trusted workspace. `context_window` and compaction settings are project-eligible; session and model-cache paths are fixed below the XDG data root and cannot be configured in JSON.

## Threshold and context indicator

Before each provider request, Notch estimates the system prompt, tool schemas, and conversation context, incorporating provider-reported input usage when available. Text added since the latest provider count uses a conservative three UTF-8 bytes per token; the next successful provider response re-anchors the estimate. Automatic compaction is attempted when:

```text
estimated context tokens >= context window - reserveTokens
```

The fullscreen footer shows current estimated use as a percentage and window size, for example `18.2%/272k (auto)`. `(auto)` means automatic compaction is enabled, not that compaction is currently running.

When `context_window` is not set, Notch first uses matching model-registry metadata from the provider cache or bundled catalog. If the registry has no model-specific value, current fallbacks are:

| Provider/model | Context window |
| --- | ---: |
| `openai-codex` (Codex models) | 272,000 |
| `anthropic` with a `claude-` model ID | 1,000,000 |
| other `anthropic` model IDs | 200,000 |
| `openrouter` | 128,000 |
| `openai` and other fallback cases | 128,000 |

Provider limits can change and model/account availability varies. Runtime `/model` selection immediately applies discovered context metadata when present. Override `context_window` when the service's real limit differs; an explicit value wins over registry metadata, and Notch's inferred value does not alter the provider's limit.

See [sessions and `/new`](tui.md#commands-and-thinking-level) and the [architecture](architecture.md#agent-loop).
