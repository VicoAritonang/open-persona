# Integrating OpenPersona into an AI agent

Two ways in:

- **Go library**: `go get go.vicoworks.com/openpersona`. Everything runs inside your process.
- **HTTP service**: run `openpersona serve` (or the Docker image) and call it from any language. Reference: [HTTP_API.md](HTTP_API.md).

Both expose the same two engines. **Memory** answers "what do we know about this user?" **Persona** answers "who is the agent, and what does it know about this topic?"

## 1. Setup (Go)

```go
import (
    "go.vicoworks.com/openpersona/graph"
    "go.vicoworks.com/openpersona/llm"
    "go.vicoworks.com/openpersona/memory"
    "go.vicoworks.com/openpersona/persona"
)

llm.LoadDotEnv() // optional: reads .env into the environment

store, err := graph.Open("memory.db", graph.Options{})
extractor, err := llm.New("openai:gpt-4o-mini")
embedder, err := llm.New("openai:text-embedding-3-small")

mem, err := memory.New(memory.Config{
    Store:     store,
    Extractor: extractor, // nil = raw-only mode, no LLM cost
    Embedder:  embedder,  // nil = lexical + graph retrieval only
})
defer store.Close()
defer mem.Close() // runs first: finishes queued extraction
```

Create one `Engine` per database file and share it; it is safe for concurrent use. Do not open the same file from two processes that both write.

## 2. The agent turn

The standard loop is retrieve, answer, remember:

```go
func turn(ctx context.Context, userID, userMsg string, history []llm.Message) (string, error) {
    // 1. Retrieve. No LLM call; about 2 ms lexical, plus one embedding call if enabled.
    res, err := mem.Search(ctx, userID, userMsg, memory.SearchOptions{})
    if err != nil {
        return "", err
    }

    // 2. Answer, with the memory block in the system prompt.
    system := "You are a helpful assistant.\n\n" + res.Context(1500) // token budget
    messages := append([]llm.Message{{Role: "system", Content: system}}, history...)
    messages = append(messages, llm.Message{Role: "user", Content: userMsg})
    resp, err := chat.Complete(ctx, llm.Request{Messages: messages})
    if err != nil {
        return "", err
    }

    // 3. Remember. Returns as soon as the messages are stored; fact
    // extraction runs in the background.
    err = mem.Add(ctx, userID, []memory.Message{
        {Role: "user", Content: userMsg},
        {Role: "assistant", Content: resp.Content},
    })
    return resp.Content, err
}
```

Things to know:

- **Scope** is the isolation boundary. Use a user ID for personal memory, `user:session` for per-conversation memory, or a team ID for shared memory. Scopes never see each other.
- **`Add` is asynchronous for facts.** Raw messages are searchable when `Add` returns; extracted facts appear a few seconds later. Call `mem.Flush(ctx)` only when you need to wait (tests, batch imports); it also returns background errors. In a server, set `Config.OnError` to log them.
- **Batching saves money.** One `Add` call is one extraction call. Adding a user/assistant pair per turn is the normal pattern; importing a whole past conversation in one `Add` is cheaper still.
- **`Context(n)`** renders facts first, then raw messages, and stops at about `n` tokens. Use `res.Facts` and `res.Episodes` directly if you want your own format.
- **History**: `SearchOptions{IncludeHistory: true}` also returns facts that were later superseded, labelled "no longer true since ...". Use it for questions about the past.
- **Rerank**: `SearchOptions{Rerank: true}` spends one LLM call (set `Config.Reranker`) to reorder results. Worth it when the context budget is tight.

## 3. Memory as agent tools

If your agent decides for itself when to look things up, expose memory as tools instead of injecting it every turn. Definitions in the JSON-schema form most function-calling APIs accept:

```json
[
  {
    "name": "memory_search",
    "description": "Search long-term memory about the current user: facts they told you, preferences, plans, and past conversations. Call this before answering anything that may depend on what the user said earlier.",
    "parameters": {
      "type": "object",
      "properties": {
        "query": {"type": "string", "description": "What you want to know, as a natural-language question"},
        "include_history": {"type": "boolean", "description": "Also return facts that are no longer true. Use for questions about the past."}
      },
      "required": ["query"]
    }
  },
  {
    "name": "memory_save",
    "description": "Save something worth remembering about the user for future conversations. Write it as a complete sentence.",
    "parameters": {
      "type": "object",
      "properties": {
        "note": {"type": "string"}
      },
      "required": ["note"]
    }
  }
]
```

Handlers:

```go
case "memory_search":
    res, err := mem.Search(ctx, userID, args.Query, memory.SearchOptions{IncludeHistory: args.IncludeHistory})
    return res.Context(1200), err // "" means nothing was found

case "memory_save":
    return "saved", mem.Add(ctx, userID, []memory.Message{{Role: "user", Content: args.Note}})
```

Take `userID` from your session, never from the model's arguments, so an agent cannot be talked into reading another user's memory.

Even with tools, still `Add` every turn in the background: the model will not reliably call `memory_save` for everything that matters.

## 4. Persona

