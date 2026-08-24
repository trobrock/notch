# Releases and upgrades

Notch uses semantic versions with a leading `v`, such as `v0.1.0`. `notch --version` prints the effective version, while `notch version` also prints the commit, build time, Go version, and target platform. `notch version --json` provides the same fields for scripts.

Release binaries embed their tag, commit, and UTC build time. Builds made with `go install module@version` fall back to Go's embedded module version when linker metadata is unavailable. Local `make build` binaries use `git describe`, so a checkout build may report a commit or a `-dirty` version instead of a release tag.

## Upgrade commands

```sh
# Only query the latest stable GitHub release.
notch upgrade --check

# Download and install the latest stable release.
notch upgrade

# Install an exact release. A leading v is optional.
notch upgrade --version v0.2.0

# Reinstall the current release or explicitly downgrade.
notch upgrade --version v0.1.0 --force
```

The updater queries releases from `trobrock/notch`. It supports Linux, macOS, and Windows on amd64 and arm64. A normal upgrade only installs a newer semantic version. Development builds can move to the latest stable release. `--version` can select a prerelease, but the default GitHub `latest` lookup does not select prereleases.

The executable's directory must be writable. Installations owned by a package manager should be upgraded through that package manager instead; Notch does not invoke `sudo`. On Unix, the verified binary is written beside the current executable, synced, and atomically renamed into place. On Windows, the current executable is moved aside before replacement; a temporary `.old` file can remain if the running process prevents immediate cleanup.

## Integrity model

Every release contains platform archives and `checksums.txt`. The updater:

1. obtains the release and asset URLs through the GitHub API;
2. downloads `checksums.txt` and the exact archive for the current platform;
3. verifies the archive's SHA-256 digest before extracting it;
4. accepts only a regular root-level `notch` or `notch.exe` entry;
5. limits both compressed downloads and extracted binary size; and
6. preserves the installed executable's permission bits during replacement.

A failed request, missing asset, malformed archive, or checksum mismatch leaves the existing executable unchanged. Checksums protect against transfer corruption and mismatched assets; they share GitHub Releases as their trust root and are not a separate signature system.

## Publishing a release

The `Release` GitHub Actions workflow runs for tags named `v*`. It validates the semantic version, runs race tests and vet, builds static archives for all supported targets, generates SHA-256 checksums, and creates the GitHub release.

From a clean `main` branch:

```sh
git tag -a v0.2.0 -m 'Notch v0.2.0'
git push origin v0.2.0
```

To inspect the artifacts locally without publishing:

```sh
scripts/release.sh v0.2.0
(cd dist && sha256sum -c checksums.txt)
```

Do not move or retag an existing release version. Publish a new semantic version for every changed binary.
