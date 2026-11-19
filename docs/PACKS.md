# Persona packs: the file format and evaluation

A trained persona ships as one file, `fina.persona`: copy it, attach it to a release, bake it into an image, or download it from a server that holds it (`GET /v1/personas/{id}/pack`, `Client.Download`). The runtime is always the same: `persona.Open` (or `local.Open`) serves the file read-only.

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

`spec.json` carries `id` (lowercase letters, digits, hyphens; derived from `name` if missing) and `version` (semantic versioning, default `0.1.0`). A version is immutable: retraining on changed material means a new version. Training is not byte-reproducible (LLM extraction, timestamps), so never ship different bytes under an existing version; a registry such as the one in `platform/` rejects them.

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
