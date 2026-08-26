# Extension packages

Notch extension packages provide a decentralized way to share, install, update, and remove Lua extensions and executable JSON-RPC plugins. Packages use GitHub, generic Git repositories, or local directories; they do not require npm, Node.js, or a central registry.

> **Security:** extensions are trusted, unsandboxed code and there are no per-command approvals after loading. An executable plugin can run arbitrary programs, and Lua extensions can call the Notch host API with the user's privileges. Review third-party source before installation. Package installation does not run build, post-install, or setup scripts, but installed extensions run automatically when Notch next starts.

## Commands

```sh
# Create a package in a new or existing repository.
notch extensions init --name example-extension ./example-extension

# Install a public GitHub repository through the GitHub API.
notch extensions install github:owner/repository
notch extensions install --ref v1.2.0 github:owner/repository

# Install a package located inside a monorepo.
notch extensions install --subdir packages/notch github:owner/repository

# Raw GitHub URLs are accepted.
notch extensions install https://github.com/owner/repository

# Generic HTTPS, SSH, and file Git URLs use the installed `git` executable.
notch extensions install git:https://gitlab.example/owner/repository.git
notch extensions install --ref stable git:ssh://git@example.com/owner/repository.git

# Local directories are copied rather than linked.
notch extensions install ./path/to/package

notch extensions validate ./path/to/package
notch extensions list
notch extensions list --json
notch extensions update                 # every installed package
notch extensions update package-name
notch extensions remove package-name
```

### Declarative sync

Keep a portable desired-state manifest in dotfiles at
`$XDG_CONFIG_HOME/notch/extensions.json` (normally
`~/.config/notch/extensions.json`):

```json
{
  "version": 1,
  "packages": [
    {
      "name": "example-extension",
      "source": "github:owner/example-extension",
      "ref": "v1.2.0"
    },
    {
      "name": "monorepo-extension",
      "source": "github:owner/repository",
      "ref": "0123456789abcdef",
      "subdir": "packages/notch"
    }
  ]
}
```

Install packages that are declared but missing:

```sh
notch extensions sync
notch extensions sync --dry-run
notch extensions sync --json /path/to/extensions.json
```

The optional positional path overrides the default manifest. Relative local
sources are resolved from the manifest's directory, which makes paths inside a
dotfiles repository portable. Package `name` must match the fetched package's
`notch-package.json` name. Sync is intentionally additive: it does not update,
replace, or remove installed packages. If an installed package with the same
name has a different source, sync stops and reports the mismatch. Use explicit
`update` or `remove` commands for those changes. Pin `ref` to a tag or commit
when reproducibility matters.

The desired-state manifest is separate from the generated private lock at
`$XDG_DATA_HOME/notch/packages.json`; do not copy or hand-edit that lock.

`notch extension` is an alias for `notch extensions`. `add`, `upgrade`, `uninstall`, `rm`, and `ls` are also accepted where their meaning is unambiguous.

Restart Notch after install, update, or remove. An already-running process keeps its current extension registry until exit.

## Package manifest

A repository or selected `--subdir` must contain `notch-package.json` at its root:

```json
{
  "schema_version": 1,
  "name": "example-extension",
  "version": "1.2.0",
  "description": "Example Notch commands and tools",
  "license": "MIT",
  "homepage": "https://github.com/owner/example-extension",
  "extensions": ["extensions"]
}
```

Required fields:

- `schema_version`: currently `1`.
- `name`: a stable lowercase package identity using letters, digits, `.`, `_`, or `-`.
- `version`: a semantic version, optionally prefixed with `v`.
- `extensions`: one or more relative directories. `.` exports the package root.

Every listed directory must exist and remain inside the package root. Lua files directly inside each exported directory are loaded normally. Executable plugin manifests named `plugin.json` are discovered recursively within those directories. Package files must already be ready to run; Notch deliberately has no install scripts or automatic build step.

A typical package is:

```text
notch-package.json
extensions/
  commands.lua
  developerly/
    plugin.json
    developerly-plugin
```

`notch extensions init` writes a valid manifest plus a small Lua command that can be replaced with the real extension.

## Source and update behavior

### GitHub

`github:owner/repository` and raw `https://github.com/owner/repository` sources are fetched natively over the GitHub API. Notch resolves the selected ref to an exact commit and downloads that commit's archive. Set `GITHUB_TOKEN` or `GH_TOKEN` for private repositories or higher API limits; tokens are sent in request headers and are never saved in package state.

With no ref, install and update track the repository's default `HEAD`. A branch or tag is resolved again on update and may deliver new code; an exact commit remains unchanged and is the safest immutable pin. The durable lock record always stores the resolved commit SHA.

GitHub shorthand may encode a ref and subdirectory:

```text
github:owner/repository@v1.2.0//packages/notch
```

The explicit `--ref` and `--subdir` flags are easier to read and work for every source type.

### Generic Git

Generic `https://`, `ssh://`, `git@host:path`, and `file://` sources invoke `git`, preserving the user's SSH configuration and credential helpers. Insecure `http://` and `git://` transports and URLs containing embedded HTTPS credentials are rejected. The resolved commit SHA is locked. Generic Git is the only package source that requires an external executable.

### Local directories

Local packages are copied into Notch's managed package directory. `update` recopies the original absolute source path. The resolved value is the package tree's SHA-256 digest.

## Storage, integrity, and recovery

Global package state lives under the effective Notch home:

```text
~/.local/share/notch/packages.json
~/.local/share/notch/packages/<package-name>/
```

`XDG_DATA_HOME` relocates both paths and must be absolute when set. `packages.json` is mode `0600`, records source/ref/subdirectory, exact commit or local digest, installed version, timestamps, and a SHA-256 tree digest. `notch extensions list` reports `modified`, `missing`, or `unreadable` when installed content no longer matches its lock record.

Install, update, and remove use a process lock, same-filesystem staging directories, atomic renames, and recovery markers. GitHub archives reject traversal paths, links, devices, duplicate paths, oversized downloads, and excessive expanded size. Local and Git copies reject symbolic links and special files. Updates reconcile modified or missing installed content with the locked source and refuse semantic-version downgrades unless `--force` is passed.

Installed package extension directories are appended after direct configured extension directories. Existing duplicate tool or command rules still apply, so a package cannot silently replace a built-in or an earlier extension.

## Publishing workflow

1. Run `notch extensions init` or add `notch-package.json` to an existing repository.
2. Keep exported extension directories self-contained and ready to run.
3. Run `notch extensions validate .`, then test by installing the local directory under a temporary absolute `XDG_DATA_HOME`.
4. Commit and push the repository.
5. Tag semantic releases when consumers should pin stable versions.
6. Share `github:owner/repository`, the raw repository URL, or the URL plus a recommended tag.

There is currently no central search index, dependency resolver, publisher account, signing service, or platform-specific release-asset selector. The manifest and lock format are designed so a future registry can use the same package contents without replacing this decentralized workflow.
