// Package persona builds and serves trained personas.
//
// A persona is knowledge compiled once, in a controlled environment, into a
// frozen knowledge graph (a "pack": one read-only file). At runtime nothing
// is ever written: each request retrieves the relevant slice of the graph and
// renders it, under an always-on identity core, as the system prompt. It is a
// <role> prompt with depth: the role stays small, the discipline behind it
// can be as large as the source material.
package persona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.vicoworks.com/openpersona/graph"
	"go.vicoworks.com/openpersona/internal/textutil"
	"go.vicoworks.com/openpersona/llm"
	"go.vicoworks.com/openpersona/recall"
)

// Node kinds in a persona pack.
const (
	KindCore      graph.Kind = "core"      // identity, always in the prompt
	KindPrinciple graph.Kind = "principle" // guiding stance
	KindRule      graph.Kind = "rule"      // hard constraint: WHEN ... THEN ...
	KindProcedure graph.Kind = "procedure" // ordered steps
	KindConcept   graph.Kind = "concept"   // definition, formula, framework
	KindReference graph.Kind = "reference" // specific reference information
	KindExample   graph.Kind = "example"   // worked case
	KindSource    graph.Kind = "source"    // verbatim chunk of training material
)

// UnitKinds are the kinds of compiled knowledge units.
var UnitKinds = []graph.Kind{KindPrinciple, KindRule, KindProcedure, KindConcept, KindReference, KindExample}

// scope is the single namespace inside a pack.
const scope = "persona"

// Spec is the brief a persona is trained from. It is stored in the pack.
type Spec struct {
	// ID is the persona's stable identifier in a registry: lowercase
	// letters, digits and hyphens (default: derived from Name).
	ID string `json:"id,omitempty"`
	// Version identifies this build of the persona, as semantic versioning
	// ("1.2.0"; default "0.1.0"). A registry treats id@version as immutable,
	// so retraining on changed material means a new version.
	Version     string   `json:"version,omitempty"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`                  // e.g. "Senior finance controller"
	Description string   `json:"description,omitempty"` // whom it serves, what it is responsible for
	Expertise   []string `json:"expertise,omitempty"`   // the areas it covers; questions outside them are out of scope
	Tone        string   `json:"tone,omitempty"`        // e.g. "precise, calm, direct"
	Language    string   `json:"language,omitempty"`    // answer language; empty = follow the user
	Boundaries  []string `json:"boundaries,omitempty"`  // things it must never do
	Author      string   `json:"author,omitempty"`      // who trained it (team, lab)
	// Retrieve sets this persona's default retrieval budget. A broad
	// persona (a travel planner whose one request touches visas, transport,
	// food and events) needs more units per prompt than a narrow one.
	// Optional; RetrieveOptions passed at runtime still win.
	Retrieve *RetrieveDefaults `json:"retrieve,omitempty"`
}

// RetrieveDefaults is a pack's default retrieval budget.
type RetrieveDefaults struct {
	Units   int `json:"units,omitempty"`   // knowledge units per prompt (runtime default 8)
	Sources int `json:"sources,omitempty"` // verbatim excerpts per prompt (runtime default 2; -1 for none)
}

// Source is one training document (markdown or plain text).
type Source struct {
	Name string
	Text string
}

// Trainer compiles sources into a pack.
type Trainer struct {
	LLM llm.Client
	// Concurrency is the number of chunks extracted in parallel (default 6).
	Concurrency int
	// ChunkTokens is the target chunk size of the source material (default 700).
	ChunkTokens int
	// Embedder adds vectors to the pack so retrieval can match by meaning,
	// not only by words. Optional. EmbedModel names it ("provider:model")
	// and is recorded in the pack: queries must be embedded by the same model.
	Embedder   llm.Embedder
	EmbedModel string
	// TrainModel names the extraction model ("provider:model"). Optional;
	// it is recorded in the pack for provenance.
	TrainModel string
	// Eval is a held-out question set scored against the pack before it is
	// frozen; the result is stored in the pack. Optional.
	Eval []EvalCase
	// Logf receives progress messages. Optional.
	Logf func(format string, args ...any)
}

// Report summarizes a training run.
type Report struct {
	Sources  int            `json:"sources"`
	Chunks   int            `json:"chunks"`
	Units    int            `json:"units"`
	Entities int            `json:"entities"`
	ByKind   map[string]int `json:"by_kind"`
	Eval     *EvalSummary   `json:"eval,omitempty"`
}

type unit struct {
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags"`

	chunks []int // indexes of the chunks it came from
}

