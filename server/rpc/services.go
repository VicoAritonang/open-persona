package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "go.vicoworks.com/open-persona/api/gen/openpersona/v1"
	"go.vicoworks.com/open-persona/api/gen/openpersona/v1/openpersonav1connect"
	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/memory"
	"go.vicoworks.com/open-persona/persona"
	"go.vicoworks.com/open-persona/server"
)

type personaService struct{ s *server.Server }

var _ openpersonav1connect.PersonaServiceHandler = (*personaService)(nil)

func (p *personaService) ListPersonas(ctx context.Context, _ *connect.Request[v1.ListPersonasRequest]) (*connect.Response[v1.ListPersonasResponse], error) {
	specs, err := p.s.ListPersonas(ctx)
	if err != nil {
		return nil, toConnect(err)
	}
	out := &v1.ListPersonasResponse{Personas: make([]*v1.Persona, len(specs))}
	for i, sp := range specs {
		out.Personas[i] = specProto(sp)
	}
	return connect.NewResponse(out), nil
}

func (p *personaService) GetPersona(ctx context.Context, req *connect.Request[v1.GetPersonaRequest]) (*connect.Response[v1.GetPersonaResponse], error) {
	m, err := p.s.Manifest(ctx, req.Msg.GetPersonaId())
	if err != nil {
		return nil, toConnect(err)
	}
	out := &v1.GetPersonaResponse{
		Persona: specProto(m.Spec), Format: m.Format, Core: m.Core,
		TrainedAt: timestamp(m.TrainedAt), TrainModel: m.TrainModel, EmbedModel: m.EmbedModel,
	}
	if r := m.Report; r != nil {
		out.Report = &v1.TrainingReport{Sources: int32(r.Sources), Chunks: int32(r.Chunks), Units: int32(r.Units), Entities: int32(r.Entities),
			ByKind: make(map[string]int32, len(r.ByKind))}
		for k, n := range r.ByKind {
			out.Report.ByKind[k] = int32(n)
		}
	}
	if e := m.Eval; e != nil {
		out.Eval = &v1.EvalSummary{Cases: int32(e.Cases), Passed: int32(e.Passed), Coverage: e.Coverage,
			AnswerPassed: int32(e.AnswerPassed), AnswerCoverage: e.AnswerCoverage}
	}
	return connect.NewResponse(out), nil
}

func (p *personaService) GetKnowledge(ctx context.Context, req *connect.Request[v1.GetKnowledgeRequest]) (*connect.Response[v1.GetKnowledgeResponse], error) {
	m := req.Msg
	k, err := p.s.Knowledge(ctx, m.GetPersonaId(), server.KnowledgeRequest{Query: m.GetQuery(), Units: int(m.GetUnits()), Sources: int(m.GetSources())})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.GetKnowledgeResponse{
		PersonaId: k.Persona, Version: k.Version, System: k.System, Context: k.Context,
		Units: itemsProto(k.Units), Sources: itemsProto(k.Sources),
	}), nil
}

func (p *personaService) Chat(ctx context.Context, req *connect.Request[v1.ChatRequest]) (*connect.Response[v1.ChatResponse], error) {
	m := req.Msg
	msgs, err := messages(m.GetMessages())
	if err != nil {
		return nil, err
	}
	res, err := p.s.Chat(ctx, m.GetPersonaId(), server.ChatRequest{Messages: msgs, Context: m.GetContext(), MaxTokens: int(m.GetMaxTokens()),
		MemoryScope: m.GetMemoryScope(), Remember: m.GetRemember()}, nil)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.ChatResponse{Answer: answerProto(res)}), nil
}

func (p *personaService) ChatStream(ctx context.Context, req *connect.Request[v1.ChatStreamRequest], stream *connect.ServerStream[v1.ChatStreamResponse]) error {
	m := req.Msg
	msgs, err := messages(m.GetMessages())
	if err != nil {
		return err
	}
	res, err := p.s.Chat(ctx, m.GetPersonaId(), server.ChatRequest{Messages: msgs, Context: m.GetContext(), MaxTokens: int(m.GetMaxTokens()),
		MemoryScope: m.GetMemoryScope(), Remember: m.GetRemember()}, func(delta string) error {
		return stream.Send(&v1.ChatStreamResponse{Event: &v1.ChatStreamResponse_Delta{Delta: delta}})
	})
	if err != nil {
		return toConnect(err)
	}
	return stream.Send(&v1.ChatStreamResponse{Event: &v1.ChatStreamResponse_Answer{Answer: answerProto(res)}})
}

func (p *personaService) SubmitChatJob(ctx context.Context, req *connect.Request[v1.SubmitChatJobRequest]) (*connect.Response[v1.SubmitChatJobResponse], error) {
	m := req.Msg
	msgs, err := messages(m.GetMessages())
	if err != nil {
		return nil, err
	}
	j, err := p.s.SubmitJob(ctx, m.GetPersonaId(), server.ChatRequest{Messages: msgs, Context: m.GetContext(), MaxTokens: int(m.GetMaxTokens())})
	if err != nil {
		return nil, toConnect(err)
	}
	job, err := jobProto(j)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.SubmitChatJobResponse{Job: job}), nil
}

