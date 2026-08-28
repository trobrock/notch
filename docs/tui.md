# Fullscreen terminal UI

Notch's interactive default is a fullscreen, event-driven terminal UI. It is selected when both stdin and stdout are TTYs and none of `--print`/`-p`, `--no-tui`, `--json`, or `--mode rpc` is set. Positional prompt text is submitted immediately as the initial message while the TUI remains open. If either stream is redirected or piped, Notch uses its buffered line-oriented interface instead. `--print`/`-p` processes a positional prompt and exits; `--no-tui` selects the line-oriented fallback explicitly, and JSONL output also avoids the fullscreen UI.

The fullscreen UI puts the terminal in raw mode and uses the alternate screen, restoring the previous screen and terminal mode on exit. Bracketed paste is always enabled; SGR mouse reporting is enabled when mouse capture is active (the default). This lets Notch own transcript scrollback and text selection consistently in direct terminals, tmux, and nested tmux sessions (including remote Termius sessions), provided each tmux layer has mouse forwarding enabled (`set -g mouse on`). Drag to select rendered text, then press `Ctrl-Y` to copy it through OSC 52 and an available platform clipboard helper.

Set `"mouse": false` in user or project `config.json` to restore terminal-native mouse selection instead of Notch's wheel scrolling and drag selection. The alternate-screen TUI still does not provide terminal-native scrollback; use `--no-tui` when that is required. With capture enabled, copying uses `tmux load-buffer -w` inside tmux, emits OSC 52, and also tries `pbcopy`, `wl-copy`, `xclip`, `xsel`, PowerShell, or `clip.exe` when available. Clipboard acceptance cannot be acknowledged by OSC 52, and remote/tmux installations may require clipboard passthrough configuration.

## Pi-style layout

The transcript is the upper, scrollable part of the screen. Its presentation mirrors Pi:

- user messages are full-width background boxes with one-cell horizontal padding and blank padding rows above and below their content;
- assistant responses are unboxed prose, offset by one cell rather than presented as chat bubbles;
- tool calls/results are full-width cards whose background reflects pending, successful, or failed status; long-running official delegation tools keep one live progress line and replace its elapsed/count status instead of accumulating heartbeat lines;
- notices, errors, and extension interactions use semantic theme colors.

Exactly one normal blank row separates transcript entries, independent of the internal padding in user and tool cards. Markdown blocks likewise use one blank row of separation instead of preserving arbitrary runs of source blank lines. This keeps user, assistant, and tool spacing consistent while streaming and after resize.

New activity returns the viewport to the bottom. A multiline composer is pinned below the transcript, grows to eight visible rows, and keeps the cursor in view when input is longer. Full-width rules above and below the editor use the current thinking level's color.

The footer has two lines. The first shows the abbreviated working directory and Git branch. The second combines activity/session and last-turn provider token usage with a context indicator on the left, and provider/model/thinking level on the right. After `run_subagent` or `explore_codebase`, it adds the separate delegated token total and elapsed time; parallel exploration reports batch elapsed time rather than summed child durations. The context indicator is a percentage and context-window size, with `(auto)` when automatic compaction is enabled; for example `18.2%/272k (auto)`.

The UI redraws itself for terminal resizes (`SIGWINCH`), rewrapping content to the new width and preserving a valid composer cursor. See [themes](themes.md) and [compaction](compaction.md) for the associated settings.

On startup, the transcript shows compact `[Skills]` and `[Commands]` sections for loaded skills, prompt templates, and extension commands. Resource names are sorted, and the sections appear after restored conversation history when resuming a session. Built-in commands remain available through `/help` without cluttering the startup list.

## Keys

| Key | Action |
| --- | --- |
| `Enter` | Submit when idle; queue a steering message while the model is active |
| `Alt-Enter` | Queue a follow-up while active; submit normally when idle |
| `Ctrl-J` | Insert a newline |
| `Shift-Enter` | Insert a newline (Notch enables enhanced keyboard reporting when supported) |
| Left/Right, `Ctrl-B`/`Ctrl-F` | Move by character |
| `Alt-Left`/`Alt-Right` | Move by word; `Alt-B`/`Alt-F` encodings are also accepted |
| Home/End, `Ctrl-A`/`Ctrl-E` | Move to start/end of the current logical line |
| Up/Down | Select slash suggestions when open; otherwise move between lines or submitted history |
| Backspace/Delete | Delete before/at the cursor |
| `Ctrl-W` | Delete the previous word |
| `Ctrl-U`/`Ctrl-K` | Delete to the start/end of the logical line |
| Tab | Accept the selected slash suggestion when open; otherwise insert indentation |
| `Shift-Tab` | Cycle `off` → `minimal` → `low` → `medium` → `high` → `xhigh` |
| Escape | Close slash help/completion or cancel an extension/session selector |
| `PageUp`/`PageDown` | Scroll the transcript by roughly one viewport |
| Mouse drag | Select rendered text |
| `Ctrl-Y` | Copy the current TUI selection |
| Mouse wheel | Scroll the transcript by three rendered lines |
| `Ctrl-C` | Cancel active model/command work; otherwise clear the composer; if already empty, exit |
| `Ctrl-D` | Exit when the composer is empty; otherwise delete at the cursor |