const extractPrompt = `You are compiling the working knowledge of a professional persona into a knowledge graph.

From the SOURCE TEXT, extract every piece of knowledge this persona needs in order to do its job with discipline. Give each one a kind:
- principle: a value or guiding stance that shapes judgment.
- rule: a hard constraint, threshold or obligation. Write it as "WHEN <condition> THEN <required action>" and keep exact numbers, limits and deadlines.
- procedure: an ordered way of doing a task. Write numbered steps.
- concept: the definition of a term, formula or framework.
- reference: specific reference information (figures, names, codes, contacts, dates).
- example: a worked case that shows the knowledge applied.

Requirements:
- Self-contained: every unit must be understandable without the source text.
- Faithful: never invent, generalize beyond, or soften the source. Keep numbers, names and conditions exact.
- Complete: keep every detail that could change a decision.
- "title": a short, specific name (3-8 words). "tags": 1-5 key terms the unit is about.
- Write in the language of the source text.

Return JSON: {"units":[{"kind":"...","title":"...","body":"...","tags":["..."]}]}
Return {"units":[]} if the text contains no usable knowledge.`

const corePrompt = `You are writing the core identity of an AI persona. It sits at the top of every system prompt, so it must be compact (under 300 words in total) and hold only what should shape EVERY answer. Detailed rules are retrieved separately, per question - do not list them here.

From the persona brief and its extracted principles and rules, return JSON:
{"identity": "2-4 sentences in the second person (\"You are ...\"): who the persona is, whom it serves, what it is responsible for",
 "principles": ["the 5-8 most important operating principles, one imperative sentence each"],
 "style": "1-3 sentences on tone and on how answers should be structured"}

Stay faithful to the material; never invent policy.`