func (p *personaService) GetJob(ctx context.Context, req *connect.Request[v1.GetJobRequest]) (*connect.Response[v1.GetJobResponse], error) {
	j, err := p.s.Job(ctx, req.Msg.GetJobId())
	if err != nil {
		return nil, toConnect(err)
	}
	job, err := jobProto(j)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetJobResponse{Job: job}), nil
}

type memoryService struct{ s *server.Server }

var _ openpersonav1connect.MemoryServiceHandler = (*memoryService)(nil)

func (m *memoryService) AddMessages(ctx context.Context, req *connect.Request[v1.AddMessagesRequest]) (*connect.Response[v1.AddMessagesResponse], error) {
	in := req.Msg.GetMessages()
	msgs := make([]memory.Message, len(in))
	for i, x := range in {
		msgs[i] = memory.Message{Role: x.GetRole(), Name: x.GetName(), Content: x.GetContent()}
		if x.GetTime() != nil {
			msgs[i].Time = x.GetTime().AsTime()
		}
	}
	if err := m.s.AddMessages(ctx, req.Msg.GetScope(), msgs); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.AddMessagesResponse{Stored: int32(len(msgs))}), nil
}

func (m *memoryService) SearchMemory(ctx context.Context, req *connect.Request[v1.SearchMemoryRequest]) (*connect.Response[v1.SearchMemoryResponse], error) {
	r := req.Msg
	res, err := m.s.SearchMemory(ctx, r.GetScope(), server.MemorySearchRequest{Query: r.GetQuery(), Facts: int(r.GetFacts()), Episodes: int(r.GetEpisodes()),
		IncludeHistory: r.GetIncludeHistory(), Rerank: r.GetRerank(), MaxTokens: int(r.GetMaxTokens())})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&v1.SearchMemoryResponse{Facts: itemsProto(res.Facts), Episodes: itemsProto(res.Episodes), Context: res.Context}), nil
}

// Conversions between the API's Go types and its Protobuf messages.

var roles = map[v1.Role]string{v1.Role_ROLE_USER: "user", v1.Role_ROLE_ASSISTANT: "assistant"}

func messages(in []*v1.Message) ([]llm.Message, error) {
	out := make([]llm.Message, len(in))
	for i, m := range in {
		role, ok := roles[m.GetRole()]
		if !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("messages[%d]: role must be ROLE_USER or ROLE_ASSISTANT", i))
		}
		out[i] = llm.Message{Role: role, Content: m.GetContent()}
	}
	return out, nil
}

func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func specProto(s persona.Spec) *v1.Persona {
	out := &v1.Persona{Id: s.ID, Version: s.Version, Name: s.Name, Role: s.Role, Description: s.Description,
		Expertise: s.Expertise, Tone: s.Tone, Language: s.Language, Boundaries: s.Boundaries, Author: s.Author}
	if r := s.Retrieve; r != nil {
		out.Retrieve = &v1.RetrieveDefaults{Units: int32(r.Units), Sources: int32(r.Sources)}
	}
	return out
}

func itemsProto(in []server.Item) []*v1.Item {
	out := make([]*v1.Item, len(in))
	for i, it := range in {
		out[i] = &v1.Item{Id: it.ID, Kind: it.Kind, Title: it.Title, Text: it.Text,
			ValidAt: timestamp(it.ValidAt), InvalidAt: timestamp(it.InvalidAt), Score: it.Score, Channels: it.Channels}
	}
	return out
}

func answerProto(r *server.ChatResult) *v1.Answer {
	return &v1.Answer{PersonaId: r.Persona, Version: r.Version, Reply: r.Reply, Units: itemsProto(r.Units),
		Usage: &v1.Usage{PromptTokens: r.Usage.PromptTokens, CompletionTokens: r.Usage.CompletionTokens, Calls: r.Usage.Calls}}
}

var jobStatuses = map[string]v1.JobStatus{
	server.JobQueued:    v1.JobStatus_JOB_STATUS_QUEUED,
	server.JobRunning:   v1.JobStatus_JOB_STATUS_RUNNING,
	server.JobSucceeded: v1.JobStatus_JOB_STATUS_SUCCEEDED,
	server.JobFailed:    v1.JobStatus_JOB_STATUS_FAILED,
}

func jobProto(j *server.Job) (*v1.Job, error) {
	out := &v1.Job{Id: j.ID, PersonaId: j.PersonaID, Status: jobStatuses[j.Status], Error: j.Error, Attempts: int32(j.Attempts),
		CreatedAt: timestamp(j.CreatedAt)}
	if j.StartedAt != nil {
		out.StartedAt = timestamp(*j.StartedAt)
	}
	if j.FinishedAt != nil {
		out.FinishedAt = timestamp(*j.FinishedAt)
	}
	if j.Status == server.JobSucceeded && len(j.Result) > 0 {
		var r server.ChatResult
		if err := json.Unmarshal(j.Result, &r); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("job %s: bad result: %w", j.ID, err))
		}
		out.Answer = answerProto(&r)
	}
	return out, nil
}
