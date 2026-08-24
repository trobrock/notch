# Notch contributor guide

## Project goals

Notch is a small, fast, extensible coding agent distributed as a single compiled Go binary. Preserve these priorities in this order:

1. **Simplicity** — keep the provider-independent agent loop and core APIs easy to understand. Prefer direct code and narrow interfaces over frameworks or speculative abstraction.
2. **Efficiency** — keep startup, memory, terminal rendering, and idle power use low. The TUI must remain event-driven: no idle polling, animation loops, or periodic work without a measured need.
3. **Extensibility** — put reusable mechanisms in core and optional policy or workflow behavior in tools, Lua extensions, executable JSON-RPC plugins, or MCP servers. Do not grow the built-in tool surface when a stable extension API is sufficient.

## Constraints

- Do not introduce a Node.js or npm runtime requirement.
- Keep the normal installation to one native executable. Optional external plugins and MCP servers may have their own runtimes.
- Keep provider-specific wire formats behind `internal/provider`; the agent loop uses `internal/model` types.
- Preserve durable, append-only JSONL sessions and backwards compatibility where practical.
- Treat model, tool, extension, and session text as untrusted terminal content. Preserve ANSI sanitization and display-width-safe Unicode handling.
- Keep the fullscreen TUI usable in tmux and modern terminals, with the composer pinned above the footer.

## Change guidance

- Prefer the smallest complete change that establishes a useful primitive.
- Add core services only when they cannot be implemented cleanly through existing extension boundaries.
- Avoid background goroutines, timers, and dependencies unless their lifecycle and cost are explicit.
- Add focused tests for parsing, session durability, provider streams, terminal input, and frame layout.
- Run `make check` and `make build` before considering a change complete.

## Key paths

- `cmd/notch`: composition root and CLI
- `internal/agent`: agent loop and context management
- `internal/model`: provider-neutral types
- `internal/provider`: provider adapters
- `internal/tui`: fullscreen event loop and renderer
- `internal/session`: JSONL session store
- `internal/extension`, `internal/luaext`, `plugin`: extension systems
- `internal/resources`: skills and command templates
