# Contributing to Notch

Contributions are welcome through GitHub issues and pull requests.

## Before opening an issue

- Search existing issues and discussions first.
- Use the security process in [SECURITY.md](SECURITY.md) for vulnerabilities.
- Remove API keys, OAuth tokens, prompts containing private data, and local credentials from logs and screenshots.
- Include `notch version`, the operating system, terminal, provider, and model when they affect the report.

## Development setup

Notch requires Go 1.23 or newer and has no Node.js or npm dependency.

```sh
git clone https://github.com/trobrock/notch.git
cd notch
go mod download
make check
make build
./bin/notch --version
```

Use a focused branch based on `main`. Keep generated binaries, credentials, `.notch` project state, and unrelated formatting changes out of commits.

## Project expectations

- Keep the provider-independent agent loop small and direct.
- Preserve the single-native-binary installation and event-driven TUI.
- Avoid background polling, unnecessary dependencies, and speculative abstractions.
- Treat provider, tool, extension, and session text as untrusted terminal content.
- Put optional workflows in extensions when a generic core primitive is sufficient.
- Preserve session compatibility unless a migration is documented.

See [AGENTS.md](AGENTS.md) and [docs/architecture.md](docs/architecture.md) for the detailed design constraints.

## Pull requests

A pull request should:

1. explain the problem and the chosen behavior;
2. stay limited to one coherent change;
3. include focused tests for changed behavior;
4. update user-facing documentation when needed; and
5. pass:

```sh
make check
make build
git diff --check
```

Cross-compile when changing platform-specific terminal, filesystem, or upgrade code. Provider changes should use deterministic test servers and must not require live credentials in CI.

By contributing, you agree that your contribution is licensed under the repository's [MIT License](LICENSE). No contributor license agreement is required.
