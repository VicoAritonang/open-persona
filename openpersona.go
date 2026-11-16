// Package openpersona is the client library for OpenPersona personas, such
// as Kora, the travel agent. Import it with an explicit name, since the
// path's last element is not an identifier:
//
//	import openpersona "go.vicoworks.com/open-persona"
//
// One interface, Persona, works two ways:
//
//   - Hosted: the persona runs on the OpenPersona service and is called over
//     HTTPS (NewClient, then Client.Persona). Nothing to download; answers
//     come back synchronously, as an async job, or streamed.
//   - Local: the persona's pack is downloaded once (Client.Download, or a
//     file you were given) and runs inside your own backend (Open). Knowledge
//     retrieval is then a local lookup with no network call except the
//     query embedding; answers use your own model keys.
//
// Either way, KnowledgeTool turns a persona into a tool your agent's model
// can call, once per sub-question:
//
//	client := openpersona.NewClient(os.Getenv("OPENPERSONA_API_KEY"), openpersona.ClientOptions{})
//	kora := client.Persona("kora")                   // hosted
//	// or: kora, _ := openpersona.Open("kora.persona", openpersona.LocalOptions{})   // local
//
//	k, _ := kora.Knowledge(ctx, "kereta Nagoya ke Kariya", openpersona.KnowledgeOptions{Units: 6})
//	reply, _ := kora.Chat(ctx, []openpersona.Message{{Role: "user", Content: "..."}}, openpersona.ChatOptions{})
//	tool, _ := openpersona.NewKnowledgeTool(ctx, kora, openpersona.ToolOptions{})
//
// The engines themselves (training, memory, the server) live in the
// subpackages persona, memory, server and registry.
package openpersona

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/persona"
	"go.vicoworks.com/open-persona/recall"
)

// Message is one chat message ("user" or "assistant"; the persona supplies
// the system message).
type Message = llm.Message

// Usage counts model tokens.
type Usage = llm.Usage

// Info describes a persona: id, version, name, role, expertise, language.
type Info = persona.Spec

