# Contributing

Thank you for helping. Issues and pull requests are welcome at https://github.com/VicoAritonang/open-persona.

## Setup

- Go 1.26 or newer. No cgo, no external services: tests use a fake model, so they need no API keys and cost nothing.
- `make check` runs everything CI runs on the library: formatting, `go mod tidy`, vet, tests with the race detector, Protobuf lint, the breaking-change check and the generated-code check. The Protobuf steps run [buf](https://buf.build) through `go run`, pinned in the `Makefile`.

## Layout

The repository holds two Go modules:

- `/` is the library, `go.vicoworks.com/open-persona`.
- `/platform` is the hosted platform, a separate module that will move to its own repository. It may depend on the library's public API; the library must never depend on it.

## Changes

- Keep each package's dependencies minimal: the root package uses the standard library only, and only `server/rpc` and `api/gen` may import protobuf. A new third-party dependency needs a reason in the pull request.
- API changes start in the contract (`api/openapi.yaml`, `api/proto`) and follow the compatibility rules in [api/README.md](api/README.md). Run `make generate` after editing `.proto` files and commit the result.
- Add tests next to the code. Behaviour shared by HTTP and gRPC belongs in `server/core.go`, so both transports get it.
- Add a line to `CHANGELOG.md` under "Unreleased" for anything a user would notice. Mark breaking Go API changes and say how to migrate.
- Commits: one logical change each, with a message that says why. Imperative subject line, at most 72 characters.

## License

By contributing you agree that your contributions are licensed under the Apache License 2.0, as the project is.
