# gRPC

`openpersona serve` serves the API as gRPC services on the same port as the HTTP API. Use it from any language with a gRPC library: generate a client from the `.proto` files and call it.

```
openpersona.v1.PersonaService   ListPersonas, GetPersona, GetKnowledge, Chat, ChatStream, SubmitChatJob, GetJob
openpersona.v1.MemoryService    AddMessages, SearchMemory
grpc.health.v1.Health           Check (no API key needed)
grpc.reflection.v1.ServerReflection   (on by default; serve -reflection=false turns it off)
```

The contract is [`api/proto/openpersona/v1`](../api/proto/openpersona/v1); the rules for changing it are in [`api/README.md`](../api/README.md).

## Protocols

The server is built on [Connect](https://connectrpc.com), so one handler speaks three protocols:

| Protocol | Transport | For |
|---|---|---|
| gRPC | HTTP/2 (cleartext h2c, or TLS in front) | generated gRPC clients in any language, grpcurl, Postman |
| gRPC-Web | HTTP/1.1 or HTTP/2 | browsers |
| Connect | HTTP/1.1 or HTTP/2, JSON or Protobuf body | `curl`; platforms that buffer responses (serverless) |

## Authentication

Send the API key as metadata, the same key as for HTTP:

```
authorization: Bearer <key>
```

(`x-api-key: <key>` also works.) Errors are standard gRPC status codes; the table in [`api/README.md`](../api/README.md#errors) maps them to the HTTP codes.

## Try it

```sh
OPENPERSONA_API_KEY=change-me openpersona serve -personas ./packs

grpcurl -plaintext localhost:8080 list
grpcurl -plaintext -H "authorization: Bearer change-me" localhost:8080 openpersona.v1.PersonaService/ListPersonas
grpcurl -plaintext -H "authorization: Bearer change-me" \
  -d '{"persona_id": "kora", "query": "kereta Nagoya ke Kariya", "units": 6}' \
  localhost:8080 openpersona.v1.PersonaService/GetKnowledge
grpcurl -plaintext -H "authorization: Bearer change-me" \
  -d '{"persona_id": "kora", "messages": [{"role": "ROLE_USER", "content": "Nagoya ke Kariya naik apa?"}]}' \
  localhost:8080 openpersona.v1.PersonaService/ChatStream
```

The Connect protocol needs nothing but HTTP:

```sh
curl -H "authorization: Bearer change-me" -H "content-type: application/json" \
  -d '{"persona_id": "kora", "query": "kereta Nagoya ke Kariya"}' \
  http://localhost:8080/openpersona.v1.PersonaService/GetKnowledge
```

## Generate a client

With [buf](https://buf.build/docs/installation) and its remote plugins, from a checkout of this repository. Python:

```yaml
# buf.gen.python.yaml
version: v2
plugins:
  - remote: buf.build/protocolbuffers/python
    out: gen/python
  - remote: buf.build/protocolbuffers/pyi
    out: gen/python
  - remote: buf.build/grpc/python
    out: gen/python
```

```sh
buf generate --template buf.gen.python.yaml
```

```python
import grpc
from openpersona.v1 import persona_service_pb2 as pb, persona_service_pb2_grpc as rpc

channel = grpc.insecure_channel("localhost:8080")
personas = rpc.PersonaServiceStub(channel)
k = personas.GetKnowledge(pb.GetKnowledgeRequest(persona_id="kora", query="kereta Nagoya ke Kariya"),
                          metadata=[("authorization", "Bearer change-me")])
print(k.context)
```

TypeScript (Node or browser), with [Connect for ES](https://connectrpc.com/docs/web/getting-started):

```yaml
# buf.gen.ts.yaml
version: v2
plugins:
  - remote: buf.build/bufbuild/es
    out: gen/ts
    opt: target=ts
```

```ts
import { createClient } from "@connectrpc/connect";
import { createGrpcTransport } from "@connectrpc/connect-node"; // or createConnectTransport from @connectrpc/connect-web
import { PersonaService } from "./gen/ts/openpersona/v1/persona_service_pb";

const client = createClient(PersonaService, createGrpcTransport({ baseUrl: "http://localhost:8080" }));
const k = await client.getKnowledge(
  { personaId: "kora", query: "kereta Nagoya ke Kariya" },
  { headers: { authorization: "Bearer change-me" } },
);
```

Java, Kotlin, C#, Swift, Rust and others work the same way with their plugins (`buf.build/grpc/java`, ...). Pin the commit or tag of this repository you generate from: the generated client then matches that version of the contract, and every later v1 server stays compatible with it.

Go programs can import the generated code directly: `go.vicoworks.com/open-persona/api/gen/openpersona/v1` and its `openpersonav1connect` client.

## Deploying

- Plain gRPC needs HTTP/2 end to end. `openpersona serve` accepts cleartext HTTP/2 (h2c) next to HTTP/1.1. Terminate TLS in a proxy that forwards HTTP/2 to it, for example Caddy:
  ```
  api.example.com {
  	reverse_proxy h2c://127.0.0.1:8080 {
  		flush_interval -1
  	}
  }
  ```
- `ChatStream` needs a host that streams: no response buffering between the server and the client. On a buffering host (a serverless function URL), use `SubmitChatJob` and `GetJob`, or unary `Chat` over the Connect protocol.
- A load balancer's health check can call `grpc.health.v1.Health/Check` or `GET /healthz`; neither needs a key.
- Request messages are limited to 8 MiB (`rpc.Options.ReadMaxBytes`).

## Embedding in your own Go server

```go
srv := &server.Server{Personas: packs, ChatModel: model, Auth: myAuth}
rpc.Mount(srv, rpc.Options{Reflection: true, Interceptors: []connect.Interceptor{myLogging}})
http.ListenAndServe(":8080", srv.Handler()) // set http.Server.Protocols for h2c
```