// Unit is one piece of a persona's knowledge.
type Unit struct {
	Kind  string  `json:"kind"` // rule, procedure, reference, concept, principle, example, source
	Title string  `json:"title"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}

// Knowledge is what a persona knows about one query.
type Knowledge struct {
	Persona string `json:"persona"`
	Version string `json:"version"`
	// System is the persona's full system prompt for this query: identity,
	// boundaries, this knowledge, and grounding rules. Use it when your own
	// model should answer as the persona.
	System string `json:"system"`
	// Context is the knowledge alone, as text: the right shape for a tool
	// result returned to an agent.
	Context string `json:"context"`
	Units   []Unit `json:"units"`
	Sources []Unit `json:"sources"`
}

// KnowledgeOptions tunes retrieval. Zero values use the pack's defaults.
type KnowledgeOptions struct {
	Units   int // knowledge units (hosted: at most 64)
	Sources int // verbatim source excerpts; -1 for none
}

// ChatOptions tunes an answer.
type ChatOptions struct {
	// Context is extra material for the system prompt: what your app knows
	// about the user or the task (dates, passport, budget, live flight
	// results).
	Context   string
	MaxTokens int
}

// Reply is a persona's answer.
type Reply struct {
	Persona string `json:"persona"`
	Version string `json:"version"`
	Content string `json:"reply"`
	Units   []Unit `json:"units"` // the knowledge the answer was grounded in
	Usage   Usage  `json:"usage"`
}

// Persona is a persona, hosted or local.
type Persona interface {
	// ID is the persona id, such as "kora".
	ID() string
	// Info describes the persona.
	Info(ctx context.Context) (*Info, error)
	// Knowledge retrieves what the persona knows about query. It makes no
	// chat-model call.
	Knowledge(ctx context.Context, query string, opt KnowledgeOptions) (*Knowledge, error)
	// Chat answers the conversation's last user message as the persona.
	Chat(ctx context.Context, messages []Message, opt ChatOptions) (*Reply, error)
	// Stream is Chat with the answer delivered as it is generated.
	Stream(ctx context.Context, messages []Message, opt ChatOptions, onDelta func(text string) error) (*Reply, error)
	// Close releases the persona (a local pack file; nothing for hosted).
	Close() error
}

// Ask is Chat with a single question.
func Ask(ctx context.Context, p Persona, question string) (*Reply, error) {
	return p.Chat(ctx, []Message{{Role: "user", Content: question}}, ChatOptions{})
}

// ErrNoChatModel is returned by a local persona's Chat and Stream when
// LocalOptions.Chat is not set.
var ErrNoChatModel = errors.New("openpersona: no chat model: set LocalOptions.Chat (for example llm.New(\"openai:gpt-4o-mini\"))")

// LocalOptions configures a local persona.
type LocalOptions struct {
	// Chat is the model that answers as the persona (Chat, Stream). Any
	// llm.Client works; it streams if it implements llm.Streamer, as the
	// built-in OpenAI-compatible client does. Not needed for Knowledge.
	Chat llm.Client
	// Embedder embeds queries for semantic retrieval. It must be the model
	// the pack was trained with (see Info's pack manifest). When nil and the
	// pack has vectors, Open connects that model itself using the provider
	// key from the environment (OPENAI_API_KEY for openai:*); if no key is
	// set, retrieval is lexical and graph only.
	Embedder llm.Embedder
	// NoEmbeddings turns semantic retrieval off: no network call at all.
	NoEmbeddings bool
}

// Local is a persona running from a pack file in this process. It is safe
// for concurrent use; open it once and share it.
type Local struct {
	p    *persona.Persona
	chat llm.Client
}

var _ Persona = (*Local)(nil)

// Open opens a pack file (.persona) to run locally.
func Open(path string, opt LocalOptions) (*Local, error) {
	p, err := persona.Open(path)
	if err != nil {
		return nil, err
	}
	if p.EmbedModel != "" && !opt.NoEmbeddings {
		if opt.Embedder != nil {
			p.Embedder = opt.Embedder
		} else if model, err := llm.New(p.EmbedModel); err == nil {
			p.Embedder = model
		} else {
			fmt.Fprintf(os.Stderr, "openpersona: %s: %v; running without semantic retrieval\n", p.Spec.ID, err)
		}
	}
	return &Local{p: p, chat: opt.Chat}, nil
}

// Pack is the underlying engine, for what this package does not wrap
// (evaluation, the units list, the manifest).
func (l *Local) Pack() *persona.Persona { return l.p }

// ID implements Persona.
func (l *Local) ID() string { return l.p.Spec.ID }

// Info implements Persona.
func (l *Local) Info(context.Context) (*Info, error) {
	s := l.p.Spec
	return &s, nil
}

// Knowledge implements Persona.
func (l *Local) Knowledge(ctx context.Context, query string, opt KnowledgeOptions) (*Knowledge, error) {
	k, err := l.p.Retrieve(ctx, query, persona.RetrieveOptions{Units: opt.Units, Sources: opt.Sources})
	if err != nil {
		return nil, err
	}
	return &Knowledge{
		Persona: l.p.Spec.ID, Version: l.p.Spec.Version,
		System: l.p.SystemPrompt(k), Context: k.Text(),
		Units: units(k.Units), Sources: units(k.Sources),
	}, nil
}

// Chat implements Persona.
func (l *Local) Chat(ctx context.Context, messages []Message, opt ChatOptions) (*Reply, error) {
	if l.chat == nil {
		return nil, ErrNoChatModel
	}
	r, err := l.p.Chat(ctx, l.chat, messages, persona.ChatOptions{Context: opt.Context, MaxTokens: opt.MaxTokens})
	if err != nil {
		return nil, err
	}
	return l.reply(r), nil
}

// Stream implements Persona.
func (l *Local) Stream(ctx context.Context, messages []Message, opt ChatOptions, onDelta func(string) error) (*Reply, error) {
	if l.chat == nil {
		return nil, ErrNoChatModel
	}
	r, err := l.p.ChatStream(ctx, l.chat, messages, persona.ChatOptions{Context: opt.Context, MaxTokens: opt.MaxTokens}, onDelta)
	if err != nil {
		return nil, err
	}
	return l.reply(r), nil
}

// Close implements Persona.
func (l *Local) Close() error { return l.p.Close() }

func (l *Local) reply(r *persona.Reply) *Reply {
	return &Reply{Persona: l.p.Spec.ID, Version: l.p.Spec.Version, Content: r.Content, Units: units(r.Knowledge.Units), Usage: r.Usage}
}

func units(items []recall.Item) []Unit {
	out := make([]Unit, len(items))
	for i, it := range items {
		out[i] = Unit{Kind: string(it.Node.Kind), Title: it.Node.Title, Text: it.Node.Body, Score: it.Score}
	}
	return out
}