### Train (offline, in CI or a lab)

```go
trainer := persona.Trainer{
    LLM:        extractor,
    Embedder:   embedder,
    EmbedModel: "openai:text-embedding-3-small", // recorded in the pack
}
report, err := trainer.Train(ctx,
    persona.Spec{
        Name: "Fina",
        Role: "Finance controller",
        Description: "Helps employees apply the company finance policy.",
        Tone: "precise, calm, direct",
        Boundaries: []string{"Never approve payments or exceptions."},
    },
    []persona.Source{{Name: "policy", Text: policyMarkdown}},
    "fina.persona")
```

Or from the command line: `openpersona train -spec spec.json -src ./docs -out fina.persona`.

The pack is a single file. Review it (`openpersona inspect`), commit or upload it like any build artifact, and ship it with your service. To change the persona, retrain and replace the file.

Source material works best as markdown with headings: each chunk keeps its heading path, which gives the extractor and the index their context.

### Serve (read-only)

```go
p, err := persona.Open("fina.persona")
if p.EmbedModel != "" {
    p.Embedder, _ = llm.New(p.EmbedModel) // must be the model the pack was trained with
}

// Option A: let OpenPersona call the model.
reply, err := p.Chat(ctx, chat, conversation, persona.ChatOptions{})
fmt.Println(reply.Content)

// Option B: get the system prompt and use your own model or framework.
k, err := p.Retrieve(ctx, userMsg, persona.RetrieveOptions{})
system := p.SystemPrompt(k)
```

`conversation` is the user/assistant turns without a system message. `Chat` retrieves on the last two user turns, so short follow-ups keep their topic.

## 5. Persona and memory together

The persona supplies who the agent is and what it knows; memory supplies who it is talking to.

```go
res, _ := mem.Search(ctx, userID, userMsg, memory.SearchOptions{})
reply, err := p.Chat(ctx, chat, conversation, persona.ChatOptions{
    Context: res.Context(1500), // appended after the persona prompt
})
mem.Add(ctx, userID, []memory.Message{
    {Role: "user", Content: userMsg},
    {Role: "assistant", Content: reply.Content},
})
```

Over HTTP this is one call: `POST /v1/personas/{name}/chat` with `memory_scope` and `remember`.

## 6. From Python or TypeScript

```python
import requests

BASE = "http://localhost:8080"
H = {"Authorization": f"Bearer {API_KEY}"}

requests.post(f"{BASE}/v1/memory/user-42/messages", headers=H, json={
    "messages": [{"role": "user", "content": "I moved to Bandung in May."}]})

ctx = requests.post(f"{BASE}/v1/memory/user-42/search", headers=H, json={
    "query": "where does the user live?", "max_tokens": 1500}).json()["context"]
```

```ts
const r = await fetch(`${BASE}/v1/personas/fina/chat`, {
  method: "POST",
  headers: { "Content-Type": "application/json", Authorization: `Bearer ${API_KEY}` },
  body: JSON.stringify({
    messages: [{ role: "user", content: "Who approves a 40 million purchase?" }],
    memory_scope: "user-42",
    remember: true,
  }),
});
const { reply } = await r.json();
```

## 7. Customizing

| Need | Use |
|---|---|
| Another model or provider | Implement `llm.Client` (one method), or add an entry to `llm.Providers` for any OpenAI-compatible endpoint |
| Control what gets remembered | `memory.Config.ExtractionRules`, e.g. "Only remember facts relevant to customer support. Never store payment card numbers." |
| Another language's morphology | `graph.Options.Analyzer`: return the terms to index for each token. Must be the same on every open of a file |
| An extra retrieval signal | Build a `recall.Channel` and fuse it with the built-in ones via `recall.Fuse` |
| Exactly one LLM call per write | `memory.Config.SkipReconcile` |
| No LLM at all | Leave `Extractor` nil: raw messages with BM25 (plus vectors if `Embedder` is set) |

## 8. Production notes

- **Authentication**: set `OPENPERSONA_API_KEY`; without it the HTTP API is open. Put TLS in front (a reverse proxy or your platform's ingress).
- **Storage**: one SQLite file in WAL mode, one writing process. Back it up with `sqlite3 memory.db ".backup copy.db"` or a volume snapshot. Persona packs are immutable and can be copied freely.
- **Shutdown**: call `mem.Close()` before exit so queued extraction finishes. `openpersona serve` does this on SIGINT.
- **Scale**: vector search scans the vectors of one scope, which is fine up to roughly 100,000 vectors per scope. Lexical and graph search use indexes.
- **Cost**: per `Add`, one extraction call on the batch (plus a small second call when a new fact may contradict an old one) and one embedding call. Per `Search`, one embedding call if embeddings are on, otherwise none.
- **Failure modes**: if the embedding API is down, search silently falls back to lexical + graph. If extraction fails, the raw messages are still stored and searchable; the error goes to `OnError` and `Flush`.
- **Privacy**: there is no scope-deletion API yet. Until there is, delete a user's data with SQL on the `nodes`, `edges`, `vectors` and `nodes_fts` tables.