// Train compiles sources into a frozen pack at outPath, replacing any
// existing file there. Chunks are processed in parallel; the pack is only
// written if every step succeeds.
func (t *Trainer) Train(ctx context.Context, spec Spec, sources []Source, outPath string) (*Report, error) {
	if t.LLM == nil {
		return nil, errors.New("persona: Trainer.LLM is required")
	}
	if err := spec.normalize(); err != nil {
		return nil, err
	}
	logf := t.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	chunkTokens := t.ChunkTokens
	if chunkTokens <= 0 {
		chunkTokens = 700
	}
	workers := t.Concurrency
	if workers <= 0 {
		workers = 6
	}

	// 1. Chunk. Each chunk keeps its heading path as its title, so it
	// carries its document context into both the prompt and the index.
	type chunk struct{ title, text string }
	var chunks []chunk
	for _, src := range sources {
		for _, c := range textutil.ChunkMarkdown(src.Text, chunkTokens) {
			title := src.Name
			if c.Heading != "" {
				title += " > " + c.Heading
			}
			chunks = append(chunks, chunk{title, c.Text})
		}
	}
	if len(chunks) == 0 {
		return nil, errors.New("persona: no source text to train on")
	}
	logf("training %s@%s: %d sources, %d chunks, %d workers", spec.ID, spec.Version, len(sources), len(chunks), workers)

	// 2. Map: one extraction call per chunk, in parallel.
	brief := fmt.Sprintf("Persona: %s - %s. %s", spec.Name, spec.Role, spec.Description)
	extracted := make([][]unit, len(chunks))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
		sem      = make(chan struct{}, workers)
	)
	for i, c := range chunks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			var out struct {
				Units []unit `json:"units"`
			}
			err := llm.JSON(ctx, t.LLM, llm.Request{
				Messages: []llm.Message{
					{Role: "system", Content: extractPrompt},
					{Role: "user", Content: brief + "\n\nSOURCE: " + c.title + "\n\nSOURCE TEXT:\n" + c.text},
				},
				Temperature: llm.Temp(0),
			}, &out)
			if err != nil {
				errOnce.Do(func() {
					firstErr = fmt.Errorf("persona: extract %q: %w", c.title, err)
					cancel()
				})
				return
			}
			extracted[i] = out.Units
			logf("  chunk %d/%d %q: %d units", i+1, len(chunks), c.title, len(out.Units))
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	// 3. Reduce: merge units that share a title, keeping the fuller body.
	var units []*unit
	byKey := make(map[string]*unit)
	for ci, us := range extracted {
		for _, u := range us {
			u.Title, u.Body = strings.TrimSpace(u.Title), strings.TrimSpace(u.Body)
			if u.Title == "" || u.Body == "" {
				continue
			}
			u.Kind = string(normalizeKind(u.Kind))
			key := textutil.Normalize(u.Title)
			if prev, ok := byKey[key]; ok && !sameKnowledge(prev.Body, u.Body) {
				// Same title, different knowledge: "Opening hours" of two
				// places, say. Keep both, each qualified by its section, so a
				// broad corpus does not lose one to the other.
				u.Title += " (" + sectionOf(chunks[ci].title) + ")"
				key = textutil.Normalize(u.Title)
			}
			if prev, ok := byKey[key]; ok {
				if len(u.Body) > len(prev.Body) {
					prev.Body, prev.Kind = u.Body, u.Kind
				}
				prev.Tags = append(prev.Tags, u.Tags...)
				prev.chunks = append(prev.chunks, ci)
				continue
			}
			u.chunks = []int{ci}
			byKey[key] = &u
			units = append(units, &u)
		}
	}
	if len(units) == 0 {
		return nil, errors.New("persona: no knowledge could be extracted from the sources")
	}

	// 4. Synthesize the identity core from the brief and the principles/rules.
	core, err := t.synthesizeCore(ctx, spec, units)
	if err != nil {
		return nil, err
	}

	// 5. Assemble the graph: sources, units, tag entities and their edges.
	report := &Report{Sources: len(sources), Chunks: len(chunks), Units: len(units), ByKind: make(map[string]int)}
	nodes := make([]*graph.Node, 0, len(chunks)+len(units)+1)
	var edges []graph.Edge
	for _, c := range chunks {
		nodes = append(nodes, &graph.Node{Scope: scope, Kind: KindSource, Title: c.title, Body: c.text})
	}
	entityIdx := make(map[string]int)
	for _, u := range units {
		ui := len(nodes)
		nodes = append(nodes, &graph.Node{Scope: scope, Kind: graph.Kind(u.Kind), Title: u.Title, Body: u.Body, Key: textutil.Normalize(u.Title)})
		report.ByKind[u.Kind]++
		for _, ci := range u.chunks {
			edges = append(edges, graph.Edge{Src: graph.Ref(ui), Dst: graph.Ref(ci), Rel: graph.RelSource})
		}
		for _, tag := range u.Tags {
			key := textutil.Normalize(tag)
			if key == "" {
				continue
			}
			ei, ok := entityIdx[key]
			if !ok {
				ei = len(nodes)
				entityIdx[key] = ei
				nodes = append(nodes, &graph.Node{Scope: scope, Kind: graph.KindEntity, Title: strings.TrimSpace(tag), Key: key})
			}
			edges = append(edges, graph.Edge{Src: graph.Ref(ui), Dst: graph.Ref(ei), Rel: graph.RelMentions})
		}
	}
	report.Entities = len(entityIdx)
	nodes = append(nodes, &graph.Node{Scope: scope, Kind: KindCore, Title: spec.Name, Body: core})

	// 6. Write to a temp file, freeze, and move into place.
	tmp := outPath + ".tmp"
	os.Remove(tmp)
	store, err := graph.Open(tmp, graph.Options{})
	if err != nil {
		return nil, err
	}
	specJSON, _ := json.Marshal(spec)
	reportJSON, _ := json.Marshal(report)
	err = errors.Join(
		store.SetFileFormat(ctx, PackApplicationID, PackFormat),
		store.Insert(ctx, nodes, edges),
		store.SetMeta(ctx, "spec", string(specJSON)),
		store.SetMeta(ctx, "report", string(reportJSON)),
		store.SetMeta(ctx, "trained_at", time.Now().UTC().Format(time.RFC3339)),
		store.SetMeta(ctx, "train_model", t.TrainModel),
	)
	if err == nil && t.Embedder != nil {
		var embed []*graph.Node
		for _, n := range nodes {
			if n.Kind != graph.KindEntity && n.Kind != KindCore {
				embed = append(embed, n)
			}
		}
		logf("embedding %d nodes", len(embed))
		err = errors.Join(
			recall.EmbedNodes(ctx, store, t.Embedder, embed, embedText),
			store.SetMeta(ctx, "embed_model", t.EmbedModel),
		)
	}
	// Score the held-out questions on the finished graph, before freezing,
	// so the result travels inside the pack.
	if err == nil && len(t.Eval) > 0 {
		p := &Persona{Spec: spec, Core: core, store: store}
		if t.Embedder != nil {
			p.Embedder, p.EmbedModel = t.Embedder, t.EmbedModel
		}
		var res *EvalResult
		if res, err = p.Evaluate(ctx, t.Eval, EvalOptions{}); err == nil {
			report.Eval = res.Summary()
			logf("eval: %d/%d questions fully covered, %.0f%% of expected facts in context", res.Passed, res.Cases, 100*res.Coverage)
			evalJSON, _ := json.Marshal(res)
			reportJSON, _ := json.Marshal(report)
			err = errors.Join(store.SetMeta(ctx, "eval", string(evalJSON)), store.SetMeta(ctx, "report", string(reportJSON)))
		}
	}
	if err != nil {
		store.Close()
		os.Remove(tmp)
		return nil, err
	}
	if err := store.Freeze(ctx); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Remove(tmp + suffix)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return nil, err
	}
	logf("wrote %s: %d units, %d entities", outPath, report.Units, report.Entities)
	return report, nil
}

