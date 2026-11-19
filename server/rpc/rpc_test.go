package rpc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"

	v1 "go.vicoworks.com/open-persona/api/gen/openpersona/v1"
	"go.vicoworks.com/open-persona/api/gen/openpersona/v1/openpersonav1connect"
	"go.vicoworks.com/open-persona/graph"
	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/memory"
	"go.vicoworks.com/open-persona/persona"
	"go.vicoworks.com/open-persona/server"
	"go.vicoworks.com/open-persona/server/rpc"
)

const answer = "Take the Haramain train."

// fakeModel trains a one-unit pack and answers chats in two streamed pieces.
type fakeModel struct{}

func (fakeModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	switch system := req.Messages[0].Content; {
	case strings.HasPrefix(system, "You are writing the core identity"):
		return llm.Response{Content: `{"identity":"You are Kora.","principles":[],"style":""}`}, nil
	case strings.HasPrefix(system, "You are compiling"):
		return llm.Response{Content: `{"units":[{"kind":"reference","title":"Haramain train Makkah to Madinah","body":"About 2 hours 15 minutes; economy about SAR 162 before VAT.","tags":["Haramain"]}]}`}, nil
	}
	return llm.Response{Content: answer, Usage: llm.Usage{PromptTokens: 50, CompletionTokens: 5, Calls: 1}}, nil
}

func (m fakeModel) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Response, error) {
	resp, _ := m.Complete(ctx, req)
	for _, piece := range []string{"Take the ", "Haramain train."} {
		if err := onDelta(piece); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

type keys map[string]*server.Principal

func (k keys) Authenticate(_ context.Context, token string) (*server.Principal, error) {
	if p := k[token]; p != nil {
		return p, nil
	}
	return nil, server.ErrUnauthorized
}

func start(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "kora.persona")
	spec := persona.Spec{ID: "kora", Version: "1.0.0", Name: "Kora", Role: "Travel planner"}
	if _, err := (&persona.Trainer{LLM: fakeModel{}}).Train(ctx, spec, []persona.Source{{Name: "saudi", Text: "# Rail\n\nHaramain train.\n"}}, out); err != nil {
		t.Fatal(err)
	}
	p, err := persona.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	store, err := graph.Open(filepath.Join(t.TempDir(), "mem.db"), graph.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.New(memory.Config{Store: store}) // raw-only: no model
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mem.Close(); store.Close() })

	srv := &server.Server{
		Personas:  map[string]*persona.Persona{"kora": p},
		Memory:    mem,
		ChatModel: fakeModel{},
		Jobs:      &server.MemoryJobs{},
		Auth: keys{
			"full":     {KeyID: "k1", Personas: []string{"*"}, Scopes: server.AllScopes},
			"readonly": {KeyID: "k2", Personas: []string{"kora"}, Scopes: []string{server.ScopeKnowledge}},
		},
		Logf: t.Logf,
	}
	rpc.Mount(srv, rpc.Options{Reflection: true})
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func withKey[T any](msg *T, key string) *connect.Request[T] {
	req := connect.NewRequest(msg)
	if key != "" {
		req.Header().Set("Authorization", "Bearer "+key)
	}
	return req
}

func userMsg(text string) []*v1.Message {
	return []*v1.Message{{Role: v1.Role_ROLE_USER, Content: text}}
}

// The same calls over the gRPC protocol (HTTP/2) and the Connect protocol.
func TestPersonaService(t *testing.T) {
	ts := start(t)
	for name, opts := range map[string][]connect.ClientOption{
		"grpc":    {connect.WithGRPC()},
		"connect": {connect.WithProtoJSON()},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c := openpersonav1connect.NewPersonaServiceClient(ts.Client(), ts.URL, opts...)

			list, err := c.ListPersonas(ctx, withKey(&v1.ListPersonasRequest{}, "full"))
			if err != nil || len(list.Msg.GetPersonas()) != 1 || list.Msg.GetPersonas()[0].GetId() != "kora" {
				t.Fatalf("list: %v, %v", list, err)
			}
			info, err := c.GetPersona(ctx, withKey(&v1.GetPersonaRequest{PersonaId: "kora"}, "full"))
			if err != nil || info.Msg.GetPersona().GetVersion() != "1.0.0" || !strings.Contains(info.Msg.GetCore(), "Kora") || info.Msg.GetReport().GetUnits() != 1 {
				t.Fatalf("get persona: %v, %v", info, err)
			}
			k, err := c.GetKnowledge(ctx, withKey(&v1.GetKnowledgeRequest{PersonaId: "kora", Query: "Haramain train Makkah Madinah", Sources: -1}, "readonly"))
			if err != nil || len(k.Msg.GetUnits()) == 0 || !strings.Contains(k.Msg.GetContext(), "SAR 162") || !strings.Contains(k.Msg.GetSystem(), "You are Kora") {
				t.Fatalf("knowledge: %v, %v", k, err)
			}

			chat, err := c.Chat(ctx, withKey(&v1.ChatRequest{PersonaId: "kora", Messages: userMsg("Makkah to Madinah?")}, "full"))
			if err != nil || chat.Msg.GetAnswer().GetReply() != answer || chat.Msg.GetAnswer().GetUsage().GetCalls() != 1 {
				t.Fatalf("chat: %v, %v", chat, err)
			}

			stream, err := c.ChatStream(ctx, withKey(&v1.ChatStreamRequest{PersonaId: "kora", Messages: userMsg("Makkah to Madinah?")}, "full"))
			if err != nil {
				t.Fatal(err)
			}
			var deltas strings.Builder
			var final *v1.Answer
			for stream.Receive() {
				switch ev := stream.Msg().GetEvent().(type) {
				case *v1.ChatStreamResponse_Delta:
					deltas.WriteString(ev.Delta)
				case *v1.ChatStreamResponse_Answer:
					final = ev.Answer
				}
			}
			if err := stream.Err(); err != nil || deltas.String() != answer || final.GetReply() != answer {
				t.Fatalf("stream: deltas %q, final %v, %v", deltas.String(), final, err)
			}

			sub, err := c.SubmitChatJob(ctx, withKey(&v1.SubmitChatJobRequest{PersonaId: "kora", Messages: userMsg("Makkah to Madinah?")}, "full"))
			if err != nil || sub.Msg.GetJob().GetId() == "" {
				t.Fatalf("submit: %v, %v", sub, err)
			}
			var job *v1.Job
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				got, err := c.GetJob(ctx, withKey(&v1.GetJobRequest{JobId: sub.Msg.GetJob().GetId()}, "full"))
				if err != nil {
					t.Fatal(err)
				}
				if job = got.Msg.GetJob(); job.GetStatus() == v1.JobStatus_JOB_STATUS_SUCCEEDED || job.GetStatus() == v1.JobStatus_JOB_STATUS_FAILED {
					break
				}
			}
			if job.GetStatus() != v1.JobStatus_JOB_STATUS_SUCCEEDED || job.GetAnswer().GetReply() != answer {
				t.Fatalf("job: %v", job)
			}
			if _, err := c.GetJob(ctx, withKey(&v1.GetJobRequest{JobId: job.GetId()}, "readonly")); connect.CodeOf(err) != connect.CodeNotFound {
				t.Errorf("another key's job: %v", err)
			}
		})
	}
}

