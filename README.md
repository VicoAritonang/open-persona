# OpenPersona

Open-source persona and context engineering for AI products, in Go. One static binary, one SQLite file per graph, no external services.

It has two engines on one storage kernel:

| | **Persona** | **Memory** |
|---|---|---|
| Purpose | Give an AI a trained discipline (a finance controller, a support lead) | Remember users and conversations |
| Writes | Only during training, in a controlled environment | Continuously, at runtime |
| Runtime | Read-only: retrieve, then answer as the persona | Store and retrieve |
| Artifact | A frozen `.persona` pack (one file) | A `.db` file with scopes per user |

Design goal: the quality of graph-memory systems at a fraction of the cost. Retrieval never needs an LLM; ingestion needs one small call per batch of messages (two when a contradiction has to be resolved). See [docs/DESIGN.md](docs/DESIGN.md) for the research behind this and the roadmap.

## Quick start

Requires Go 1.26+. Put `OPENAI_API_KEY` and `OPENROUTER_API_KEY` in `.env` (see `.env.example`).

```sh
go build -o bin/openpersona ./cmd/openpersona
```

### Persona: train once, serve read-only

```sh
# Compile a policy document into a persona pack
openpersona train -spec examples/finance/spec.json -src examples/finance/policy.md -out fina.persona

openpersona inspect -pack fina.persona
openpersona chat -pack fina.persona -q "Saya mau beli server 40 juta, siapa yang menyetujui?"
openpersona chat -pack fina.persona -show     # interactive; -show prints the system prompt
```

Training chunks the sources, extracts knowledge units in parallel (principles, rules, procedures, concepts, references, examples), links them through shared tags, and synthesizes a compact identity core. At runtime each question gets a system prompt of this shape:

```
<role>       identity core: always present, about 300 words      </role>
<knowledge>  the units and source excerpts relevant to this question </knowledge>
<instructions> grounding rules </instructions>
```

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
mem, _ := memory.New(memory.Config{Store: store, Extractor: extractor})

mem.Add(ctx, "user-42", []memory.Message{{Role: "user", Content: "I moved to Bandung."}})
res, _ := mem.Search(ctx, "user-42", "where does the user live?", memory.SearchOptions{})
block := res.Context(1500) // prompt-ready, token-budgeted

p, _ := persona.Open("fina.persona")
chat, _ := llm.New("openrouter:typesafe/jev-router")
reply, _ := p.Chat(ctx, chat, messages, persona.ChatOptions{Context: block})
```

### As a service

```sh
openpersona serve -addr :8080 -db mem.db -personas ./packs
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
- **Embeddings**: optional. Set `memory.Config.Embedder` to add a vector channel.
- **Language**: `graph.Options.Analyzer` controls text analysis. The default handles Indonesian affixes, with English stemming on top.
- **What to remember**: `memory.Config.ExtractionRules`.

## Layout

```
graph/     storage kernel: nodes, edges, BM25 index, vectors (SQLite, pure Go)
recall/    parallel retrieval channels, rank fusion, LLM rerank
memory/    read-write context engine
persona/   trainer and read-only runtime
llm/       OpenAI-compatible client, JSON helper, usage meter
server/    HTTP API
cmd/openpersona/   CLI
```

## Status

Early. The engines, CLI and server work and are covered by offline tests (`go test ./...`). Not yet done: a benchmark run (LongMemEval / LoCoMo), an ANN vector index, and a license file. See the roadmap in [docs/DESIGN.md](docs/DESIGN.md).
