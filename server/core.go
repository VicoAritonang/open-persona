package server

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/memory"
	"go.vicoworks.com/open-persona/persona"
	"go.vicoworks.com/open-persona/recall"
)

// The operations below are the API, independent of transport: the HTTP
// handlers in this package and the gRPC services in package rpc both call
// them, so the two cannot drift apart. Each expects the caller's principal
// in ctx (WithPrincipal) and returns an *Error on failure.

// Request limits.
const (
	MaxUnits   = 64 // knowledge units per request
	MaxSources = 16 // source excerpts per request
)

// Item is one retrieved piece of knowledge or memory.
type Item struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title,omitempty"`
	Text      string    `json:"text"`
	ValidAt   time.Time `json:"valid_at,omitzero"`
	InvalidAt time.Time `json:"invalid_at,omitzero"`
	Score     float64   `json:"score"`
	Channels  []string  `json:"channels,omitempty"`
}

func items(in []recall.Item) []Item {
	out := make([]Item, len(in))
	for i, it := range in {
		n := it.Node
		out[i] = Item{ID: n.ID, Kind: string(n.Kind), Title: n.Title, Text: n.Body,
			ValidAt: n.ValidAt.UTC(), InvalidAt: n.InvalidAt.UTC(), Score: it.Score, Channels: it.Channels}
	}
	return out
}

// ListPersonas returns the personas the caller may use, sorted by id.
func (s *Server) ListPersonas(ctx context.Context) ([]persona.Spec, error) {
	specs, err := s.catalog().List(ctx)
	if err != nil {
		return nil, wrapError(CodeInternal, err)
	}
	p := PrincipalFrom(ctx)
	allowed := make([]persona.Spec, 0, len(specs))
	for _, sp := range specs {
		if p.canAny(sp.ID) {
			allowed = append(allowed, sp)
		}
	}
	sort.Slice(allowed, func(i, j int) bool { return allowed[i].ID < allowed[j].ID })
	return allowed, nil
}

// Persona resolves id for a caller holding scope ("" requires any scope).
// A persona the caller cannot see is reported as not found.
func (s *Server) Persona(ctx context.Context, id, scope string) (*persona.Persona, error) {
	pr := PrincipalFrom(ctx)
	if !pr.canAny(id) {
		return nil, newError(CodeNotFound, "unknown persona: %s", id)
	}
	if scope != "" && !pr.Can(scope, id) {
		return nil, newError(CodePermissionDenied, "this API key lacks the %q scope for %s", scope, id)
	}
	p, err := s.catalog().Persona(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, newError(CodeNotFound, "unknown persona: %s", id)
	}
	if err != nil {
		s.logf("load persona %s: %v", id, err)
		return nil, &Error{Code: CodeUnavailable, Message: "persona " + id + " is unavailable, retry", Err: err}
	}
	return p, nil
}

// Manifest describes a persona: spec, identity core, training report and
// evaluation.
func (s *Server) Manifest(ctx context.Context, id string) (*persona.Manifest, error) {
	p, err := s.Persona(ctx, id, "")
	if err != nil {
		return nil, err
	}
	m, err := p.Manifest(ctx)
	if err != nil {
		return nil, wrapError(CodeInternal, err)
	}
	return m, nil
}

// KnowledgeRequest asks what a persona knows about one query.
type KnowledgeRequest struct {
	Query string `json:"query"`
	// Units and Sources override the pack's retrieval budget. An agent that
	// calls this as a knowledge tool, once per sub-question, can ask for a
	// few units per call. Sources -1 means none.
	Units   int `json:"units,omitempty"`
	Sources int `json:"sources,omitempty"`
}

// KnowledgeResult is what a persona knows about one query.
type KnowledgeResult struct {
	Persona string `json:"persona"`
	Version string `json:"version"`
	// System is the persona's full system prompt for this query.
	System string `json:"system"`
	// Context is the knowledge alone, as text.
	Context string `json:"context"`
	Units   []Item `json:"units"`
	Sources []Item `json:"sources"`
}

