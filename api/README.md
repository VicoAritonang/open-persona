# The OpenPersona API contract

This directory is the source of truth for the wire API. Servers implement it (`server`, `server/rpc`); clients in any language are written or generated against it.

| File | Describes |
|---|---|
| [`openapi.yaml`](openapi.yaml) | The HTTP API (OpenAPI 3.1). Served at `GET /openapi.yaml`. |
| [`proto/openpersona/v1/`](proto/openpersona/v1) | The gRPC services `openpersona.v1.PersonaService` and `openpersona.v1.MemoryService` (Protobuf). |
| [`gen/`](gen) | Go code generated from the Protobuf files. Do not edit; run `make generate`. |

Both describe the same operations. Each RPC and its HTTP endpoint call the same function on the server, so they authorize, validate and fail the same way.

| Operation | HTTP | gRPC |
|---|---|---|
| List personas | `GET /v1/personas` | `PersonaService.ListPersonas` |
| Describe a persona | `GET /v1/personas/{id}` | `PersonaService.GetPersona` |
| Retrieve knowledge | `POST /v1/personas/{id}/knowledge` | `PersonaService.GetKnowledge` |
| Answer | `POST /v1/personas/{id}/chat` | `PersonaService.Chat` |
| Answer, streamed | `POST /v1/personas/{id}/chat` with `"stream": true` (SSE) | `PersonaService.ChatStream` |
| Answer asynchronously | `POST /v1/personas/{id}/jobs` | `PersonaService.SubmitChatJob` |
| Poll a job | `GET /v1/jobs/{id}` | `PersonaService.GetJob` |
| Store messages | `POST /v1/memory/{scope}/messages` | `MemoryService.AddMessages` |
| Search memory | `POST /v1/memory/{scope}/search` | `MemoryService.SearchMemory` |
| Download a pack | `GET /v1/personas/{id}/pack` | (HTTP only: a file with `Range` support) |
| Health | `GET /healthz` | `grpc.health.v1.Health/Check` |

## Errors

| Code (HTTP body `code`) | HTTP | gRPC | Retry? |
|---|---|---|---|
| `invalid_argument` | 400 | `INVALID_ARGUMENT` | No: fix the request |
| `unauthenticated` | 401 | `UNAUTHENTICATED` | No |
| `permission_denied` | 403 | `PERMISSION_DENIED` | No: the key lacks a scope |
| `not_found` | 404 | `NOT_FOUND` | No |
| `internal` | 500 | `INTERNAL` | Maybe |
| `unimplemented` | 501 | `UNIMPLEMENTED` | No: use another endpoint (jobs instead of streaming) |
| `upstream` | 502 | `UNAVAILABLE` | Yes, with backoff: the chat model failed |
| `unavailable` | 503 | `UNAVAILABLE` | Yes, after `Retry-After` |

Branch on the code, never on the message text.

## Compatibility rules

Version 1 (`/v1`, `openpersona.v1`) is stable. Within it:

- **Allowed**: new endpoints and RPCs, new optional request fields, new response fields, new enum values, new error codes. Clients must ignore unknown fields and treat an unknown error code like `internal`.
- **Not allowed**: removing or renaming anything, changing a field's type or number, changing a field's meaning or default, making an optional field required.
- **Deprecation**: mark it (`deprecated: true` in OpenAPI, `[deprecated = true]` in Protobuf), keep it working for the rest of v1, and say so in the changelog.
- A change that needs any of the "not allowed" items ships as v2, side by side with v1.

CI enforces what it can:

- `buf lint`: Protobuf style (the STANDARD rule set).
- `buf breaking` against `main`: no breaking change to the Protobuf files (the FILE rule set, which also protects generated code).
- `buf generate` must leave `gen/` unchanged: generated code is never stale.
- `TestRoutesMatchOpenAPI`: the server serves exactly the operations `openapi.yaml` documents.
- `TestErrorCodesMatchOpenAPI`: the error codes in code and in the document are the same set.

## Changing the API

1. Edit `openapi.yaml` and/or `proto/openpersona/v1/*.proto`.
2. `make generate`, then implement it in `server` (the operation in `core.go`, the handler in `rest.go`) and `server/rpc`.
3. `make check` runs the linters, the breaking-change check and the tests.
4. Add the change to `CHANGELOG.md` under "Unreleased".
