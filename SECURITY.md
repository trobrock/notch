# Security policy

## Supported versions

Security fixes are applied to the latest released version and `main`. Older releases are not maintained as separate support branches.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use GitHub's private vulnerability reporting form:

https://github.com/trobrock/notch/security/advisories/new

Include the affected version, platform, impact, reproduction steps, and any suggested mitigation. Remove real API keys, OAuth tokens, session contents, and other private data. You can expect an acknowledgement within seven days, followed by updates as the report is investigated. Public disclosure should wait until a fix or coordinated disclosure plan is ready.

## Security boundaries

Notch executes tools, extensions, plugins, MCP servers, and provider-directed tool calls with the current user's privileges. It does not currently provide a sandbox or per-tool approval UI. Only install extensions and MCP servers you trust, review commands before requesting destructive work, and avoid running Notch with elevated privileges.

Credentials belong in environment variables or Notch's mode-0600 credential store, never in issues, configuration examples, session excerpts, or commits. See [docs/providers.md](docs/providers.md) for credential storage details.
