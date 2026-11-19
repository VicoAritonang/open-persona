# Versioning and releasing

## What is versioned

| What | Where | Scheme |
|---|---|---|
| The Go library | git tags `vX.Y.Z` | [SemVer](https://semver.org). Before v1.0.0 a minor release may change the Go API (listed in CHANGELOG.md with a migration note); a patch release never does. |
| The wire API | `/v1` paths, `openpersona.v1` Protobuf package | Additive changes only within v1 ([api/README.md](../api/README.md)); a breaking change is a new major API version served next to the old one. |
| The pack format | `persona.PackFormat` in each `.persona` header | An integer. A build reads every format up to its own and refuses newer ones. Bump it only when old builds would misread a new pack. |
| A persona | `id` and `version` in its `spec.json` | SemVer per persona; `id@version` is immutable. |
| The platform | git tags `platform/vX.Y.Z` (here), `vX.Y.Z` (after the split) | SemVer, with its own CHANGELOG. |

The library and the API version independently: library v0.9.0 and v1.4.0 can both serve API v1.

## The import path

The module path is `go.vicoworks.com/open-persona`; the code lives at `github.com/VicoAritonang/open-persona`. The vanity host `go.vicoworks.com` (a Cloudflare Worker managed in the separate **go-vanity** project) tells the `go` tool where the code is, so the import path survives a repository move. Check it:

```sh
curl "https://go.vicoworks.com/open-persona?go-get=1"
# <meta name="go-import" content="go.vicoworks.com/open-persona git https://github.com/VicoAritonang/open-persona">
```

The root package is named `openpersona` while its path ends in `open-persona`, so import it with an explicit name: `import openpersona "go.vicoworks.com/open-persona"`. From v2.0.0 on, the module path needs a `/v2` suffix; the vanity host needs no change for that.

## Releasing the library

1. Make sure `main` is green in CI.
2. In `CHANGELOG.md`, rename "Unreleased" to the new version and date, add an empty "Unreleased" above it, and update the compare links at the bottom. Choose the version from the changes: a breaking Go API change bumps the minor version before v1 (the major after), a feature bumps the minor, a fix bumps the patch.
3. Commit (`Release v0.2.0`), then tag and push:
   ```sh
   git tag -a v0.2.0 -m "v0.2.0"
   git push origin main v0.2.0
   ```
4. The Release workflow tests the tag, builds the CLI for Linux, macOS and Windows with the version stamped in (`openpersona version`), creates a GitHub release with the changelog section as its notes and SHA-256 checksums, and asks the Go module proxy to index the version.
5. Check it from outside the repository:
   ```sh
   cd "$(mktemp -d)" && go mod init example.com/check
   GOPROXY=direct go get go.vicoworks.com/open-persona@v0.2.0
   ```

A published version is immutable: the Go proxy and checksum database keep it forever. Fix a bad release with a new version, never by moving a tag. Pre-releases (`v0.3.0-rc.1`) are marked as such on GitHub.

## Releasing the platform

Tag `platform/v0.2.0` after updating `platform/CHANGELOG.md`. Deploy as described in [platform/docs/HOSTING.md](../platform/docs/HOSTING.md). When the platform depends on library changes, release the library first and point the platform at that version after the split.

## Workflows

| File | Trigger | Does |
|---|---|---|
| `.github/workflows/ci.yml` | push to `main`, pull requests | The library: gofmt, `go mod tidy`, vet, tests (race detector on Linux; also Windows and macOS), cgo-free build; `buf lint`, `buf breaking` against `main`, generated code up to date |
| `.github/workflows/platform.yml` | changes under `platform/` or the library | The platform module: gofmt, tidy, vet, tests, Lambda and EC2 builds |
| `.github/workflows/release.yml` | tag `v*` | Library release (above) |
| `.github/dependabot.yml` | weekly | Go module and GitHub Actions updates |

CI needs no secrets: tests use a fake model, and the registry tests skip without a database.

## Using an unreleased version from another project

Point the other project at a checkout while changing both at once:

```sh
go mod edit -replace go.vicoworks.com/open-persona=../open-persona
```

Remove the `replace` line before committing that project, or require a pseudo-version of a pushed commit (`go get go.vicoworks.com/open-persona@<commit>`).
