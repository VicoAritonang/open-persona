# OpenPersona

Open-source persona and context engineering for AI products, in Go. One static binary, one SQLite file per graph, no external services required (an optional Postgres registry distributes persona packs).

It has two engines on one storage kernel:

| | **Persona** | **Memory** |
|---|---|---|
| Purpose | Give an AI a trained discipline (a finance controller, a support lead) | Remember users and conversations |
| Writes | Only during training, in a controlled environment | Continuously, at runtime |
| Runtime | Read-only: retrieve, then answer as the persona | Store and retrieve |
| Artifact | A frozen `.persona` pack (one file) | A `.db` file with scopes per user |

Design goal: the quality of graph-memory systems at a fraction of the cost. Retrieval never needs a chat model; ingestion needs one small call per batch of messages (two when a contradiction has to be resolved). Embeddings are optional.

| Doc | For |
|---|---|
| [docs/INTEGRATION.md](docs/INTEGRATION.md) | Using it in an AI agent: the turn loop, memory as tools, persona + memory, production notes |
| [docs/HTTP_API.md](docs/HTTP_API.md) | Calling it from Python, TypeScript or anything else |
| [docs/HOSTING.md](docs/HOSTING.md) | Using a persona from Go, hosted or local; the hosted service on AWS Lambda + EC2 + Neon |
| [docs/REGISTRY.md](docs/REGISTRY.md) | The `.persona` file format, evaluation, and the Postgres/Neon registry |
| [docs/DESIGN.md](docs/DESIGN.md) | The research behind the design, measurements, roadmap |
| [docs/PUBLISHING.md](docs/PUBLISHING.md) | Import domain, CI, releasing a version |
| [llms.txt](llms.txt) | A compact map of the API for coding agents |

## Install

As a library (Go 1.26+):

```sh
go get go.vicoworks.com/open-persona
```

As a command-line tool and server:

```sh
go install go.vicoworks.com/open-persona/cmd/openpersona@latest
# or, from a checkout:
go build -o bin/openpersona ./cmd/openpersona
# or:
docker build -t openpersona .
```

Put `OPENAI_API_KEY` and `OPENROUTER_API_KEY` in `.env` (see `.env.example`); add `NEON_DB_URL` to use the registry.

## Use a persona from Go: hosted or local

```go
import openpersona "go.vicoworks.com/open-persona"

client := openpersona.NewClient(apiKey, openpersona.ClientOptions{BaseURL: "https://api.example.com"})
kora := client.Persona("kora")                                   // runs on the hosted service
// kora, _ := client.OpenLocal(ctx, "kora", "./packs", opts)     // or downloads the pack and runs in your process

tool, _ := openpersona.NewKnowledgeTool(ctx, kora, openpersona.ToolOptions{}) // a tool for your agent's model
reply, _ := openpersona.Ask(ctx, kora, "Budget Rp 20 juta, seminggu di Jepang, ikut festival musim panas")
```

