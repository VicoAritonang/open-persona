# HTTP API

Start the server:

```sh
openpersona serve -addr :8080 -db memory.db -personas ./packs
```

`-personas` is a directory of `*.persona` files; each is served under its file name (`fina.persona` becomes `fina`).

**Authentication.** If `OPENPERSONA_API_KEY` is set, every request except `/healthz` must carry `Authorization: Bearer <key>`; otherwise the response is `401`.

**Errors** are JSON: `{"error": "message"}` with a 4xx or 5xx status. Request bodies are JSON, at most 8 MB.

---

## `GET /healthz`

`200 {"status":"ok"}`. No authentication.

## `POST /v1/memory/{scope}/messages`

Store messages. `{scope}` is your isolation key, usually a user ID.

```json
{
  "messages": [
    {"role": "user", "content": "I moved to Bandung in May.", "name": "Andi", "time": "2026-05-11T10:00:00Z"},
    {"role": "assistant", "content": "Congratulations on the move!"}
  ]
}
```

| Field | | |
|---|---|---|
| `role` | required | `user`, `assistant`, or any speaker label |
| `content` | required | |
| `name` | optional | Speaker name |
| `time` | optional | RFC 3339; when it was said. Default: now. Set it when importing old conversations, so relative dates resolve correctly |

`202 {"stored": 2}`. The messages are searchable immediately; extracted facts follow within seconds.

## `POST /v1/memory/{scope}/search`

```json
{"query": "where does the user live?", "facts": 10, "episodes": 5, "include_history": false, "rerank": false, "max_tokens": 1500}
```

| Field | Default | |
|---|---|---|
| `query` | required | Natural-language question |
| `facts` | 10 | Extracted facts to return |
| `episodes` | 5 | Raw messages to return |
| `include_history` | false | Also return facts that are no longer true |
| `rerank` | false | Reorder with one LLM call |
| `max_tokens` | 0 (no limit) | Budget for `context` |

```json
{
  "facts": [
    {"id": 31, "kind": "fact", "text": "Andi moved to Bandung in early May 2026.", "valid_at": "2026-05-01T00:00:00Z", "score": 0.049, "channels": ["facts", "facts-vector"]}
  ],
  "episodes": [
    {"id": 7, "kind": "episode", "title": "user", "text": "I moved to Bandung in May.", "valid_at": "2026-05-11T10:00:00Z", "score": 0.032, "channels": ["episodes"]}
  ],
  "context": "<memory>\nKnown facts ...\n</memory>"
}
```

`context` is ready to paste into a system prompt; it is `""` when nothing was found. `invalid_at` appears on superseded facts. `channels` lists which retrieval channels found the item.

## `GET /v1/personas`

```json
{"personas": [{"id": "fina", "version": "1.0.0", "name": "Fina", "role": "Finance controller", "description": "...", "expertise": ["..."], "tone": "...", "language": "...", "boundaries": ["..."]}]}
```

## `POST /v1/personas/{name}/prompt`

The system prompt for a query, for use with your own model. Makes no LLM call.

```json
{"query": "who approves a 40 million purchase?"}
```

```json
{"system": "<role>...</role>\n\n<knowledge>...</knowledge>\n\n<instructions>...</instructions>", "units": [{"id": 12, "kind": "rule", "title": "Expense approval limits", "text": "...", "score": 0.05}], "sources": [...]}
```

Optional `units` (at most 64) and `sources` (at most 16; `-1` for none) override the pack's retrieval budget, which defaults to the spec's `retrieve` setting, else 8 units and 2 source excerpts. An agent that calls this endpoint as a knowledge tool, once per sub-question, can ask for a small budget per call (for example `{"query": "Nagoya to Kariya train", "units": 6, "sources": -1}`) and read `units`.

## `POST /v1/personas/{name}/chat`

Answer as the persona, using the server's chat model.

```json
{
  "messages": [
    {"role": "user", "content": "Who approves a 40 million purchase?"}
  ],
  "memory_scope": "user-42",
  "remember": true
}
```

| Field | | |
|---|---|---|
| `messages` | required | The conversation so far, user and assistant turns only. The last one is answered |
| `memory_scope` | optional | Adds what memory holds about this scope to the prompt |
| `remember` | optional | With `memory_scope`: store the last message and the reply afterwards |

```json
{"reply": "...", "units": [{"kind": "rule", "title": "Expense approval limits", "text": "...", "score": 0.05}], "usage": {"prompt_tokens": 1820, "completion_tokens": 240, "calls": 1}}
```

`units` are the knowledge units the answer was grounded on; show them as citations or log them for review.

| Status | Meaning |
|---|---|
| 404 | Unknown persona |
| 502 | The chat model failed |
| 503 | No chat model, or no memory, is configured |
