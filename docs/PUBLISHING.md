# Publishing

The module path is `go.vicoworks.com/openpersona`; the code lives at `github.com/VicoAritonang/open-persona`. A small Cloudflare Worker on `go.vicoworks.com` tells the `go` tool where to find the code, so the import path stays the same even if the repository moves.

## One-time setup

1. **Push the repository** and make it public. (For a private repository, importers need `GOPRIVATE=go.vicoworks.com` and git credentials, and pkg.go.dev will not show it.)
2. **Add a `LICENSE` file.** pkg.go.dev only displays documentation for modules with a recognized open-source license.
3. **Cloudflare**: `vicoworks.com` must be a zone in your account. Create an API token from the "Edit Cloudflare Workers" template.
4. **GitHub repository secrets** (Settings, Secrets and variables, Actions):
   - `CLOUDFLARE_API_TOKEN`
   - `CLOUDFLARE_ACCOUNT_ID`
5. **Deploy the Worker**: it deploys on the first push to `main` that includes `deploy/vanity/`, or run the "Deploy go.vicoworks.com" workflow by hand. To deploy from your machine instead: `cd deploy/vanity && npx wrangler deploy`.

Check it:

```sh
curl "https://go.vicoworks.com/openpersona?go-get=1"
# <meta name="go-import" content="go.vicoworks.com/openpersona git https://github.com/VicoAritonang/open-persona">
```

## Releasing a version

Go modules are published by git tags; there is no upload step.

```sh
git tag v0.1.0
git push origin v0.1.0
```

The Release workflow then runs the tests, attaches CLI binaries for Linux, macOS and Windows to a GitHub release, and asks the Go module proxy to index the version.

Other projects can now use it:

```sh
go get go.vicoworks.com/openpersona@v0.1.0
```

Versioning: while the API is still changing, stay on `v0.x` (breaking changes are allowed between minor versions). A published version is immutable: the Go proxy caches it permanently, so fix mistakes with a new tag, never by moving one.

## Workflows

| File | Trigger | Does |
|---|---|---|
| `.github/workflows/ci.yml` | push to `main`, pull requests | gofmt, `go mod tidy` check, vet, tests on Linux (with the race detector) and Windows, cgo-free build |
| `.github/workflows/vanity.yml` | changes under `deploy/vanity/`, manual | deploys the Worker, then checks the go-import tag is served |
| `.github/workflows/release.yml` | tag `v*` | tests, binaries, GitHub release, proxy warm-up |

CI needs no API keys: the tests use a fake LLM.

## Using an unreleased version from another local project

Before the first tag, or while changing both projects at once, point the other project at your checkout:

```sh
go mod edit -replace go.vicoworks.com/openpersona=../openpersona
```

Remove the `replace` line before committing that project.

## Adding more modules to the domain

Add a line to `MODULES` in `deploy/vanity/src/index.js` and push.
