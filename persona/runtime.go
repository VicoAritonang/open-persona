package persona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openpersona/openpersona/graph"
	"github.com/openpersona/openpersona/internal/textutil"
	"github.com/openpersona/openpersona/llm"
	"github.com/openpersona/openpersona/recall"
)

// Persona is a trained pack opened for serving. It never writes: the file
// is opened read-only, so runtime traffic cannot alter the persona.
type Persona struct {
	Spec  Spec
	Core  string // identity block, part of every prompt
	store *graph.Store
}

// Open loads a pack produced by Trainer.Train.
func Open(path string) (*Persona, error) {
	store, err := graph.Open(path, graph.Options{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	p := &Persona{store: store}
	specJSON, err := store.GetMeta(ctx, "spec")
	if err != nil || specJSON == "" {
		store.Close()
		return nil, fmt.Errorf("persona: %s is not a persona pack", path)
	}
	if err := json.Unmarshal([]byte(specJSON), &p.Spec); err != nil {
		store.Close()
		return nil, fmt.Errorf("persona: %s: bad spec: %w", path, err)
	}
	cores, err := store.List(ctx, scope, KindCore, 1)
	if err != nil || len(cores) == 0 {
		store.Close()
		return nil, fmt.Errorf("persona: %s has no core", path)
	}
	p.Core = cores[0].Body
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
	Units    int        // knowledge units to retrieve (default 8)
	Sources  int        // verbatim source excerpts to add (default 2; -1 for none)
	Reranker llm.Client // optional LLM rerank (one extra call)
}

func (o *RetrieveOptions) defaults() {
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

// Retrieve finds the knowledge relevant to query. It makes no LLM call
// unless a reranker is set.
func (p *Persona) Retrieve(ctx context.Context, query string, opt RetrieveOptions) (*Knowledge, error) {
	opt.defaults()
	channels := []recall.Channel{
		// Titles are short and specific, so a title match counts for more.
		{Name: "units", Weight: 1, Run: recall.Text(p.store, scope, UnitKinds, query, graph.SearchOptions{Limit: opt.Units * 3, TitleWeight: 3})},
		{Name: "graph", Weight: 0.6, Run: recall.ViaEntities(p.store, scope, query, 5, opt.Units, false)},
	}
	if opt.Sources > 0 {
		channels = append(channels, recall.Channel{Name: "sources", Weight: 1,
			Run: recall.Text(p.store, scope, []graph.Kind{KindSource}, query, graph.SearchOptions{Limit: opt.Sources * 2})})
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

// SystemPrompt renders the persona for one query: the identity core, the
// retrieved knowledge, and grounding instructions.
func (p *Persona) SystemPrompt(k *Knowledge) string {
	var b strings.Builder
	b.WriteString("<role>\n" + p.Core + "\n</role>\n\n<knowledge>\n")
	if k == nil || (len(k.Units) == 0 && len(k.Sources) == 0) {
		b.WriteString("(Nothing in your knowledge base matches this request.)\n")
	} else {
		b.WriteString("This is the part of your own working knowledge that is relevant to the current request. Apply it exactly; rules are binding.\n")
		for _, it := range k.Units {
			fmt.Fprintf(&b, "\n[%s] %s\n%s\n", it.Node.Kind, it.Node.Title, it.Node.Body)
		}
		for _, it := range k.Sources {
			fmt.Fprintf(&b, "\n[source excerpt] %s\n%s\n", it.Node.Title, textutil.Truncate(it.Node.Body, 2400))
		}
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
	resp, err := c.Complete(ctx, llm.Request{
		Messages:    append([]llm.Message{{Role: "system", Content: system}}, messages...),
		Temperature: opt.Temperature,
		MaxTokens:   opt.MaxTokens,
	})
	if err != nil {
		return nil, err
	}
	return &Reply{Content: resp.Content, System: system, Knowledge: k, Usage: resp.Usage}, nil
}
