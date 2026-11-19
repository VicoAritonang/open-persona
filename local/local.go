// Package local runs a persona from its pack file inside your process. It
// implements the same openpersona.Persona interface as a hosted persona,
// so an agent can move between the two without changes:
//
//	kora, err := local.Open("kora.persona", local.Options{Chat: model})
//	// or download it first from a server (needs the download scope):
//	kora, err := local.Fetch(ctx, client, "kora", "/var/lib/myapp/packs", local.Options{Chat: model})
//
// Knowledge retrieval needs no model; Chat and Stream need Options.Chat.
package local

import (
	"context"
	"errors"
	"fmt"
	"os"

	openpersona "go.vicoworks.com/open-persona"
	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/persona"
	"go.vicoworks.com/open-persona/recall"
)

// ErrNoChatModel is returned by Chat and Stream when Options.Chat is not set.
var ErrNoChatModel = errors.New("openpersona: no chat model: set local.Options.Chat (for example llm.New(\"openai:gpt-4o-mini\"))")

// Options configures a local persona.
type Options struct {
	// Chat is the model that answers as the persona (Chat, Stream). Any
	// llm.Client works; it streams if it implements llm.Streamer, as the
	// built-in OpenAI-compatible client does. Not needed for Knowledge.
	Chat llm.Client
	// Embedder embeds queries for semantic retrieval. It must be the model
	// the pack was trained with (Pack().EmbedModel). When nil and the pack
	// has vectors, Open connects that model itself using the provider key
	// from the environment (OPENAI_API_KEY for openai:*); if no key is set,
	// retrieval is lexical and graph only.
	Embedder llm.Embedder
	// NoEmbeddings turns semantic retrieval off: no network call at all.
	NoEmbeddings bool
	// Logf receives notes such as a missing embedding key (default: stderr).
	Logf func(format string, args ...any)
}

// Persona is a persona running from a pack file in this process. It is
// safe for concurrent use; open it once and share it.
type Persona struct {
	p    *persona.Persona
	chat llm.Client
}

var _ openpersona.Persona = (*Persona)(nil)

// Open opens a pack file (.persona).
func Open(path string, opt Options) (*Persona, error) {
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
			logf := opt.Logf
			if logf == nil {
				logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
			}
			logf("openpersona: %s: %v; running without semantic retrieval", p.Spec.ID, err)
		}
	}
	return &Persona{p: p, chat: opt.Chat}, nil
}

// Fetch downloads the current version of a persona from a server into dir
// (unless it is already there) and opens it.
func Fetch(ctx context.Context, c *openpersona.Client, id, dir string, opt Options) (*Persona, error) {
	path, err := c.Download(ctx, id, dir)
	if err != nil {
		return nil, err
	}
	return Open(path, opt)
}

// Pack is the underlying engine, for what this package does not wrap
// (evaluation, the units list, the manifest).
func (l *Persona) Pack() *persona.Persona { return l.p }

// ID implements openpersona.Persona.
func (l *Persona) ID() string { return l.p.Spec.ID }

// Info implements openpersona.Persona.
func (l *Persona) Info(context.Context) (*openpersona.Info, error) {
	s := l.p.Spec
	return &openpersona.Info{ID: s.ID, Version: s.Version, Name: s.Name, Role: s.Role, Description: s.Description,
		Expertise: s.Expertise, Tone: s.Tone, Language: s.Language, Boundaries: s.Boundaries, Author: s.Author}, nil
}

// Knowledge implements openpersona.Persona.
func (l *Persona) Knowledge(ctx context.Context, query string, opt openpersona.KnowledgeOptions) (*openpersona.Knowledge, error) {
	k, err := l.p.Retrieve(ctx, query, persona.RetrieveOptions{Units: opt.Units, Sources: opt.Sources})
	if err != nil {
		return nil, err
	}
	return &openpersona.Knowledge{
		Persona: l.p.Spec.ID, Version: l.p.Spec.Version,
		System: l.p.SystemPrompt(k), Context: k.Text(),
		Units: units(k.Units), Sources: units(k.Sources),
	}, nil
}

// Chat implements openpersona.Persona.
func (l *Persona) Chat(ctx context.Context, messages []openpersona.Message, opt openpersona.ChatOptions) (*openpersona.Reply, error) {
	if l.chat == nil {
		return nil, ErrNoChatModel
	}
	r, err := l.p.Chat(ctx, l.chat, messages, persona.ChatOptions{Context: opt.Context, MaxTokens: opt.MaxTokens})
	if err != nil {
		return nil, err
	}
	return l.reply(r), nil
}

// Stream implements openpersona.Persona.
func (l *Persona) Stream(ctx context.Context, messages []openpersona.Message, opt openpersona.ChatOptions, onDelta func(string) error) (*openpersona.Reply, error) {
	if l.chat == nil {
		return nil, ErrNoChatModel
	}
	r, err := l.p.ChatStream(ctx, l.chat, messages, persona.ChatOptions{Context: opt.Context, MaxTokens: opt.MaxTokens}, onDelta)
	if err != nil {
		return nil, err
	}
	return l.reply(r), nil
}

// Close implements openpersona.Persona.
func (l *Persona) Close() error { return l.p.Close() }

func (l *Persona) reply(r *persona.Reply) *openpersona.Reply {
	return &openpersona.Reply{Persona: l.p.Spec.ID, Version: l.p.Spec.Version, Content: r.Content, Units: units(r.Knowledge.Units), Usage: r.Usage}
}

func units(items []recall.Item) []openpersona.Unit {
	out := make([]openpersona.Unit, len(items))
	for i, it := range items {
		out[i] = openpersona.Unit{Kind: string(it.Node.Kind), Title: it.Node.Title, Text: it.Node.Body, Score: it.Score}
	}
	return out
}
