# The Go client: a persona on a server, or in process

A persona such as Kora reaches a Go program in one of two ways. Both implement the same `openpersona.Persona` interface, so an agent can move from one to the other without changes.

| | On a server | In process |
|---|---|---|
| Package | `go.vicoworks.com/open-persona` (standard library only) | `go.vicoworks.com/open-persona/local` (adds SQLite) |
| Constructor | `openpersona.NewClient(key, opts).Persona("kora")` | `local.Open("kora.persona", opts)` or `local.Fetch(ctx, client, "kora", dir, opts)` |
| Knowledge lookup | One HTTPS call | In process: a few ms, plus the query embedding |
| Answers | The server's chat model | Your own model (`local.Options.Chat`) |
| What you hold | An API key | The pack file (it contains the training material verbatim) |
| Updates | Whatever version the server serves | `Fetch` again; the file name carries the version |

## Usage

```go
import (
	openpersona "go.vicoworks.com/open-persona"
	"go.vicoworks.com/open-persona/local"
)

client := openpersona.NewClient(os.Getenv("OPENPERSONA_API_KEY"), openpersona.ClientOptions{
	BaseURL:   "https://api.example.com",
	StreamURL: "https://stream.example.com", // only if streaming runs on another host
})
kora := client.Persona("kora")
// or: kora, err := local.Fetch(ctx, client, "kora", "/var/lib/myapp/packs", local.Options{Chat: myModel})
// or: kora, err := local.Open("packs/kora.persona", local.Options{Chat: myModel})

// 1. As a tool for your own agent (the recommended integration).
tool, _ := openpersona.NewKnowledgeTool(ctx, kora, openpersona.ToolOptions{})
tools := []any{tool.OpenAI(), getFlights, getHotels, googleSearch} // or tool.Anthropic()
// ... when the model calls tool.Name:
text, _ := tool.Call(ctx, call.Arguments)

// 2. Or let the persona answer.
reply, _ := kora.Chat(ctx, msgs, openpersona.ChatOptions{Context: "Paspor: Indonesia. Flights found: ..."})
reply, _ = kora.Stream(ctx, msgs, openpersona.ChatOptions{}, func(s string) error { fmt.Print(s); return nil })

// 3. Or only the knowledge.
k, _ := kora.Knowledge(ctx, "kereta Nagoya ke Kariya", openpersona.KnowledgeOptions{})
```

- A remote `Chat` defaults to **async**: it submits a job and polls it (`ClientOptions.ChatMode = openpersona.ChatSync` waits on one request instead). No connection is held open while the model writes a long answer, which suits servers behind serverless platforms. `Remote.Submit` plus `Client.Job` or `Client.Wait` give manual control.
- `Download` (and `local.Fetch`) fetches the pack in 3 MiB ranges, verifies its SHA-256 and file header, and skips the download when the file is current. The API key needs the `download` scope.
- Errors from a server are `*openpersona.APIError` with `Status` and a stable `Code` (`"permission_denied"`, `"not_found"`, ...); `openpersona.IsNotFound(err)` covers the common case.
- A local persona connects the pack's embedding model itself using the provider key in the environment (`OPENAI_API_KEY` for `openai:*` models), or takes `local.Options.Embedder`; `NoEmbeddings` turns semantic retrieval off.
- A runnable example: `examples/client` (`go run ./examples/client -mode knowledge|chat|stream|local`).

From other languages, use the server's HTTP API ([HTTP_API.md](HTTP_API.md)) or gRPC ([GRPC.md](GRPC.md)).
