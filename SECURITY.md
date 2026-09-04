# Security policy

## Supported versions

Security fixes are applied to the latest released version and `main`. Older releases are not maintained as separate support branches.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use GitHub's private vulnerability reporting form:

https://github.com/trobrock/notch/security/advisories/new

Include the affected version, platform, impact, reproduction steps, and any suggested mitigation. Remove real API keys, OAuth tokens, session contents, and other private data. You can expect an acknowledgement within seven days, followed by updates as the report is investigated. Public disclosure should wait until a fix or coordinated disclosure plan is ready.

## Security boundaries

Notch executes enabled tools, Lua extensions, executable plugins, MCP servers, and provider-directed tool calls with the current user's privileges. It does not provide a sandbox or per-command approval UI. Workspace trust is a one-time gate for loading project-controlled `.notch` and `.agents` inputs plus root-level `AGENTS.md` and `AGENTS.local.md` instructions from the active worktree's canonical Git root. Trust is keyed by the canonical Git common directory and therefore extends to all linked worktrees in the repository; after trust, enabled project code and tool calls execute automatically. Review project inputs before trusting a workspace, install only extensions and MCP servers you trust, review commands before requesting destructive work, and avoid running Notch with elevated privileges.

Interactive runs prompt only when an untrusted workspace contains supported project inputs. Noninteractive runs skip project `.notch`/`.agents` inputs and AGENTS instructions unless trust was previously persisted; automation can opt in explicitly with `--trust-workspace`. `--safe` bypasses project trust and all project inputs for one run. These controls do not disable global extensions, installed packages, or model tools.

Executable plugins and stdio MCP servers receive a minimal child environment rather than the full Notch environment, excluding provider credentials and typical CI secrets. MCP variables must be selected explicitly with the server's `env` object. Its values—and remote HTTP `headers` values—can reference `${NAME}` variables from Notch's environment; config loading fails when a reference is unset or malformed. This reduces accidental credential exposure but is not a sandbox. Extension host process execution is cancellation-aware and retains at most 1 MiB from each of stdout and stderr while continuing to drain the child.

Human-facing fullscreen and line-mode output sanitizes untrusted model, tool, extension, and session text before display. The line-mode sanitizer is enabled only when its destination is a TTY and retains state across streamed model deltas; redirected line output, JSON event mode, and RPC remain raw machine-oriented streams.

Extension package installation runs no package scripts, rejects unsafe archive/filesystem entries, and locks Git sources to exact commits plus a tree digest. Those checks protect package-manager integrity; they do not make extension code safe. Review a package before `notch extensions install`, and inspect `notch extensions list` for modified or missing content.

Credentials belong in environment variables or Notch's mode-0600 credential store, never in issues, configuration examples, session excerpts, or commits. See [docs/providers.md](docs/providers.md) for credential storage details.

Release GitHub Actions are pinned to full commit SHAs. Build and publish run as separate least-privilege jobs, and published assets receive GitHub build-provenance attestations. Checksums and attestations still depend on GitHub and the repository's workflow security.