// sameKnowledge reports whether two unit bodies filed under one title say
// the same thing: at least half the keywords of the shorter one occur in
// the other.
func sameKnowledge(a, b string) bool {
	ka, kb := textutil.Keywords(a), textutil.Keywords(b)
	if len(ka) > len(kb) {
		ka, kb = kb, ka
	}
	if len(ka) == 0 {
		return true
	}
	in := make(map[string]bool, len(kb))
	for _, w := range kb {
		in[w] = true
	}
	shared := 0
	for _, w := range ka {
		if in[w] {
			shared++
		}
	}
	return 2*shared >= len(ka)
}

// sectionOf is the most specific part of a chunk title ("doc > A > B" -> "B").
func sectionOf(chunkTitle string) string {
	if i := strings.LastIndex(chunkTitle, " > "); i >= 0 {
		return chunkTitle[i+3:]
	}
	return chunkTitle
}

// embedText is what represents a node in vector space: title and body.
func embedText(n *graph.Node) string {
	return textutil.Truncate(n.Title+"\n"+n.Body, 6000)
}

// normalizeKind maps a model-supplied kind to a known one; anything
// unrecognized is filed as reference information.
func normalizeKind(k string) graph.Kind {
	k = strings.ToLower(strings.TrimSpace(k))
	for _, known := range UnitKinds {
		if k == string(known) {
			return known
		}
	}
	return KindReference
}

func (t *Trainer) synthesizeCore(ctx context.Context, spec Spec, units []*unit) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "PERSONA BRIEF\nName: %s\nRole: %s\nDescription: %s\nTone: %s\n", spec.Name, spec.Role, spec.Description, spec.Tone)
	if len(spec.Expertise) > 0 {
		fmt.Fprintf(&b, "Expertise: %s\n", strings.Join(spec.Expertise, "; "))
	}
	if spec.Language != "" {
		fmt.Fprintf(&b, "Write the core in: %s\n", spec.Language)
	}
	b.WriteString("\nEXTRACTED PRINCIPLES AND RULES\n")
	budget := 3000 // tokens of material shown to the synthesizer
	for _, kind := range []graph.Kind{KindPrinciple, KindRule} {
		for _, u := range units {
			if u.Kind != string(kind) {
				continue
			}
			line := fmt.Sprintf("- [%s] %s: %s\n", u.Kind, u.Title, textutil.Truncate(u.Body, 300))
			if budget -= textutil.EstimateTokens(line); budget < 0 {
				break
			}
			b.WriteString(line)
		}
	}
	var out struct {
		Identity   string   `json:"identity"`
		Principles []string `json:"principles"`
		Style      string   `json:"style"`
	}
	err := llm.JSON(ctx, t.LLM, llm.Request{
		Messages:    []llm.Message{{Role: "system", Content: corePrompt}, {Role: "user", Content: b.String()}},
		Temperature: llm.Temp(0.2),
	}, &out)
	if err != nil {
		return "", fmt.Errorf("persona: synthesize core: %w", err)
	}
	if strings.TrimSpace(out.Identity) == "" {
		return "", errors.New("persona: synthesize core: model returned no identity")
	}

	var core strings.Builder
	core.WriteString(strings.TrimSpace(out.Identity))
	if len(spec.Expertise) > 0 {
		core.WriteString("\n\nAreas you cover: " + strings.Join(spec.Expertise, "; ") + ". Anything outside them is out of scope; say so.")
	}
	if len(out.Principles) > 0 {
		core.WriteString("\n\nOperating principles:\n")
		for _, p := range out.Principles {
			core.WriteString("- " + strings.TrimSpace(p) + "\n")
		}
	}
	if s := strings.TrimSpace(out.Style); s != "" {
		core.WriteString("\nStyle: " + s + "\n")
	}
	if len(spec.Boundaries) > 0 {
		core.WriteString("\nBoundaries (never cross these):\n")
		for _, x := range spec.Boundaries {
			core.WriteString("- " + x + "\n")
		}
	}
	return strings.TrimSpace(core.String()), nil
}