// Errors carry the same meaning as on the HTTP API, as gRPC status codes.
func TestErrorCodes(t *testing.T) {
	ts := start(t)
	ctx := context.Background()
	c := openpersonav1connect.NewPersonaServiceClient(ts.Client(), ts.URL, connect.WithGRPC())
	for _, tc := range []struct {
		name string
		call func() error
		want connect.Code
	}{
		{"no key", func() error {
			_, err := c.ListPersonas(ctx, withKey(&v1.ListPersonasRequest{}, ""))
			return err
		}, connect.CodeUnauthenticated},
		{"missing scope", func() error {
			_, err := c.Chat(ctx, withKey(&v1.ChatRequest{PersonaId: "kora", Messages: userMsg("hi")}, "readonly"))
			return err
		}, connect.CodePermissionDenied},
		{"missing scope, stream", func() error {
			s, err := c.ChatStream(ctx, withKey(&v1.ChatStreamRequest{PersonaId: "kora", Messages: userMsg("hi")}, "readonly"))
			if err != nil {
				return err
			}
			for s.Receive() {
			}
			return s.Err()
		}, connect.CodePermissionDenied},
		{"unknown persona", func() error {
			_, err := c.GetKnowledge(ctx, withKey(&v1.GetKnowledgeRequest{PersonaId: "fina", Query: "x"}, "full"))
			return err
		}, connect.CodeNotFound},
		{"empty query", func() error {
			_, err := c.GetKnowledge(ctx, withKey(&v1.GetKnowledgeRequest{PersonaId: "kora"}, "full"))
			return err
		}, connect.CodeInvalidArgument},
		{"unspecified role", func() error {
			_, err := c.Chat(ctx, withKey(&v1.ChatRequest{PersonaId: "kora", Messages: []*v1.Message{{Content: "hi"}}}, "full"))
			return err
		}, connect.CodeInvalidArgument},
	} {
		if got := connect.CodeOf(tc.call()); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMemoryService(t *testing.T) {
	ts := start(t)
	ctx := context.Background()
	c := openpersonav1connect.NewMemoryServiceClient(ts.Client(), ts.URL, connect.WithGRPC())
	add, err := c.AddMessages(ctx, withKey(&v1.AddMessagesRequest{Scope: "andi", Messages: []*v1.MemoryMessage{
		{Role: "user", Content: "Saya baru pindah ke Bandung bulan lalu."},
	}}, "full"))
	if err != nil || add.Msg.GetStored() != 1 {
		t.Fatalf("add: %v, %v", add, err)
	}
	res, err := c.SearchMemory(ctx, withKey(&v1.SearchMemoryRequest{Scope: "andi", Query: "pindah Bandung"}, "full"))
	if err != nil || len(res.Msg.GetEpisodes()) != 1 || !strings.Contains(res.Msg.GetContext(), "Bandung") {
		t.Fatalf("search: %v, %v", res, err)
	}
	if _, err := c.SearchMemory(ctx, withKey(&v1.SearchMemoryRequest{Query: "x"}, "full")); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty scope: %v", err)
	}
}

// Health checks need no key, so load balancers can use them.
func TestHealth(t *testing.T) {
	ts := start(t)
	c := grpchealth.NewClient(ts.Client(), ts.URL, connect.WithGRPC())
	res, err := c.Check(context.Background(), &grpchealth.CheckRequest{Service: openpersonav1connect.PersonaServiceName})
	if err != nil || res.Status != grpchealth.StatusServing {
		t.Fatalf("health: %v, %v", res, err)
	}
	// The HTTP API is still there, on the same port.
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v, %v", resp, err)
	}
	resp.Body.Close()
	var ce *connect.Error
	_, err = openpersonav1connect.NewPersonaServiceClient(ts.Client(), ts.URL).ListPersonas(context.Background(), withKey(&v1.ListPersonasRequest{}, "nope"))
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || !strings.Contains(ce.Message(), "API key") {
		t.Errorf("bad key over Connect: %v", err)
	}
}
