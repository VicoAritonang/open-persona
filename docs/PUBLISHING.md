# Publishing

The module path is `go.vicoworks.com/open-persona`; the code lives at `github.com/VicoAritonang/open-persona`. The vanity host `go.vicoworks.com` (a Cloudflare Worker) tells the `go` tool where to find the code, so the import path stays the same even if the repository moves. It is managed in the separate **go-vanity** project, which serves every vicoworks Go library: its `src/packages.js` maps `open-persona` to this repository. Nothing in this repository deploys it.

The `module` line in `go.mod` must be exactly `go.vicoworks.com/open-persona`; otherwise `go get` fails with "module declares its path as ... but was required as ...".

The root package is named `openpersona` while its path ends in `open-persona`, so import it with an explicit name:

```go
import openpersona "go.vicoworks.com/open-persona"
```

## One-time setup

1. **Push the repository** and make it public. (For a private repository, importers need `GOPRIVATE=go.vicoworks.com` and git credentials, and pkg.go.dev will not show it.)
2. **License**: Apache-2.0 (`LICENSE`). pkg.go.dev only displays documentation for modules with a recognized open-source license, and reads it per version: a license added later shows from the next tag on.
3. **Check the vanity host** answers for the module and its packages:

```sh
curl "https://go.vicoworks.com/open-persona?go-get=1"
# <meta name="go-import" content="go.vicoworks.com/open-persona git https://github.com/VicoAritonang/open-persona">
```

## Releasing a version

Go modules are published by git tags; there is no upload step.

```sh
git tag v0.1.0
git push origin v0.1.0
```

The Release workflow then runs the tests, attaches CLI binaries for Linux, macOS and Windows to a GitHub release, and asks the Go module proxy to index the version.

Check it from outside the repository, then request indexing at https://pkg.go.dev/go.vicoworks.com/open-persona:

```sh
mkdir /tmp/gotest && cd /tmp/gotest && go mod init example.com/test
GOPROXY=direct go get go.vicoworks.com/open-persona@v0.1.0   # direct: skip the proxy's cached @latest while testing
```

- Only tags on commits whose `go.mod` declares `go.vicoworks.com/open-persona` can be fetched under this path. An older tag (from before the path change) cannot.
- Versioning: while the API is still changing, stay on `v0.x` (breaking changes are allowed between minor versions). From `v2.0.0` on, the module path needs a `/v2` suffix (`go.vicoworks.com/open-persona/v2`); the vanity host needs no change for that.
- A published version is immutable: the Go proxy caches it permanently, so fix mistakes with a new tag, never by moving one.

## Workflows

| File | Trigger | Does |
|---|---|---|
| `.github/workflows/ci.yml` | push to `main`, pull requests | gofmt, `go mod tidy` check, vet, tests on Linux (with the race detector) and Windows, cgo-free build |
| `.github/workflows/release.yml` | tag `v*` | tests, binaries, GitHub release, proxy warm-up |

CI needs no API keys: the tests use a fake LLM.

## Using an unreleased version from another local project

Before the first tag, or while changing both projects at once, point the other project at your checkout:

```sh
go mod edit -replace go.vicoworks.com/open-persona=../openpersona
```

Remove the `replace` line before committing that project.
