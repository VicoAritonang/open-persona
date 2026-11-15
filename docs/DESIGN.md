# Design: research, decisions, roadmap

## 1. What the research says (November 2026)

Published benchmark numbers in this field are mostly self-reported and conflict with each other, so the figures below are directional, not a ranking.

**Structured-memory systems are expensive to write to.**
- Zep / Graphiti builds a bi-temporal knowledge graph: every edge has a validity interval, and contradicted facts are invalidated rather than deleted. Retrieval is hybrid (embeddings, BM25, graph traversal) with no LLM call. The cost is on ingestion, which uses LLM calls for entity extraction, entity resolution, edge extraction and edge deduplication. Zep's hosted product bills per episode; only Graphiti is self-hostable. ([Zep paper](https://www.emergentmind.com/papers/2501.13956), [Graphiti overview](https://help.getzep.com/graphiti/graphiti/overview/), [pricing comparison](https://atlan.com/know/zep-vs-mem0/))
- Mem0 originally ran two LLM passes per write (extract, then reconcile with ADD/UPDATE/DELETE). Its 2026 algorithm collapses this to a single ADD-only pass that keeps old states next to new ones, and its search now fuses semantic, BM25 and entity signals. ([Mem0 write-up](https://mem0.ai/blog/mem0-the-token-efficient-memory-algorithm))
- Microsoft GraphRAG-style indexing is the costliest of all; LightRAG, HippoRAG 2 and LazyGraphRAG exist to cut that bill, the last by deferring work to query time. ([LinearRAG](https://arxiv.org/pdf/2510.10114), [RAGU](https://arxiv.org/pdf/2607.11683))

**The expensive structure buys less than advertised.**
- MemDelta, a controlled-baseline study on LongMemEval-S, reports that Mem0 matches plain verbatim RAG (72.7% vs 73.9%) at about 50x the write cost, and that swapping only the embedding model moves accuracy by 6 points. ([MemDelta](https://arxiv.org/pdf/2606.29914))
- SmartSearch stores raw conversation with no ingestion-time LLM at all and reports 93.5% on LoCoMo and 88.4% on LongMemEval-S. Its finding: recall is already 98.6% with simple retrieval; the bottleneck is ranking what fits the token budget. ([SmartSearch](https://arxiv.org/abs/2603.15599))
- BM25 alone is a strong baseline on LongMemEval-S; adding dense retrieval gives a small gain, and an off-the-shelf cross-encoder can hurt. ([fusion study](https://arxiv.org/pdf/2606.04194))
- An audit found the GPT-4o judge used in vendor benchmarks accepts a large share of deliberately wrong answers, and independent runs of vendor systems score far below vendor claims. ([independent benchmark notes](https://dev.to/everest_an/-i-benchmarked-ai-agent-memory-in-2026-and-the-numbers-tell-a-different-story-than-the-marketing-2ae4))

**What does help.**
- Atomic, self-contained facts as the memory unit (AtomMem). ([AtomMem](https://arxiv.org/html/2606.19847))
- Temporal validity and several retrieval strategies run in parallel (Hindsight, Zep). ([Hindsight](https://arxiv.org/pdf/2512.12818))
- For personas: separating expertise from behavior, and storing procedures explicitly (COLLEAGUE.SKILL, Procedural Graphs, Mem^p). None of these freezes the result for a read-only runtime; that combination is what the persona engine here does. ([COLLEAGUE.SKILL](https://arxiv.org/abs/2605.31264), [Procedural Graphs](https://arxiv.org/pdf/2609.09153))

## 2. Decisions

| Decision | Why |
|---|---|
| **Raw first.** Every message is stored verbatim and BM25-searchable before any LLM runs. | Raw text is the strongest cheap baseline and a safety net when extraction misses something. Raw-only mode costs nothing. |
| **One extraction call per batch**, producing atomic facts, their entities, dates, and which shown facts they supersede. | Collapses Graphiti's multi-call pipeline. Entity resolution is a normalized-name key, not an LLM call. |
| **Conditional reconcile call.** Only when a new fact resembles an existing fact the extractor was not shown. | "I joined Gojek" shares no words with "Rina works at Tokopedia"; the extracted fact does. Found by a failing test. |
| **Bi-temporal, invalidate-don't-delete.** | Zep's best idea. Keeps history queryable (`IncludeHistory`). |
| **Retrieval without an LLM**: parallel channels (facts BM25, raw BM25, entity graph, optional vectors) fused by reciprocal rank. | Goroutines make the fan-out free; rank fusion needs no score calibration. |
| **Hub entities skipped in the graph channel.** | An entity most facts mention (the user's own name) carries no signal. Found in a live test. |
| **Embeddings and LLM rerank are optional tiers.** | The research says they are increments, not foundations. |
| **Pluggable analyzer, Indonesian affix handling by default.** | "menyetujui" must match "persetujuan". Found in a live test: the approval rule was not retrieved without it. |
| **Persona = compile then freeze.** Map (parallel extraction per chunk), reduce (merge by title), synthesize an identity core, write one read-only file. | Training is where cost and review belong. Runtime cannot alter the persona, and a pack is a reviewable, versionable artifact. |
| **The pack file is the unit of distribution; the registry stores it, not a re-modelled copy.** Postgres (Neon) holds the bytes content-addressed by SHA-256, plus metadata and a units table for review. `id@version` is immutable. | One artifact, two channels: a pulled pack is byte-identical to the file, so serving, review and audit never diverge. Training is not reproducible, so overwriting a version would silently change what clients run. |
| **Broad personas get a bigger retrieval budget and one channel per part of the request.** The spec's `retrieve` sets the pack's default (Kora: 20 units, 3 excerpts). A request with several sentences also runs a lexical and a vector channel per sentence (at most 4), fused with the whole-query channels. | A trip request touches a passport, an airport, a festival and three cities at once. With one channel set, the strongest part filled the budget: Kora answered "not in my knowledge" for a Nagoya–Kyoto fare that was in the pack. Found in a live test. |
| **Same title, different body: keep both.** The reduce step merges units by title only when their bodies share at least half their keywords; otherwise the newer one is qualified by its section ("Opening hours (Kinkaku-ji)"). | In a corpus of many places, titles like "Jam buka" recur, and merging kept only the longest body. |
| **Go, SQLite (pure Go, no cgo), one file per graph.** | One static binary, trivial to embed and self-host. Go over Rust: the workload is I/O-bound LLM calls and index lookups, where goroutines and build simplicity matter more than raw CPU. |

### Cost tiers (memory)

| Tier | Ingestion | Retrieval |
|---|---|---|
| 0 Raw only | no LLM | BM25 |
| 1 Facts + graph (default) | 1 call per batch, sometimes 2 | BM25 + graph, no LLM |
| 2 + embeddings | + embedding calls | + vector channel |
| 3 + rerank | same | + 1 LLM call |

## 3. Measured so far

Small runs only, on the examples in this repository, with gpt-4o-mini for extraction and Jev (via OpenRouter) for persona answers.

| Run | Result |
|---|---|
| Train finance persona (1 document, 7 chunks) | 8 calls, 4,518 prompt + 1,993 completion tokens, 20 s; 26 units, 54 entities, 86 KB pack. About $0.002 at gpt-4o-mini list price. |
| Train Fina 1.0.0 (12 documents, 67 chunks, with embeddings) | 68 calls, 37,685 prompt + 19,375 completion tokens, 77 s; 274 units (173 rules), 374 entities, 3.2 MB pack (about 2 MB of it vectors). About $0.017 at gpt-4o-mini list price. |
| Held-out eval, Fina 1.0.0 (30 questions, retrieval only) | 28/30 questions with every expected fact in the prompt, 93% of expected facts; 2.8 s for all 30. Misses: "Pelanggan telat bayar 65 hari" (the dunning steps say "lewat jatuh tempo", not "telat") and "Parkir tidak ada struknya" (the no-receipt form rule ranked outside the top 8). |
| Train Kora 1.0.0 (travel agent; 30 documents, about 29,500 words, 228 chunks, with embeddings) | 229 calls, 163,936 prompt + 114,464 completion tokens, 5 min 37 s with 8 workers; 1,318 units (533 rules, 245 references, 150 procedures), 1,160 entities, 14.4 MB pack (about 9.5 MB vectors). About $0.09 at gpt-4o-mini list price. |
| Held-out eval, Kora 1.0.0 (40 questions, 20 units per prompt) | 40/40 questions with every expected fact in the prompt; 5 s for all 40. |
| Kora answers (Jev, one-shot chat) | "Rp 20 juta, a week in Japan from Jakarta, a summer festival": an open-jaw route timed to Gion Matsuri, no JR Pass, an itemized budget, an hourly plan, nothing claimed as booked. "Swedish passport, meeting in Kariya at 14:00, then Kyoto": before per-sentence channels it marked three fares in the pack as unknown; after, the fares and the time-zone shift were right, but it still missed the Swedish visa rule and the Stockholm–Japan corridor. Agents should query the knowledge per sub-question (examples/travel/README.md). |
| Push / pull Fina 1.0.0 to Neon (pooled endpoint) | Byte-identical round trip (SHA-256 verified); re-push and re-pull are no-ops. |
| Ingest 10 messages | 1 call, 756 + 334 tokens; 7 facts, 9 entities |
| Ingest 1 message that changes employer | 2 calls, 783 + 137 tokens; the old "works remotely at Tokoku" fact invalidated |
| Retrieval | about 2 ms per query including process start, no LLM |

| Same query with and without embeddings (text-embedding-3-small) | "Rekomendasi makan siang, ada pantangan?" returned 0 facts lexically; with the vector channel the fact "Andi alergi kacang" ranks first. Search then takes about 700 ms, almost all of it the embedding API call. |
| Persona query with no shared words ("Supplier minta transfer ke bank lain") | With embeddings, the vendor bank-change rule is retrieved and the answer is correct. |

Observed limits:
- Lexical retrieval alone misses pure synonyms ("pantangan" vs "alergi"); the raw message was still found. Embeddings close this gap at the price of a network call per search.
- Supersession is the extractor's judgment: on the employer change it kept "started at Tokoku on 2026-03-02" as a historical event and added "resigned from Tokoku", rather than invalidating the old fact.
- One extracted date was off (a 15 June message dated 1 June).
- The chat model sometimes adds generic best practice beyond the pack. The prompt now asks it to label such advice; it mostly does.

These are functional checks, not a benchmark. No claim is made yet about LongMemEval or LoCoMo.

## 4. Roadmap

1. **Benchmark honestly.** LongMemEval-S and LoCoMo subsets, with the controls MemDelta recommends: same reader model, same judge, a full-context baseline and a plain-RAG baseline, token and cost per question reported next to accuracy.
2. **Ranking.** The research points here, not at more structure: adjacent-message expansion, recency-aware tie-breaking, query-time date filters ("last month").
3. **Embeddings tier.** Measure what the vector channel adds on a benchmark, cache query embeddings, and add an ANN index behind `SearchVector` for large scopes.
4. **Persona depth.** Cross-unit relations (rule to exception, procedure to rule). Done: a held-out question set scored at training time (`train -eval`), units copied to the registry so versions can be diffed in SQL.
5. **Operations.** Done: a Postgres (Neon) registry for packs. Next: an HTTP distribution endpoint with per-client keys and signed packs, a Postgres backend for memory, scope deletion (privacy), metrics, SDK examples for other languages against the HTTP API.