// Knowledge retrieves what persona id knows about a query. It makes no
// chat-model call. Scope: knowledge.
func (s *Server) Knowledge(ctx context.Context, id string, req KnowledgeRequest) (*KnowledgeResult, error) {
	if strings.TrimSpace(req.Query) == "" {
		return nil, newError(CodeInvalidArgument, "query is empty")
	}
	if req.Units < 0 || req.Units > MaxUnits || req.Sources < -1 || req.Sources > MaxSources {
		return nil, newError(CodeInvalidArgument, "units must be 0-%d and sources -1-%d", MaxUnits, MaxSources)
	}
	p, err := s.Persona(ctx, id, ScopeKnowledge)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	k, err := p.Retrieve(ctx, req.Query, persona.RetrieveOptions{Units: req.Units, Sources: req.Sources})
	if err != nil {
		return nil, wrapError(CodeInternal, err)
	}
	s.record(ctx, p.Spec.ID, "knowledge", llm.Usage{}, start)
	return &KnowledgeResult{
		Persona: p.Spec.ID, Version: p.Spec.Version,
		System: p.SystemPrompt(k), Context: k.Text(),
		Units: items(k.Units), Sources: items(k.Sources),
	}, nil
}

// ChatRequest asks a persona to answer a conversation.
type ChatRequest struct {
	// Messages is the conversation so far, user and assistant turns; the
	// last one must be from the user. The persona supplies the system prompt.
	Messages []llm.Message `json:"messages"`
	// Context is extra material for the system prompt, such as what the
	// caller's own app knows about the user.
	Context   string `json:"context,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	// Stream answers as server-sent events (HTTP chat only).
	Stream bool `json:"stream,omitempty"`
	// MemoryScope adds what this server's memory holds about the user and,
	// with Remember, stores the exchange afterwards (self-hosted servers).
	MemoryScope string `json:"memory_scope,omitempty"`
	Remember    bool   `json:"remember,omitempty"`
}

func (req *ChatRequest) validate() error {
	if len(req.Messages) == 0 {
		return newError(CodeInvalidArgument, "messages is empty")
	}
	if req.Messages[len(req.Messages)-1].Role != "user" {
		return newError(CodeInvalidArgument, "the last message must be from the user")
	}
	if req.MaxTokens < 0 {
		return newError(CodeInvalidArgument, "max_tokens is negative")
	}
	return nil
}

// ChatResult is a finished answer: the body of a chat response, the final
// server-sent event, and an async job's result.
type ChatResult struct {
	Persona string    `json:"persona"`
	Version string    `json:"version"`
	Reply   string    `json:"reply"`
	Units   []Item    `json:"units"` // the knowledge the answer was grounded in
	Usage   llm.Usage `json:"usage"`
}

func result(p *persona.Persona, reply *persona.Reply) *ChatResult {
	return &ChatResult{Persona: p.Spec.ID, Version: p.Spec.Version, Reply: reply.Content, Units: items(reply.Knowledge.Units), Usage: reply.Usage}
}

// Chat answers as persona id. With onDelta it streams: onDelta receives
// each piece of the answer as it is generated, and the result holds the
// whole of it. Scope: chat.
func (s *Server) Chat(ctx context.Context, id string, req ChatRequest, onDelta func(string) error) (*ChatResult, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	p, err := s.Persona(ctx, id, ScopeChat)
	if err != nil {
		return nil, err
	}
	if s.ChatModel == nil {
		return nil, newError(CodeUnavailable, "no chat model is configured")
	}
	opt, err := s.chatOptions(ctx, &req)
	if err != nil {
		return nil, wrapError(CodeInternal, err)
	}
	start := time.Now()
	var reply *persona.Reply
	op := "chat"
	if onDelta != nil {
		op = "stream"
		reply, err = p.ChatStream(ctx, s.ChatModel, req.Messages, opt, onDelta)
	} else {
		reply, err = p.Chat(ctx, s.ChatModel, req.Messages, opt)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, wrapError(CodeUnavailable, ctx.Err())
		}
		return nil, wrapError(CodeUpstream, err)
	}
	s.record(ctx, p.Spec.ID, op, reply.Usage, start)
	if err := s.rememberExchange(ctx, &req, reply.Content); err != nil {
		return nil, wrapError(CodeInternal, err)
	}
	return result(p, reply), nil
}

func (s *Server) chatOptions(ctx context.Context, req *ChatRequest) (persona.ChatOptions, error) {
	opt := persona.ChatOptions{Context: req.Context, MaxTokens: req.MaxTokens}
	if req.MemoryScope != "" && s.Memory != nil {
		res, err := s.Memory.Search(ctx, req.MemoryScope, req.Messages[len(req.Messages)-1].Content, memory.SearchOptions{})
		if err != nil {
			return opt, err
		}
		if block := res.Context(1500); block != "" {
			opt.Context = strings.TrimSpace(opt.Context + "\n\n" + block)
		}
	}
	return opt, nil
}

func (s *Server) rememberExchange(ctx context.Context, req *ChatRequest, reply string) error {
	if !req.Remember || req.MemoryScope == "" || s.Memory == nil {
		return nil
	}
	last := req.Messages[len(req.Messages)-1]
	return s.Memory.Add(ctx, req.MemoryScope, []memory.Message{{Role: last.Role, Content: last.Content}, {Role: "assistant", Content: reply}})
}

func (s *Server) memory() (*memory.Engine, error) {
	if s.Memory == nil {
		return nil, newError(CodeUnavailable, "memory is not configured")
	}
	return s.Memory, nil
}

// AddMessages stores messages in a memory scope. Facts are extracted in the
// background.
func (s *Server) AddMessages(ctx context.Context, scope string, msgs []memory.Message) error {
	mem, err := s.memory()
	if err != nil {
		return err
	}
	if strings.TrimSpace(scope) == "" {
		return newError(CodeInvalidArgument, "scope is empty")
	}
	if len(msgs) == 0 {
		return newError(CodeInvalidArgument, "messages is empty")
	}
	if err := mem.Add(ctx, scope, msgs); err != nil {
		return wrapError(CodeInternal, err)
	}
	return nil
}

// MemorySearchRequest asks what a memory scope holds about a query.
type MemorySearchRequest struct {
	Query          string `json:"query"`
	Facts          int    `json:"facts,omitempty"`
	Episodes       int    `json:"episodes,omitempty"`
	IncludeHistory bool   `json:"include_history,omitempty"`
	Rerank         bool   `json:"rerank,omitempty"`
	// MaxTokens budgets Context (0: no limit).
	MaxTokens int `json:"max_tokens,omitempty"`
}

// MemorySearchResult is what a memory scope holds about a query.
type MemorySearchResult struct {
	Facts    []Item `json:"facts"`
	Episodes []Item `json:"episodes"`
	// Context is a prompt-ready block ("" when nothing was found).
	Context string `json:"context"`
}

// SearchMemory retrieves facts and raw messages from a memory scope.
func (s *Server) SearchMemory(ctx context.Context, scope string, req MemorySearchRequest) (*MemorySearchResult, error) {
	mem, err := s.memory()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(scope) == "" {
		return nil, newError(CodeInvalidArgument, "scope is empty")
	}
	if req.Facts < 0 || req.Episodes < 0 || req.MaxTokens < 0 {
		return nil, newError(CodeInvalidArgument, "facts, episodes and max_tokens must not be negative")
	}
	res, err := mem.Search(ctx, scope, req.Query, memory.SearchOptions{
		Facts: req.Facts, Episodes: req.Episodes, IncludeHistory: req.IncludeHistory, Rerank: req.Rerank,
	})
	if err != nil {
		return nil, wrapError(CodeInternal, err)
	}
	return &MemorySearchResult{Facts: items(res.Facts), Episodes: items(res.Episodes), Context: res.Context(req.MaxTokens)}, nil
}