Notch enables the Kitty keyboard protocol in native Ghostty, Kitty, and WezTerm sessions. Inside tmux it uses xterm `modifyOtherKeys`, matching Pi's fallback when tmux does not expose Kitty protocol flags. Both modes are restored on exit. This enhanced reporting is also how Notch distinguishes `Shift-Tab`; terminals/multiplexers that do not report it can send the conventional back-tab sequence (`CSI Z`) as a fallback. On terminals without enhanced keyboard reporting, `Shift-Enter` may still be indistinguishable from Enter; `Ctrl-J` always inserts a newline. Common xterm and rxvt modified-key encodings are also handled. Standard F1–F9 sequences are accepted for configured presets.

Function-key presets are configured under `presets` using keys `f1` through `f9`:

```json
{
  "presets": {
    "f1": {"provider": "openai-codex", "model": "gpt-5.6-sol", "thinking_level": "high"},
    "f2": {"provider": "anthropic", "model": "claude-sonnet-4-5", "thinking_level": "medium"}
  }
}
```

Pressing a configured key changes subsequent turns and does not rewrite config. Missing provider, model, or thinking fields preserve the current value. Presets cannot be applied while model or command work is active; unconfigured function keys are ignored.

While a model run is active, Enter clears the composer and queues its text as steering for the next safe turn boundary, after current tool results. Alt-Enter queues a follow-up for after the run would otherwise settle. Pending messages appear directly above the composer; they become labeled user transcript entries when delivered. Queues are first-in/first-out and deliver one message per model turn. Slash templates and skills are expanded before queueing; built-in or unknown slash commands are rejected while streaming.

Pasted text is accepted as one bracketed-paste event, so multiline content is inserted rather than accidentally submitted. Ordinary UTF-8 input and incomplete terminal sequences are decoded incrementally.

## Commands and thinking level

Typing `/` opens an automatically filtered menu above the composer. It includes built-ins, skills, command templates, and executable/Lua extension commands with their descriptions and argument hints. Up/Down changes the selection, Tab accepts it, and Enter accepts a partial selection or executes an exact command. Escape dismisses the menu until the composer text changes. `/help` opens the same menu with every command visible.

The fullscreen built-ins are:

- `/model` (also `/models` or `/provider`) opens provider then model selectors; add `refresh` to force provider discovery;
- `/thinking` reports the current level; `/thinking LEVEL` changes it;
- `/theme` lists built-in and custom themes; `/theme NAME` changes the current process only;
- `/compact [instructions]` summarizes older context while retaining recent complete turns;
- `/new` starts a clean conversation and clears the transcript and submitted-input history;
- `/resume` opens a saved-session selector showing modification time, original directory, model, preview, and ID suffix;
- `/clear` clears only the displayed transcript;
- `/help` opens command help, while `/tools`, `/skills`, `/exit`, and `/quit` retain their usual meanings.

The provider selector puts the current provider first. The model selector puts the current model first, shows known context size and reasoning support, caps visible rows, and filters case-insensitively as you type. A successful change preserves conversation context, affects subsequent turns and `/new` sessions, appends a model-change marker to durable sessions, and does not rewrite config files. Models come from a 24-hour cache refreshed through provider APIs when available, with an embedded offline fallback. Codex currently uses that bundled fallback because its subscription endpoint does not expose model listing.

