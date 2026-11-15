# Distributing personas: the pack file and the registry

A trained persona ships in two ways, carrying the same bytes:

1. **As a file**: `fina.persona`. Copy it, attach it to a release, bake it into an image.
2. **From the registry**: a Postgres database (Neon in our setup) that stores every version of every pack. `openpersona pull` writes the identical file back to disk.

Either way the runtime is the same: `persona.Open` serves the file read-only.

## The `.persona` file

A pack is one SQLite file (pure Go, no cgo needed to read it) holding the frozen knowledge graph.

| Property | Value |
|---|---|
| Extension | `.persona` (`persona.PackExt`) |
| SQLite `application_id` | `0x4F504E50` ("OPNP", `persona.PackApplicationID`) |
| SQLite `user_version` | pack format, currently `1` (`persona.PackFormat`) |
| Journal | none: compacted with `VACUUM`, `journal_mode=DELETE`, marked frozen |

The header lets a file identify itself before it is read as a graph. `persona.Open` refuses a file with another application ID (a memory database, say), and refuses a format newer than the build understands rather than misreading it. Packs written before the stamp existed (application ID 0) still open, as format 0.

What the file contains:

| Table / meta key | Content |
|---|---|
| `nodes` | `core` (identity block), knowledge units (`principle`, `rule`, `procedure`, `concept`, `reference`, `example`), `source` (verbatim training chunks), `entity` (tags) |
| `edges` | unit → source (`source`), unit → tag (`mentions`) |
| `nodes_fts` | BM25 index (contentless) |
| `vectors` | embeddings, when trained with an embedding model |
| meta `spec` | the persona spec, including `id` and `version` |
| meta `report`, `eval` | training report; held-out evaluation results |
| meta `trained_at`, `train_model`, `embed_model` | provenance |

**The pack contains the training material verbatim** (the `source` nodes), so anyone holding the file can read it. Treat a pack as being as confidential as the documents it was trained on.

### Identity and versions

`spec.json` carries `id` (lowercase letters, digits, hyphens; derived from `name` if missing) and `version` (semantic versioning, default `0.1.0`). A version is immutable: retraining on changed material means a new version. Training is not byte-reproducible (LLM extraction, timestamps), so the registry rejects different bytes under an existing version instead of silently replacing them.

### Evaluation

An eval set is a JSON list of held-out questions with the facts a good answer needs:

```json
[{"q": "Saya mau beli server 40 juta, siapa yang menyetujui?", "expect": ["CFO", "25.000.000"]}]
```

`"a|b"` accepts either string. Matching ignores case, spaces and punctuation. Two ways to run it:

```sh
openpersona train ... -eval eval.json          # scored before freezing; stored in the pack
openpersona eval -pack fina.persona -set eval.json [-answer] [-v]
```

Retrieval scoring needs no chat model (only a query embedding when the pack has vectors). `-answer` also asks the chat model each question and checks the answers: one call per question.

## The registry (Postgres / Neon)

### Setup

```sh
# .env
NEON_DB_URL=postgresql://user:pass@ep-....-pooler.<region>.aws.neon.tech/neondb?sslmode=require&channel_binding=require

openpersona registry migrate     # creates schema "openpersona"; safe to re-run
openpersona registry status
```

The URL is read from `OPENPERSONA_REGISTRY_URL`, `NEON_DB_URL` or `DATABASE_URL` (first set wins). `OPENPERSONA_REGISTRY_SCHEMA` overrides the schema name. Pooled Neon endpoints (`-pooler` in the host) are supported: the client then avoids named prepared statements.

### Commands

```sh
openpersona push -pack fina.persona [-status draft] [-visibility private|unlisted|public] [-owner "..."] [-notes "..."]
openpersona pull -id fina [-version 1.0.0] [-out fina.persona]     # default: latest published
openpersona registry list
openpersona registry versions -id fina
openpersona registry set-status -id fina -version 1.0.0 -status deprecated|yanked|published|draft
openpersona serve -personas ./packs -registry fina,other@1.2.0     # pull at startup, then serve
```

- `push` of identical bytes is a no-op; different bytes under an existing version fail.
- `pull` verifies the SHA-256 and opens the pack before replacing the target file, and skips the download when the file on disk already has that hash (so `serve -registry` is a cache).
- **Latest** is the highest published version by semver. Drafts and deprecated versions are never latest; yanked versions are refused on pull.

### Schema

All tables live in the `openpersona` schema. The DDL is in [`registry/migrations/001_init.sql`](../registry/migrations/001_init.sql); applied migrations are recorded in `schema_migrations`.

| Table | One row per | Key columns |
|---|---|---|
| `pack_blobs` | distinct pack file | `sha256` (PK), `size_bytes`, `data bytea` |
| `personas` | persona | `id` (PK), `name`, `role`, `description`, `language`, `owner`, `visibility`, `latest_version` |
| `persona_versions` | trained build | (`persona_id`, `version`) PK, `sha256` → `pack_blobs`, `status`, `format`, `spec jsonb`, `core`, `report jsonb`, `eval jsonb`, `embed_model`, `train_model`, `trained_at`, `published_at`, `published_by`, `notes` |
| `persona_units` | knowledge unit of a version | (`persona_id`, `version`, `unit_id`) PK, `kind`, `title`, `body`, `tags text[]`; full-text index |

`persona_units` is a copy for people, not for the runtime: review what a version knows, search it, or diff two versions in SQL.

```sql
-- Units added in 1.1.0 compared with 1.0.0
SELECT kind, title FROM openpersona.persona_units WHERE persona_id = 'fina' AND version = '1.1.0'
EXCEPT
SELECT kind, title FROM openpersona.persona_units WHERE persona_id = 'fina' AND version = '1.0.0';

-- Which versions mention a topic
SELECT version, title FROM openpersona.persona_units
WHERE persona_id = 'fina' AND to_tsvector('simple', title || ' ' || body) @@ plainto_tsquery('simple', 'kas kecil');
```

### As a library

```go
reg, err := registry.Open(ctx, registry.URLFromEnv(), registry.Options{})
defer reg.Close()
v, created, err := reg.Push(ctx, "fina.persona", registry.PushOptions{Notes: "..."})
v, downloaded, err := reg.Pull(ctx, "fina", "", "packs/fina.persona")
p, err := persona.Open("packs/fina.persona")
```

### Limits and next steps

- Pack bytes sit in `bytea`. That is fine for packs of a few MB to a few hundred MB (Fina 1.0.0 is 3.2 MB and Kora 1.0.0 is 14.4 MB, about two thirds of each is vectors). For much larger packs, move `pack_blobs.data` to object storage (S3/R2) and keep the hash and URL in the table.
- `visibility` and `owner` are recorded but not enforced: the registry trusts whoever holds the database URL. Distributing packs to third parties needs an HTTP endpoint with per-client keys in front of it, and pack signing so a client can verify the publisher.
- Memory (`memory.Engine`) still uses SQLite; only personas are in Postgres.
