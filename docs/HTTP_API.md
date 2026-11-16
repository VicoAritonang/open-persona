# HTTP API

Start the server:

```sh
openpersona serve -addr :8080 -db memory.db -personas ./packs
```

`-personas` is a directory of `*.persona` files; each is served under its persona id (`fina`). `openpersona serve -cloud` is the hosted mode: API keys, async jobs and usage live in the registry database and packs are pulled on demand; the same handler runs on AWS Lambda (see [HOSTING.md](HOSTING.md)).

**Authentication.** Send `Authorization: Bearer <key>` (or `X-API-Key: <key>`) on every request except `/healthz`. A self-hosted server accepts the single `OPENPERSONA_API_KEY`; the hosted service accepts keys created with `openpersona keys create`, each limited to some personas and some scopes: `knowledge` (retrieval, no LLM), `chat` (answers, streaming, jobs, MCP ask), `download` (the pack file). A missing or wrong key is `401`; a missing scope is `403`; a persona outside the key is `404`.

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

The personas this key may use.

```json
{"personas": [{"id": "kora", "version": "1.0.0", "name": "Kora", "role": "AI travel agent dan perencana perjalanan", "expertise": ["..."]}]}
```

## `GET /v1/personas/{id}`

The pack's manifest: spec, identity core, training report, eval result, models.

## `POST /v1/personas/{id}/knowledge`

What the persona knows about one query. Makes no chat-model call (only a query embedding when the pack has vectors). Scope `knowledge`. `POST /v1/personas/{id}/prompt` is the same endpoint under its older name.

```json
{"query": "kereta Nagoya ke Kariya", "units": 8, "sources": -1}
```

```json
{"persona": "kora", "version": "1.0.0",
 "system": "<role>...</role>\n\n<knowledge>...</knowledge>\n\n<instructions>...</instructions>",
 "context": "\n[reference] Transportasi dalam Aichi\n...",
 "units": [{"id": 812, "kind": "reference", "title": "Transportasi dalam Aichi", "text": "...", "score": 0.05}],
 "sources": []}
```

- `system` is the full persona prompt for your own model.
- `context` is the knowledge alone, the right shape for a tool result.
- `units` (at most 64) and `sources` (at most 16; `-1` for none) override the pack's budget (the spec's `retrieve`, else 8 and 2).

## `POST /v1/personas/{id}/chat`

Answer as the persona with the server's chat model. Scope `chat`.

```json
{"messages": [{"role": "user", "content": "Dari Nagoya ke Kariya naik apa?"}], "context": "Passport: Swedish. Meeting 12 March 14:00.", "stream": false}
```

| Field | | |
|---|---|---|
| `messages` | required | The conversation, user and assistant turns only; the last one (a user turn) is answered |
| `context` | optional | Extra material for the system prompt: what your app knows about the user, live tool results |
| `max_tokens` | optional | |
| `stream` | optional | Answer as server-sent events (also chosen by `Accept: text/event-stream`) |
| `memory_scope`, `remember` | optional | Self-hosted servers with memory: add the user's memory, store the exchange |

```json
{"persona": "kora", "version": "1.0.0", "reply": "...", "units": [{"kind": "reference", "title": "...", "text": "...", "score": 0.05}], "usage": {"prompt_tokens": 3811, "completion_tokens": 90, "calls": 1}}
```

With `"stream": true` the response is `text/event-stream`:

```
event: delta
data: {"text":"Naik JR Tokaido"}

event: delta
data: {"text":", 15–20 menit."}

event: done
data: {"persona":"kora","version":"1.0.0","reply":"Naik JR Tokaido, 15–20 menit.","units":[...],"usage":{...}}
```

On failure the last event is `error` with `{"error": "..."}`. Streaming needs a host that can stream (EC2 in the hosted setup); a buffered Lambda Function URL answers `501` and points to the jobs endpoint.

| Status | Meaning |
|---|---|
| 403 | The key lacks the `chat` scope |
| 404 | Unknown persona, or not one this key may use |
| 502 | The chat model failed |
| 503 | No chat model is configured, or the persona could not be loaded (retry) |

## `POST /v1/personas/{id}/jobs`

The same answer, asynchronously: the request returns at once and the answer is computed outside it (on Lambda, by an asynchronous invocation). Same body as chat, without `stream` and memory. Scope `chat`.

`202 Accepted`, `Location: /v1/jobs/job_...`:

```json
{"id": "job_8f3c...", "persona": "kora", "kind": "chat", "status": "queued", "attempts": 0, "created_at": "..."}
```

## `GET /v1/jobs/{id}`

The job; poll it until `status` is `succeeded` or `failed` (`Retry-After` suggests the wait). `result` is the chat response above. A job is visible only to the key that created it.

```json
{"id": "job_8f3c...", "persona": "kora", "status": "succeeded", "result": {"reply": "...", "units": [...], "usage": {...}}, "finished_at": "..."}
```

## `GET /v1/personas/{id}/pack`

The pack file, to run the persona inside your own backend. Scope `download`. Supports `Range` (the Go client downloads in 3 MiB ranges, which keeps each response under Lambda's 6 MB limit) and identifies the file with `X-Pack-SHA256`, `X-Persona-Id`, `X-Persona-Version` and `ETag`. A pack contains its training material verbatim.

## `POST /mcp`

[Model Context Protocol](https://modelcontextprotocol.io) over streamable HTTP: stateless, JSON responses, no SSE stream (so it also runs on Lambda). Methods: `initialize`, `ping`, `tools/list`, `tools/call`. Each persona the key may use becomes tools:

| Tool | Scope | Does |
|---|---|---|
| `{id}_knowledge` | knowledge | `{"query": "...", "units": 8}` → the knowledge as text; no LLM call |
| `{id}_ask` | chat | `{"question": "...", "context": "..."}` → an answer as the persona |

Configure an MCP client with the URL `https://<host>/mcp` and the header `Authorization: Bearer <key>`.

## CORS

Set `OPENPERSONA_CORS_ORIGINS` (comma-separated, or `*`) to let a browser frontend on another origin call the API. Do not ship a customer's API key to browsers: give the frontend its own key with only the scopes it needs, or call the API from your backend.