Hosted personas also answer over HTTP and [MCP](docs/HTTP_API.md#post-mcp). See [docs/HOSTING.md](docs/HOSTING.md).

## Quick start

### Persona: train once, serve read-only

```sh
# Compile 12 policy documents (a fictional company's finance handbook) into a persona pack,
# scoring a held-out question set before the pack is frozen
openpersona train -spec examples/finance/spec.json -src examples/finance/sources \
                  -eval examples/finance/eval.json -out fina.persona

openpersona inspect -pack fina.persona [-units]
openpersona eval -pack fina.persona -set examples/finance/eval.json [-answer]
openpersona chat -pack fina.persona -q "Saya mau beli server 40 juta, siapa yang menyetujui?"
openpersona chat -pack fina.persona -show     # interactive; -show prints the system prompt
```

A second, broader example is [Kora](examples/travel/README.md), a travel agent for Indonesia, Japan, Saudi Arabia and Sweden: 30 documents, 1,318 units, and a guide to wiring it into an agent that has flight, hotel and search tools.

Training chunks the sources, extracts knowledge units in parallel (principles, rules, procedures, concepts, references, examples), links them through shared tags, and synthesizes a compact identity core. At runtime each question gets a system prompt of this shape:

```
<role>       identity core: always present, about 300 words      </role>
<knowledge>  the units and source excerpts relevant to this question </knowledge>
<instructions> grounding rules </instructions>
```

### Distribute: as a file, or from the registry

A pack is one `.persona` file (SQLite, self-identifying header, versioned by `id` and `version` in the spec). Ship the file, or store every version in Postgres (Neon) and pull it where it runs:

```sh
openpersona registry migrate                      # once: creates schema "openpersona"
openpersona push -pack fina.persona -notes "first comprehensive build"
openpersona pull -id fina -out packs/fina.persona # latest published, SHA-256 verified
openpersona serve -personas ./packs -registry fina
```

Format, schema and versioning rules: [docs/REGISTRY.md](docs/REGISTRY.md).

### Memory: store and retrieve context

```sh
openpersona remember -db mem.db -scope andi -file examples/memory/conversation.json
openpersona recall   -db mem.db -scope andi -q "Andi sekarang kerja di mana?"
openpersona recall   -db mem.db -scope andi -q "..." -history   # include superseded facts
openpersona recall   -db mem.db -scope andi -q "..." -rerank    # one LLM call to rerank
```

### As a library

```go
store, _ := graph.Open("mem.db", graph.Options{})
extractor, _ := llm.New("openai:gpt-4o-mini")
embedder, _ := llm.New("openai:text-embedding-3-small")
mem, _ := memory.New(memory.Config{Store: store, Extractor: extractor, Embedder: embedder})

mem.Add(ctx, "user-42", []memory.Message{{Role: "user", Content: "I moved to Bandung."}})
res, _ := mem.Search(ctx, "user-42", "where does the user live?", memory.SearchOptions{})
block := res.Context(1500) // prompt-ready, token-budgeted

p, _ := persona.Open("fina.persona")
chat, _ := llm.New("openrouter:typesafe/jev-router")
reply, _ := p.Chat(ctx, chat, messages, persona.ChatOptions{Context: block})
```

### As a service

```sh
OPENPERSONA_API_KEY=change-me openpersona serve -addr :8080 -db mem.db -personas ./packs
```

| Endpoint | Purpose |
|---|---|
| `POST /v1/memory/{scope}/messages` | Store messages |
| `POST /v1/memory/{scope}/search` | Facts, messages and a prompt-ready context block |
| `GET /v1/personas` | List loaded packs |
| `POST /v1/personas/{name}/prompt` | System prompt for a query (bring your own model) |
| `POST /v1/personas/{name}/chat` | Answer as the persona; `memory_scope` adds user memory |

## Customizing

- **Models**: `llm.Client` is one method. `OPENPERSONA_EXTRACT_MODEL` and `OPENPERSONA_CHAT_MODEL` take `provider:model`; add providers in `llm.Providers`.
- **Retrieval**: a `recall.Channel` is a function returning ranked hits. Channels run in parallel and are fused by rank.
- **Embeddings**: `OPENPERSONA_EMBED_MODEL` (default `openai:text-embedding-3-small`; `off` runs lexical + graph only). In code, `memory.Config.Embedder` and `persona.Trainer.Embedder`.
- **Language**: `graph.Options.Analyzer` controls text analysis. The default handles Indonesian affixes, with English stemming on top.
- **What to remember**: `memory.Config.ExtractionRules`.

## Layout

```
graph/     storage kernel: nodes, edges, BM25 index, vectors (SQLite, pure Go)
recall/    parallel retrieval channels, rank fusion, LLM rerank
memory/    read-write context engine
persona/   trainer, read-only runtime, pack format, evaluation
(root)    the client library: Persona, hosted (Client) or local (Open), KnowledgeTool
cloud/     API-key principals and scopes, async jobs, usage (shared by server, registry, Lambda)
registry/  Postgres (Neon): packs and versions, API keys, jobs, usage, the on-demand catalog
llm/       OpenAI-compatible client, JSON helper, usage meter
server/    HTTP API
cmd/openpersona/          CLI and server (serve -cloud for the hosted EC2 tier)
cmd/openpersona-lambda/   the hosted API on AWS Lambda (HTTP + async job worker)
deploy/aws/               SAM template for Lambda; systemd and Caddy for EC2
deploy/vanity/     Cloudflare Worker serving the go.vicoworks.com import path
```

## Status

Early (v0.x: the API may still change). The engines, CLI and server work and are covered by offline tests (`go test ./...`). Not yet done: a benchmark run (LongMemEval / LoCoMo), an ANN vector index, scope deletion, and a license file. See the roadmap in [docs/DESIGN.md](docs/DESIGN.md).