`thinking_level` accepts `off`, `minimal`, `low`, `medium`, `high`, or `xhigh` and defaults to `medium`. `Shift-Tab` and `/thinking LEVEL` update subsequent provider requests, including later model turns; they do not rewrite the global/project config or alter a request already in flight. Provider adapters transmit the selected reasoning setting in their native format. Actual support and accepted effort may differ by provider and model, and a service may reject, clamp, or otherwise reinterpret a level. See [providers](providers.md#reasoning-and-thinking).

When sessions are enabled, `/new` creates a distinct durable session, switches the agent to it, and closes the old session after a successful switch. `/resume` restores the selected session's effective context, transcript, and submitted-input history, then continues appending to that file with the currently configured provider and model. With `--no-session`, no file is created, `/new` resets only memory, and `/resume` is unavailable. See [compaction and context](compaction.md) for the separate `/compact` behavior.

## Extension UI integration

Lua `notch.ui.input` / `notch.ui.select` and executable-plugin `host.ui.input` / `host.ui.select` calls are integrated into the fullscreen event loop rather than reading the terminal independently:

- `Input` displays a dedicated question block with a bold prompt, placeholder, and keyboard hints, then temporarily uses the composer. Enter accepts the text, or the placeholder when left empty. The normal movement and editing keys work, including multiline insertion.
- `Select` displays a dedicated question block with a highlighted current option and descriptions on separate muted lines. A bounded option window keeps long lists usable. Typing reveals a filter, Up/Down changes the highlighted match, Backspace/Ctrl-U edits the filter, and Enter accepts it.
- `Ctrl-C` (or Escape) cancels the current extension prompt. Concurrent requests are queued, and context cancellation removes or closes them.
- Notifications become transcript notices or errors.

In `--no-tui` or pipe fallback mode these APIs use ordinary line prompts and numbered selection instead.

## Transcript rendering

User and assistant text is Markdown-aware. The renderer supports:

- ATX and setext headings;
- bold and italic emphasis;
- inline code and indented or fenced code blocks (with whitespace retained and tabs expanded consistently);
- labeled links and autolinks, with an explicit URL shown for labeled links;
- ordered, unordered, and nested lists;
- blockquotes and thematic rules;
- Markdown hard line breaks, escapes, and character entities.

Headings, links and URLs, code, quote bars, rules, and list bullets use semantic theme styles; bold, italic, and link underline use terminal attributes. User Markdown keeps the user's full-width background across inline style resets, while assistant Markdown remains unboxed. Fenced-code language tags do not enable syntax highlighting. Raw HTML is shown literally rather than interpreted. Extended Markdown is intentionally limited: tables are not laid out as tables, images are not displayed, and extensions such as task lists and strikethrough have no special rendering.

Wrapping uses terminal display cells rather than bytes, so wide Unicode, emoji, combining characters, and ordinary words wrap against the actual available width. Words move intact when possible; overlong tokens and verbatim code are split safely at rune boundaries. The renderer remains valid at very narrow widths and while Markdown is incomplete during streaming.

When thinking is enabled, a static `● Thinking…` row appears while the model has not produced visible content. If the provider streams a reasoning summary, it becomes a muted, italic, Markdown-aware `◆ Thinking` section and remains in session history; otherwise the temporary row disappears when answer text or a tool call arrives. OpenAI Responses/Codex summaries, Anthropic thinking blocks, and OpenRouter reasoning deltas are supported, but availability still depends on the selected model and provider. Notch only displays reasoning content returned by the provider and does not infer or manufacture it. Thinking deltas use the same one-shot render coalescing as answer text, with no idle animation or polling.

Tool cards use `●`, `✓`, and `✗` for pending, successful, and failed calls respectively, together with distinct state-colored backgrounds. Arguments are summarized as compact `key=value` pairs: useful keys are prioritized, large content/edit bodies are omitted, and long values or summaries are shortened. The summary stays on the title row when it fits and otherwise wraps below it. Output appears below a `│` bar in muted text. Updates and successful results retain at most eight lines, errors at most sixteen, and output text is capped at 2,000 runes with an ellipsis or omitted-line count. There is currently no expand control for the shortened content.

Model, tool, label, argument, URL, notice, extension, and session text is sanitized in the fullscreen renderer and before line-mode writes to a TTY: ANSI/CSI/OSC and related terminal controls are removed, with line-mode streaming sanitizer state retained across model deltas. Only renderer-generated styling sequences reach the fullscreen display. Redirected line output, JSON event mode, and RPC are not sanitized, preserving raw machine-oriented streams.

## Rendering and idle behavior

The implementation is event-driven. A goroutine blocks in terminal input, while the main loop blocks waiting for input, model and extension events, context cancellation, or `SIGWINCH`. There is no idle ticker and no periodic terminal-size polling.

During model streaming, the first pending text delta starts a **one-shot 33 ms timer**. Deltas arriving in that window are coalesced; the timer exists only while text is pending, and ordered non-text events flush that text immediately. This is not a continuous 30 FPS render loop.

Transcript rendering is cached per entry. Markdown cache keys include the source text, available display width, base card style, and complete theme, so unchanged text is reused while a resize, streamed text, user/assistant style change, or runtime theme change rebuilds it. Plain tool/notice wrapping is similarly reused for unchanged text and width. The screen renderer compares the desired frame with the previous successful frame, addresses only changed rows, and assembles all output for one render into a single buffered write. A resize invalidates the frame, and actual events still incur layout/frame work; the design avoids idle CPU and unnecessary terminal writes rather than claiming rendering is free.

## Current gaps

The fullscreen UI currently has no:

- terminal table layout, image display, syntax highlighting, or special rendering for Markdown extensions such as task lists and strikethrough;
- general mouse interaction beyond text selection;
- configurable keybindings;
- inline mode that preserves output in the normal screen buffer;
- expand/collapse control for shortened tool output.

It also does not add an interactive session tree. Notch intentionally has no per-command approval prompts; trusted tool execution is automatic. Use `--no-tui` when alternate-screen operation is undesirable.
