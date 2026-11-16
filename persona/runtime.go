package persona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.vicoworks.com/openpersona/graph"
	"go.vicoworks.com/openpersona/internal/textutil"
	"go.vicoworks.com/openpersona/llm"
	"go.vicoworks.com/openpersona/recall"
)

// Persona is a trained pack opened for serving. It never writes: the file
// is opened read-only, so runtime traffic cannot alter the persona.
type Persona struct {
	Spec Spec
	Core string // identity block, part of every prompt
	// EmbedModel is the "provider:model" the pack was embedded with, or ""
	// if it has no vectors. Set Embedder to a client for that model to turn
	// on semantic retrieval.
	EmbedModel string
	// Embedder embeds queries. Optional; without it retrieval is lexical + graph.
	Embedder llm.Embedder

	store  *graph.Store
	format int32
	path   string
}

// Path is the pack file this persona was opened from.
func (p *Persona) Path() string { return p.path }

// Open loads a pack produced by Trainer.Train.
func Open(path string) (*Persona, error) {
	store, err := graph.Open(path, graph.Options{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	p := &Persona{store: store, path: path}
	appID, format, err := store.FileFormat(ctx)
	switch {
	case err != nil:
		store.Close()
		return nil, fmt.Errorf("persona: %s is not a persona pack: %w", path, err)
	case appID != PackApplicationID && appID != 0:
		store.Close()
		return nil, fmt.Errorf("persona: %s is not a persona pack (application id %#x)", path, appID)
	case format > PackFormat:
		store.Close()
		return nil, fmt.Errorf("persona: %s has pack format %d; this build reads up to %d, upgrade openpersona", path, format, PackFormat)
	}
	p.format = format
	specJSON, err := store.GetMeta(ctx, "spec")
	if err != nil || specJSON == "" {
		store.Close()
		return nil, fmt.Errorf("persona: %s is not a persona pack", path)
	}
	if err := json.Unmarshal([]byte(specJSON), &p.Spec); err != nil {
		store.Close()
		return nil, fmt.Errorf("persona: %s: bad spec: %w", path, err)
	}
	if p.Spec.ID == "" { // packs from before IDs existed
		p.Spec.ID = Slug(p.Spec.Name)
	}
	cores, err := store.List(ctx, scope, KindCore, 1)
	if err != nil || len(cores) == 0 {
		store.Close()
		return nil, fmt.Errorf("persona: %s has no core", path)
	}
	p.Core = cores[0].Body
	p.EmbedModel, _ = store.GetMeta(ctx, "embed_model")
	return p, nil
}

// Close releases the pack.
func (p *Persona) Close() error { return p.store.Close() }

// Stats returns the number of nodes per kind in the pack.
func (p *Persona) Stats(ctx context.Context) (map[graph.Kind]int, error) {
	return p.store.Count(ctx, scope)
}

// RetrieveOptions tunes retrieval. The zero value is a good default.
type RetrieveOptions struct {
	Units    int        // knowledge units to retrieve (default: the pack's spec, else 8)
	Sources  int        // verbatim source excerpts to add (default: the pack's spec, else 2; -1 for none)
	Reranker llm.Client // optional LLM rerank (one extra call)
}

func (o *RetrieveOptions) defaults(d *RetrieveDefaults) {
	if d != nil {
		if o.Units <= 0 {
			o.Units = d.Units
		}
		if o.Sources == 0 {
			o.Sources = d.Sources
		}
	}
	if o.Units <= 0 {
		o.Units = 8
	}
	if o.Sources == 0 {
		o.Sources = 2
	} else if o.Sources < 0 {
		o.Sources = 0
	}
}

// Knowledge is the slice of the persona relevant to one query.
type Knowledge struct {
	Units   []recall.Item // compiled knowledge, most relevant first
	Sources []recall.Item // verbatim excerpts of the training material
}

// Empty reports whether nothing was retrieved.
func (k *Knowledge) Empty() bool { return k == nil || (len(k.Units) == 0 && len(k.Sources) == 0) }

// Text renders the retrieved units and excerpts as plain text, the form
// they take inside the system prompt. It suits a tool result returned to an
// agent's model.
func (k *Knowledge) Text() string {
	if k.Empty() {
		return ""
	}
	var b strings.Builder
	for _, it := range k.Units {
		fmt.Fprintf(&b, "\n[%s] %s\n%s\n", it.Node.Kind, it.Node.Title, it.Node.Body)
	}
	for _, it := range k.Sources {
		fmt.Fprintf(&b, "\n[source excerpt] %s\n%s\n", it.Node.Title, textutil.Truncate(it.Node.Body, 2400))
	}
	return b.String()
}

// Retrieve finds the knowledge relevant to query. It makes no LLM call
// unless a reranker is set.
func (p *Persona) Retrieve(ctx context.Context, query string, opt RetrieveOptions) (*Knowledge, error) {
	opt.defaults(p.Spec.Retrieve)
	channels := []recall.Channel{
		// Titles are short and specific, so a title match counts for more.
		{Name: "units", Weight: 1, Run: recall.Text(p.store, scope, UnitKinds, query, graph.SearchOptions{Limit: opt.Units * 3, TitleWeight: 3})},
		{Name: "graph", Weight: 0.6, Run: recall.ViaEntities(p.store, scope, query, 5, opt.Units, false)},
	}
	if opt.Sources > 0 {
		channels = append(channels, recall.Channel{Name: "sources", Weight: 1,
			Run: recall.Text(p.store, scope, []graph.Kind{KindSource}, query, graph.SearchOptions{Limit: opt.Sources * 2})})
	}
	if p.Embedder != nil && p.EmbedModel != "" {
		kinds := UnitKinds
		if opt.Sources > 0 {
			kinds = append(append([]graph.Kind{}, UnitKinds...), KindSource)
		}
		channels = append(channels, recall.Channel{Name: "vector", Weight: 1, Optional: true,
			Run: recall.Vector(p.store, p.Embedder, nil, scope, kinds, query, graph.SearchOptions{Limit: opt.Units * 3})})
	}
	// A request with several parts ("I live in Stockholm. I have a meeting
	// in Kariya on 12 March. Then three days in Kyoto.") gets a channel per
	// part, so the strongest part does not crowd the others out of the budget.
	for i, f := range facets(query) {
		lim := graph.SearchOptions{Limit: opt.Units, TitleWeight: 3}
		channels = append(channels, recall.Channel{Name: fmt.Sprintf("facet%d", i), Weight: 0.5,
			Run: recall.Text(p.store, scope, UnitKinds, f, lim)})
		if p.Embedder != nil && p.EmbedModel != "" {
			channels = append(channels, recall.Channel{Name: fmt.Sprintf("facet%d-vector", i), Weight: 0.5, Optional: true,
				Run: recall.Vector(p.store, p.Embedder, nil, scope, UnitKinds, f, graph.SearchOptions{Limit: opt.Units})})
		}
	}
	items, err := recall.Fuse(ctx, channels)
	if err != nil {
		return nil, err
	}
	k := &Knowledge{}
	for _, it := range items {
		if it.Node.Kind == KindSource {
			if len(k.Sources) < opt.Sources {
				k.Sources = append(k.Sources, it)
			}
		} else if len(k.Units) < opt.Units*2 {
			k.Units = append(k.Units, it)
		}
	}
	if opt.Reranker != nil {
		if reranked, err := recall.Rerank(ctx, opt.Reranker, query, k.Units); err == nil {
			k.Units = reranked
		}
	}
	if len(k.Units) > opt.Units {
		k.Units = k.Units[:opt.Units]
	}
	return k, nil
}

// maxFacets caps the extra channels (and, with embeddings, the extra
// embedding calls) one query can add.
const maxFacets = 4

// facets splits a multi-part query into its sentences. It returns nil for a
// single-part query, which the whole-query channels already cover. A
// sentence needs three keywords to count, and a dot only ends a sentence
// before a space, so "Rp 20.000.000" stays whole.
func facets(query string) []string {
	var parts []string
	start := 0
	for i, r := range query {
		end := r == '\n' || r == '?' || r == '!' || r == ';' ||
			(r == '.' && (i+1 == len(query) || query[i+1] == ' '))
		if !end {
			continue
		}
		parts = append(parts, query[start:i])
		start = i + 1
	}
	parts = append(parts, query[start:])
	var out []string
	for _, s := range parts {
		if s = strings.TrimSpace(s); len(textutil.Keywords(s)) >= 3 {
			out = append(out, s)
		}
	}
	if len(out) < 2 {
		return nil
	}
	if len(out) > maxFacets {
		out = out[:maxFacets]
	}
	return out
}

// SystemPrompt renders the persona for one query: the identity core, the
// retrieved knowledge, and grounding instructions.
func (p *Persona) SystemPrompt(k *Knowledge) string {
	var b strings.Builder
	b.WriteString("<role>\n" + p.Core + "\n</role>\n\n<knowledge>\n")
	if k.Empty() {
		b.WriteString("(Nothing in your knowledge base matches this request.)\n")
	} else {
		b.WriteString("This is the part of your own working knowledge that is relevant to the current request. Apply it exactly; rules are binding.\n")
		b.WriteString(k.Text())
	}
	b.WriteString("</knowledge>\n\n<instructions>\n")
	b.WriteString("Answer as this persona. Base every policy, number and procedure on the knowledge above. ")
	b.WriteString("Never present a requirement, step or figure as policy unless it appears in the knowledge above; ")
	b.WriteString("if you add general professional advice, label it clearly as general practice. ")
	b.WriteString("If the knowledge above does not cover the question, say so plainly instead of guessing.")
	if p.Spec.Language != "" {
		b.WriteString(" Answer in " + p.Spec.Language + " unless the user writes in another language.")
	}
	b.WriteString("\n</instructions>")
	return b.String()
}

// ChatOptions tunes Chat.
type ChatOptions struct {
	Retrieve RetrieveOptions
	// Context is extra material appended to the system prompt, such as a
	// memory block about the current user (see package memory).
	Context     string
	Temperature *float64
	MaxTokens   int
}

// Reply is a persona's answer with the context it was produced from.
type Reply struct {
	Content   string
	System    string // the system prompt that was used
	Knowledge *Knowledge
	Usage     llm.Usage
}

// Chat answers the last user message in messages as the persona.
// messages is the conversation so far (user/assistant turns, no system
// message); the persona supplies the system prompt.
func (p *Persona) Chat(ctx context.Context, c llm.Client, messages []llm.Message, opt ChatOptions) (*Reply, error) {
	return p.chat(ctx, c, messages, opt, nil)
}

// ChatStream is Chat with the answer delivered as it is generated:
// onDelta receives each piece of text. It streams when c implements
// llm.Streamer; otherwise the whole answer arrives as one piece. The Reply
// holds the complete answer.
func (p *Persona) ChatStream(ctx context.Context, c llm.Client, messages []llm.Message, opt ChatOptions, onDelta func(string) error) (*Reply, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	return p.chat(ctx, c, messages, opt, onDelta)
}

func (p *Persona) chat(ctx context.Context, c llm.Client, messages []llm.Message, opt ChatOptions, onDelta func(string) error) (*Reply, error) {
	// Retrieve on the last two user turns so follow-ups ("and for travel?")
	// still find the knowledge the conversation is about.
	var userTurns []string
	for i := len(messages) - 1; i >= 0 && len(userTurns) < 2; i-- {
		if messages[i].Role == "user" {
			userTurns = append([]string{messages[i].Content}, userTurns...)
		}
	}
	if len(userTurns) == 0 {
		return nil, errors.New("persona: no user message to answer")
	}
	k, err := p.Retrieve(ctx, strings.Join(userTurns, "\n"), opt.Retrieve)
	if err != nil {
		return nil, err
	}
	system := p.SystemPrompt(k)
	if opt.Context != "" {
		system += "\n\n" + opt.Context
	}
	req := llm.Request{
		Messages:    append([]llm.Message{{Role: "system", Content: system}}, messages...),
		Temperature: opt.Temperature,
		MaxTokens:   opt.MaxTokens,
	}
	var resp llm.Response
	if onDelta == nil {
		resp, err = c.Complete(ctx, req)
	} else {
		resp, err = llm.StreamOrComplete(ctx, c, req, onDelta)
	}
	if err != nil {
		return nil, err
	}
	return &Reply{Content: resp.Content, System: system, Knowledge: k, Usage: resp.Usage}, nil
}
