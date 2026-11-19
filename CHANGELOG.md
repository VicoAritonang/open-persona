# Changelog

All notable changes to the OpenPersona library (`go.vicoworks.com/open-persona`). The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow [Semantic Versioning](https://semver.org). Before v1.0.0, a minor release may change the Go API; such changes are listed under "Changed" or "Removed" with how to migrate. The wire API (`/v1`, `openpersona.v1`) and the pack format stay compatible regardless.

The hosted platform in `platform/` has its own [changelog](platform/CHANGELOG.md).

## [Unreleased]

### Added

- **gRPC.** `server/rpc` serves the API as `openpersona.v1.PersonaService` and `openpersona.v1.MemoryService` (gRPC, gRPC-Web and Connect on the same port as HTTP), with the standard health service and optional reflection. `openpersona serve` mounts it and accepts cleartext HTTP/2.
- **API contract.** `api/openapi.yaml` (OpenAPI 3.1, served at `GET /openapi.yaml`) and `api/proto/openpersona/v1` (Protobuf), with generated Go code in `api/gen`. CI checks Protobuf style and breaking changes, regenerates the code, and tests that the server serves exactly the documented routes and error codes.
- **Error codes.** Every HTTP error body carries a stable `code` next to `error` (`invalid_argument`, `unauthenticated`, `permission_denied`, `not_found`, `unavailable`, `unimplemented`, `upstream`, `internal`); `openpersona.APIError.Code` exposes it. Streaming errors carry it too. `503` responses set `Retry-After`.
- `server.Server` exposes its operations (`Knowledge`, `Chat`, `SubmitJob`, `Job`, `ListPersonas`, `Manifest`, `AddMessages`, `SearchMemory`, `Authenticate`) for other transports, and `Mount` for extra handlers.
- `local` package: `local.Open` and `local.Fetch` run a pack in process.
- `openpersona version`; binaries report their module version.
- `NOTICE`, `CONTRIBUTING.md`, `SECURITY.md`, this changelog, a `Makefile`, Dependabot.

### Changed

- **The root package no longer compiles the storage engine.** `openpersona.NewClient` and the `Persona` interface depend on the standard library only (a client-only binary went from 7.6 MB to 3.5 MB). Migrate: `openpersona.Open` → `local.Open`; `openpersona.LocalOptions` → `local.Options`; `*openpersona.Local` → `*local.Persona`; `client.OpenLocal(ctx, id, dir, opt)` → `local.Fetch(ctx, client, id, dir, opt)`; `openpersona.ErrNoChatModel` → `local.ErrNoChatModel`.
- `openpersona.Info` is its own struct instead of an alias of `persona.Spec` (same JSON fields, without the retrieval defaults).
- `Client.Download` verifies the pack's file header itself instead of opening it as a database. The download chunk size is the exported `openpersona.DownloadChunk`.
- Package `cloud` is merged into `server`: `cloud.Principal` → `server.Principal`, likewise `Authenticator`, `StaticKey`, `AnyOf`, `Job`, `JobStore`, `MemoryJobs`, `Usage`, `UsageRecorder`, the `Scope*` and `Job*` constants, `ErrUnauthorized`, `ErrNotFound` and `NewID`; `cloud.Open` → `server.OpenAccess`.
- `server.Server.Chat` (the model) is renamed `ChatModel`; `Chat` is now the operation.
- `knowledge` rejects `units` above 64 or `sources` above 16 with `400` (it already did) and negative values too.

### Removed

- The Postgres registry (`registry`), the AWS Lambda command and the AWS deployment moved to the hosted platform (`platform/`), as did the CLI's `registry`, `push`, `pull` and `keys` commands and `serve -cloud` / `serve -registry`. The library's go.mod no longer requires pgx or the AWS SDK.

## [0.1.1] - 2026-11-16

### Added

- Apache-2.0 license.

## [0.1.0] - 2026-11-16

First tagged version under the import path `go.vicoworks.com/open-persona`: the persona and memory engines, the CLI, the HTTP and MCP server, the Go client (hosted and local), the Postgres registry and the AWS deployment.

[Unreleased]: https://github.com/VicoAritonang/open-persona/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/VicoAritonang/open-persona/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/VicoAritonang/open-persona/releases/tag/v0.1.0
