# OpenPersona

[![CI](https://github.com/VicoAritonang/open-persona/actions/workflows/ci.yml/badge.svg)](https://github.com/VicoAritonang/open-persona/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/go.vicoworks.com/open-persona.svg)](https://pkg.go.dev/go.vicoworks.com/open-persona)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Persona and context engineering for AI products, in Go. Train a knowledge graph once, ship it as one file, and use it from Go in process, or from any language over HTTP or gRPC. Pure Go (no cgo), one SQLite file per graph, no external services required.

Two engines share one storage kernel:

| | **Persona** | **Memory** |
|---|---|---|
| Purpose | Give an AI a trained discipline (a finance controller, a travel agent) | Remember users and conversations |
| Writes | Only during training, in a controlled environment | Continuously, at runtime |
| Runtime | Read-only: retrieve, then answer as the persona | Store and retrieve |
| Artifact | A frozen `.persona` pack (one file) | A `.db` file with a scope per user |

Retrieval never needs a chat model; memory ingestion needs one small call per batch of messages. Embeddings are optional.

## Use only what you need

Every capability is its own package. Go compiles only the packages you import, so a program that only talks to a server does not compile the storage engine, and one that only retrieves does not compile gRPC.

| Import | For | Pulls in |
|---|---|---|
| `go.vicoworks.com/open-persona` | Client for a persona on a server (`NewClient`), the `Persona` interface, the agent tool | standard library only |
| `.../local` | Run a `.persona` pack in your process (`local.Open`) | + SQLite (pure Go) |
| `.../persona` | Train, open, retrieve, evaluate packs | + SQLite |
| `.../memory` | Long-term memory engine | + SQLite |
| `.../recall`, `.../graph` | Build your own retrieval channels on the storage kernel | + SQLite |
| `.../llm` | OpenAI-compatible client, or implement the one-method `llm.Client` | standard library only |
| `.../server` | HTTP API and MCP, to embed in your own server | + SQLite |
| `.../server/rpc` | The same API as gRPC services | + protobuf, Connect |
| `.../api/gen/openpersona/v1` | Generated Go types and gRPC clients | + protobuf, Connect |

Measured (stripped `linux/amd64` binaries): a client-only program is 3.5 MB, a retrieval-only program 8.3 MB, the HTTP server 10.5 MB, the HTTP and gRPC server 13.4 MB.

## Install

```sh
go get go.vicoworks.com/open-persona@latest                          # the library (Go 1.26+)
go install go.vicoworks.com/open-persona/cmd/openpersona@latest      # the CLI and server
docker build -t openpersona .                                        # or the server as an image
```

Put `OPENAI_API_KEY` and `OPENROUTER_API_KEY` in `.env` (see `.env.example`).

## Quick start

### Train a persona, then use it

```sh
# Compile 12 policy documents into a pack, scoring a held-out question set before it is frozen
openpersona train -spec examples/finance/spec.json -src examples/finance/sources \
                  -eval examples/finance/eval.json -out packs/fina.persona

openpersona inspect -pack packs/fina.persona [-units]
openpersona eval    -pack packs/fina.persona -set examples/finance/eval.json [-answer]
openpersona chat    -pack packs/fina.persona -q "Saya mau beli server 40 juta, siapa yang menyetujui?"
```

A broader example is [Kora](examples/travel/README.md), a travel agent: 30 documents, 1,318 knowledge units, and a guide to wiring it into an agent that has flight, hotel and search tools.

### From Go

```go
import (
	openpersona "go.vicoworks.com/open-persona"
	"go.vicoworks.com/open-persona/local"
)

kora, err := local.Open("packs/kora.persona", local.Options{Chat: model}) // in process
// or, on a server: kora := openpersona.NewClient(key, openpersona.ClientOptions{BaseURL: url}).Persona("kora")

k, _ := kora.Knowledge(ctx, "kereta Nagoya ke Kariya", openpersona.KnowledgeOptions{}) // no model call
tool, _ := openpersona.NewKnowledgeTool(ctx, kora, openpersona.ToolOptions{})           // a tool for your agent's model
reply, _ := openpersona.Ask(ctx, kora, "Budget Rp 20 juta, seminggu di Jepang")
```

Memory:

```go
store, _ := graph.Open("mem.db", graph.Options{})
mem, _ := memory.New(memory.Config{Store: store, Extractor: extractor, Embedder: embedder})
mem.Add(ctx, "user-42", []memory.Message{{Role: "user", Content: "I moved to Bandung."}})
res, _ := mem.Search(ctx, "user-42", "where does the user live?", memory.SearchOptions{})
block := res.Context(1500) // prompt-ready, token-budgeted
```

### From any other language

```sh
OPENPERSONA_API_KEY=change-me openpersona serve -addr :8080 -db mem.db -personas ./packs
```

One port serves three interfaces over the same operations, with the same authorization and error codes:

| Interface | Contract | Use it from |
|---|---|---|
| HTTP/JSON | [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1, also at `GET /openapi.yaml`) | anything with an HTTP client |
| gRPC | [`api/proto/openpersona/v1`](api/proto/openpersona/v1) | generated clients in Python, TypeScript, Java, ...; grpcurl |
| MCP | `POST /mcp` | MCP-capable agents |

```sh
grpcurl -plaintext -H "authorization: Bearer change-me" \
  -d '{"persona_id": "kora", "query": "kereta Nagoya ke Kariya"}' \
  localhost:8080 openpersona.v1.PersonaService/GetKnowledge
```

See [docs/GRPC.md](docs/GRPC.md) to generate a client for your language and [docs/HTTP_API.md](docs/HTTP_API.md) for the endpoints.

## Customizing

- **Models**: `llm.Client` is one method; plug in any provider. `OPENPERSONA_EXTRACT_MODEL`, `OPENPERSONA_CHAT_MODEL` and `OPENPERSONA_EMBED_MODEL` take `provider:model`; add providers in `llm.Providers`.
- **Retrieval**: a `recall.Channel` is a function returning ranked hits. Channels run in parallel and are fused by rank.
- **Embeddings**: optional everywhere (`OPENPERSONA_EMBED_MODEL=off` runs lexical + graph only).
- **Language**: `graph.Options.Analyzer` controls text analysis. The default handles Indonesian affixes, with English stemming on top.
- **What to remember**: `memory.Config.ExtractionRules`.
- **Server wiring**: `server.Server` takes your own `Catalog` (where packs come from), `Authenticator`, `JobStore` and `UsageRecorder`.

## Repository layout

```
openpersona.go, client.go, tool.go   the client library (package openpersona)
local/        run a pack in process
persona/      trainer, read-only runtime, pack format, evaluation
memory/       read-write memory engine
recall/       parallel retrieval channels, rank fusion, LLM rerank
graph/        storage kernel: nodes, edges, BM25, vectors (SQLite, pure Go)
llm/          OpenAI-compatible client, JSON helper, usage meter
server/       the API over HTTP and MCP; server/rpc: over gRPC
api/          the API contract: openapi.yaml, proto/, generated Go code in gen/
cmd/openpersona/   the CLI and server
examples/     training material (finance, travel) and a client program
docs/         guides
platform/     the hosted platform (registry, AWS deployment): a separate module, see platform/README.md
```

## Versioning and stability

The project follows [Semantic Versioning](https://semver.org). Three things are versioned separately:

| What | Version | Promise |
|---|---|---|
| Go module | `v0.x.y` tags | Pre-1.0: a minor release may change the Go API, listed under "Changed" in [CHANGELOG.md](CHANGELOG.md). |
| Wire API | `/v1`, `openpersona.v1` | Stable: additive changes only. CI rejects breaking Protobuf changes (`buf breaking`) and routes that drift from the OpenAPI document. |
| Pack format | `persona.PackFormat` (1) | A build reads every older format and refuses newer ones. |

See [api/README.md](api/README.md) and [docs/RELEASING.md](docs/RELEASING.md).

## Documentation

| Doc | For |
|---|---|
| [docs/INTEGRATION.md](docs/INTEGRATION.md) | Using it in an AI agent: the turn loop, memory as tools, persona + memory |
| [docs/CLIENT.md](docs/CLIENT.md) | The Go client: a persona on a server or in process |
| [docs/HTTP_API.md](docs/HTTP_API.md) | The HTTP endpoints |
| [docs/GRPC.md](docs/GRPC.md) | gRPC: clients for other languages, grpcurl, deployment |
| [docs/PACKS.md](docs/PACKS.md) | The `.persona` file format and evaluation |
| [docs/DESIGN.md](docs/DESIGN.md) | The research behind the design, measurements, roadmap |
| [docs/RELEASING.md](docs/RELEASING.md) | Versioning, CI, releasing |
| [llms.txt](llms.txt) | A compact map of the API for coding agents |

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

Copyright 2026 Vico Winner Sebastian Aritonang. Licensed under the [Apache License 2.0](LICENSE); see [NOTICE](NOTICE).

Persona packs (`.persona` files) and their training material are not covered by it: each pack is distributed under its own terms.
