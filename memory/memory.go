// Package memory is the read-write context engine: it stores conversations
// and retrieves what matters for the next turn.
//
// Design, in one paragraph: every message is stored verbatim and is
// searchable immediately with BM25, at zero LLM cost. In the background, one
// LLM call per batch of messages distills atomic facts, links them to the
// entities they mention, and marks older facts they contradict as no longer
// valid (history is kept, never deleted); a second, small call is made only
// when a new fact resembles an existing one the first call was not shown.
// Retrieval runs several channels in
// parallel - facts, raw messages, the entity graph, and embeddings if you
// configure them - and fuses the rankings. No LLM is needed to retrieve.
package memory

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"go.vicoworks.com/open-persona/graph"
	"go.vicoworks.com/open-persona/internal/textutil"
	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/recall"
)

// Message is one conversation turn to remember.
type Message struct {
	Role    string    `json:"role"`           // user | assistant | any label
	Name    string    `json:"name,omitempty"` // speaker name, if known
	Content string    `json:"content"`
	Time    time.Time `json:"time,omitempty"` // when it was said (default: now)
}

// Config configures an Engine. Only Store is required.
type Config struct {
	Store *graph.Store

	// Extractor distills facts from messages. Nil runs the engine in
	// raw-only mode: messages are stored and searched, with no LLM cost.
	Extractor llm.Client
	// Embedder adds a vector channel to retrieval. Optional.
	Embedder llm.Embedder
	// Reranker is used by searches that ask for reranking. Optional.
	Reranker llm.Client

	// Workers is the number of extraction goroutines (default 4). Batches of
	// one scope always run in order on the same worker.
	Workers int
	// MaxBatchTokens caps the messages sent in one extraction call (default 2500).
	MaxBatchTokens int
	// ExtractionRules are extra instructions for the extractor, e.g. what
	// your product considers worth remembering.
	ExtractionRules string
	// SkipReconcile turns off the second pass that checks new facts against
	// existing ones the extractor was not shown. Ingestion is then exactly
	// one LLM call per batch, at the price of missing some contradictions.
	SkipReconcile bool
	// OnError receives background extraction errors. Flush also returns them.
	OnError func(error)
}

// Engine is a long-term memory. It is safe for concurrent use.
type Engine struct {
	cfg     Config
	shards  []chan job
	pending sync.WaitGroup
	workers sync.WaitGroup

	mu   sync.Mutex
	errs []error

	closeMu sync.RWMutex
	closed  bool
}

type job struct {
	scope    string
	episodes []*graph.Node
}

// New starts an engine and its background workers.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil {
		return nil, errors.New("memory: Config.Store is required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.MaxBatchTokens <= 0 {
		cfg.MaxBatchTokens = 2500
	}
	e := &Engine{cfg: cfg, shards: make([]chan job, cfg.Workers)}
	for i := range e.shards {
		ch := make(chan job, 256)
		e.shards[i] = ch
		e.workers.Add(1)
		go func() {
			defer e.workers.Done()
			for j := range ch {
				// Background work must outlive the request that queued it.
				if err := e.process(context.Background(), j); err != nil {
					e.fail(fmt.Errorf("memory: extract (scope %s): %w", j.scope, err))
				}
				e.pending.Done()
			}
		}()
	}
	return e, nil
}

func (e *Engine) fail(err error) {
	e.mu.Lock()
	e.errs = append(e.errs, err)
	e.mu.Unlock()
	if e.cfg.OnError != nil {
		e.cfg.OnError(err)
	}
}

// Add stores messages under scope. They are searchable when Add returns;
// fact extraction continues in the background (see Flush).
func (e *Engine) Add(ctx context.Context, scope string, msgs []Message) error {
	if scope == "" {
		return errors.New("memory: scope is required")
	}
	now := time.Now().UTC()
	var episodes []*graph.Node
	for _, m := range msgs {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		speaker := m.Name
		if speaker == "" {
			speaker = m.Role
		}
		at := m.Time
		if at.IsZero() {
			at = now
		}
		// Long messages are split so each piece ranks on its own content.
		parts := []textutil.Chunk{{Text: m.Content}}
		if textutil.EstimateTokens(m.Content) > 600 {
			parts = textutil.ChunkMarkdown(m.Content, 400)
		}
		for _, p := range parts {
			episodes = append(episodes, &graph.Node{Scope: scope, Kind: graph.KindEpisode, Title: speaker, Body: p.Text, ValidAt: at.UTC()})
		}
	}
	if len(episodes) == 0 {
		return nil
	}
	if err := e.cfg.Store.Insert(ctx, episodes, nil); err != nil {
		return err
	}
	if e.cfg.Extractor == nil && e.cfg.Embedder == nil {
		return nil
	}

	// Held while queueing so Close cannot close the channels under us.
	e.closeMu.RLock()
	defer e.closeMu.RUnlock()
	if e.closed {
		return errors.New("memory: engine is closed")
	}
	h := fnv.New32a()
	h.Write([]byte(scope))
	shard := e.shards[int(h.Sum32())%len(e.shards)]
	for _, batch := range e.batches(episodes) {
		e.pending.Add(1)
		select {
		case shard <- job{scope: scope, episodes: batch}:
		case <-ctx.Done():
			e.pending.Done()
			return ctx.Err()
		}
	}
	return nil
}

// batches groups episodes into windows of at most MaxBatchTokens.
func (e *Engine) batches(eps []*graph.Node) [][]*graph.Node {
	var out [][]*graph.Node
	var cur []*graph.Node
	size := 0
	for _, ep := range eps {
		t := textutil.EstimateTokens(ep.Body)
		if len(cur) > 0 && size+t > e.cfg.MaxBatchTokens {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, ep)
		size += t
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// Flush waits until all queued extraction has finished and returns any
// background errors since the last Flush.
func (e *Engine) Flush(ctx context.Context) error {
	done := make(chan struct{})
	go func() { e.pending.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	err := errors.Join(e.errs...)
	e.errs = nil
	return err
}

// Close finishes queued work and stops the workers. It does not close the Store.
func (e *Engine) Close() {
	e.closeMu.Lock()
	if e.closed {
		e.closeMu.Unlock()
		return
	}
	e.closed = true
	e.closeMu.Unlock()
	e.pending.Wait()
	for _, ch := range e.shards {
		close(ch)
	}
	e.workers.Wait()
}

const extractPrompt = `You maintain the long-term memory of an AI assistant. From the NEW MESSAGES, extract the facts worth remembering in future conversations.

Rules:
- Each fact is ONE atomic, self-contained sentence. Name people and things explicitly (no "he", "she", "it", "there") so the fact is clear with no other context.
- Call the human "User" unless their name is known; then use the name.
- Resolve relative time ("yesterday", "next month") into absolute dates using the message dates, and write the date into the sentence when it matters.
- "date" is the day the fact became true or the event happened (YYYY-MM-DD); use the message date if unsure.
- Keep specifics exactly: names, numbers, amounts, places, preferences, plans, decisions, relationships, events and outcomes.
- Capture what the user states AND substantive information from the assistant (recommendations, commitments, answers the user may refer back to).
- Skip greetings, filler, and anything EXISTING FACTS already cover.
- If a new fact contradicts or updates an existing fact (a changed job, city, preference, plan, status), put that existing fact's id in "supersedes". Never supersede a fact that is only related or is still true.
- "entities": 1-4 proper names or key topics the fact is about (people, organizations, places, products, projects), in canonical form. Do not list "User" or "Assistant".
- Write the facts in the language of the messages.

Return JSON: {"facts":[{"text":"...","date":"YYYY-MM-DD","entities":["..."],"supersedes":[]}]}
Return {"facts":[]} when nothing is worth remembering.`

type extraction struct {
	Facts []struct {
		Text       string   `json:"text"`
		Date       string   `json:"date"`
		Entities   []string `json:"entities"`
		Supersedes []int64  `json:"supersedes"`
	} `json:"facts"`
}

// existingFacts is how many related facts are shown to the extractor so it
// can skip duplicates and flag contradictions in the same single call.
const existingFacts = 10

func (e *Engine) process(ctx context.Context, j job) error {
	store := e.cfg.Store
	var newNodes []*graph.Node // nodes needing embeddings
	newNodes = append(newNodes, j.episodes...)

	if e.cfg.Extractor != nil {
		var transcript strings.Builder
		for _, ep := range j.episodes {
			fmt.Fprintf(&transcript, "[%s] %s: %s\n", ep.ValidAt.Format("2006-01-02 Mon"), ep.Title, ep.Body)
		}
		related, err := store.Search(ctx, j.scope, []graph.Kind{graph.KindFact}, transcript.String(), graph.SearchOptions{Limit: existingFacts})
		if err != nil {
			return err
		}
		known := make(map[int64]bool, len(related))
		var prompt strings.Builder
		prompt.WriteString("EXISTING FACTS:\n")
		if len(related) == 0 {
			prompt.WriteString("(none)\n")
		}
		for _, h := range related {
			known[h.Node.ID] = true
			fmt.Fprintf(&prompt, "[%d] (%s) %s\n", h.Node.ID, h.Node.ValidAt.Format("2006-01-02"), h.Node.Body)
		}
		prompt.WriteString("\nNEW MESSAGES:\n")
		prompt.WriteString(transcript.String())

		system := extractPrompt
		if e.cfg.ExtractionRules != "" {
			system += "\n\nAdditional rules:\n" + e.cfg.ExtractionRules
		}
		var out extraction
		if err := llm.JSON(ctx, e.cfg.Extractor, llm.Request{
			Messages:    []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: prompt.String()}},
			Temperature: llm.Temp(0),
		}, &out); err != nil {
			return err
		}

		last := j.episodes[len(j.episodes)-1].ValidAt
		var nodes []*graph.Node
		var edges []graph.Edge
		var factIdx []int // index in nodes of each accepted fact
		var supersedes [][]int64
		entityIdx := make(map[string]int)
		for _, f := range out.Facts {
			text := strings.TrimSpace(f.Text)
			if text == "" {
				continue
			}
			at := last
			if d, err := time.Parse("2006-01-02", strings.TrimSpace(f.Date)); err == nil {
				at = d
			}
			fi := len(nodes)
			nodes = append(nodes, &graph.Node{Scope: j.scope, Kind: graph.KindFact, Body: text, Key: textutil.Normalize(text), ValidAt: at})
			factIdx = append(factIdx, fi)
			var sup []int64
			for _, id := range f.Supersedes {
				if known[id] { // ignore ids the model made up
					sup = append(sup, id)
				}
			}
			supersedes = append(supersedes, sup)
			for _, ep := range j.episodes {
				edges = append(edges, graph.Edge{Src: graph.Ref(fi), Dst: ep.ID, Rel: graph.RelSource})
			}
			for _, name := range f.Entities {
				key := textutil.Normalize(name)
				if key == "" || key == "user" || key == "assistant" {
					continue
				}
				ei, ok := entityIdx[key]
				if !ok {
					ei = len(nodes)
					entityIdx[key] = ei
					nodes = append(nodes, &graph.Node{Scope: j.scope, Kind: graph.KindEntity, Title: strings.TrimSpace(name), Key: key})
				}
				edges = append(edges, graph.Edge{Src: graph.Ref(fi), Dst: graph.Ref(ei), Rel: graph.RelMentions})
			}
		}
		if err := store.Insert(ctx, nodes, edges); err != nil {
			return err
		}
		validAt := make(map[int64]time.Time, len(related))
		for _, h := range related {
			validAt[h.Node.ID] = h.Node.ValidAt
		}
		var fresh []*graph.Node // facts that are new, not restatements
		inBatch := make(map[int64]bool)
		for i, fi := range factIdx {
			fact := nodes[fi]
			inBatch[fact.ID] = true
			var ids []int64
			for _, id := range supersedes[i] {
				// A restated fact cannot replace itself, and older
				// information never overrides newer.
				if id != fact.ID && !validAt[id].After(fact.ValidAt) {
					ids = append(ids, id)
				}
			}
			if err := store.Invalidate(ctx, j.scope, ids, fact.ValidAt, fact.ID); err != nil {
				return err
			}
			if !fact.CreatedAt.IsZero() { // Insert leaves deduplicated nodes untouched
				fresh = append(fresh, fact)
			}
		}
		newNodes = append(newNodes, fresh...)
		if !e.cfg.SkipReconcile {
			if err := e.reconcile(ctx, j.scope, fresh, inBatch, known); err != nil {
				return err
			}
		}
	}

	if e.cfg.Embedder != nil {
		return recall.EmbedNodes(ctx, store, e.cfg.Embedder, newNodes, func(n *graph.Node) string { return n.Body })
	}
	return nil
}

const reconcilePrompt = `You keep a memory of facts consistent. For each NEW fact, decide whether it makes an EXISTING fact outdated: the existing fact used to be true and the new fact replaces it (a changed job, home, preference, plan, status or number).
An existing fact that is only related, more general, or still true alongside the new fact is NOT outdated.

Return JSON: {"outdated":[{"id": <existing fact id>, "by": <new fact number>}]}
Return {"outdated":[]} if nothing is outdated.`

// reconcile finds contradictions the extraction call could not see.
//
// The extractor is shown existing facts that match the raw messages, but a
// message such as "I joined Gojek" shares no words with the stored "Rina
// works at Tokopedia". The extracted fact "Rina works at Gojek" does, so the
// lookup is repeated with the new facts. Only if that turns up existing
// facts the extractor never saw is a second, small LLM call made.
func (e *Engine) reconcile(ctx context.Context, scope string, fresh []*graph.Node, inBatch, seen map[int64]bool) error {
	store := e.cfg.Store
	candidates := make(map[int64]*graph.Node)
	var order []int64
	for _, f := range fresh {
		hits, err := store.Search(ctx, scope, []graph.Kind{graph.KindFact}, f.Body, graph.SearchOptions{Limit: 5})
		if err != nil {
			return err
		}
		for _, h := range hits {
			id := h.Node.ID
			if inBatch[id] || seen[id] || candidates[id] != nil || h.Node.ValidAt.After(f.ValidAt) {
				continue
			}
			candidates[id] = h.Node
			order = append(order, id)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("NEW FACTS:\n")
	for i, f := range fresh {
		fmt.Fprintf(&b, "(%d) (%s) %s\n", i+1, f.ValidAt.Format("2006-01-02"), f.Body)
	}
	b.WriteString("\nEXISTING FACTS:\n")
	for _, id := range order {
		fmt.Fprintf(&b, "[%d] (%s) %s\n", id, candidates[id].ValidAt.Format("2006-01-02"), candidates[id].Body)
	}
	var out struct {
		Outdated []struct {
			ID int64 `json:"id"`
			By int   `json:"by"`
		} `json:"outdated"`
	}
	if err := llm.JSON(ctx, e.cfg.Extractor, llm.Request{
		Messages:    []llm.Message{{Role: "system", Content: reconcilePrompt}, {Role: "user", Content: b.String()}},
		Temperature: llm.Temp(0),
	}, &out); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	for _, o := range out.Outdated {
		old := candidates[o.ID]
		if old == nil || o.By < 1 || o.By > len(fresh) { // ignore ids the model made up
			continue
		}
		by := fresh[o.By-1]
		if old.ValidAt.After(by.ValidAt) {
			continue
		}
		if err := store.Invalidate(ctx, scope, []int64{o.ID}, by.ValidAt, by.ID); err != nil {
			return err
		}
	}
	return nil
}

// SearchOptions tunes Search. The zero value is a good default.
type SearchOptions struct {
	Facts          int  // facts to return (default 10)
	Episodes       int  // raw messages to return (default 5)
	IncludeHistory bool // also return facts that are no longer true
	Rerank         bool // rerank with Config.Reranker (one extra LLM call)
}

// Result is what the memory holds about a query.
type Result struct {
	Query    string
	Facts    []recall.Item
	Episodes []recall.Item
}

// Search retrieves the facts and raw messages most relevant to query.
// Without Rerank it makes no LLM call.
func (e *Engine) Search(ctx context.Context, scope, query string, opt SearchOptions) (*Result, error) {
	if opt.Facts <= 0 {
		opt.Facts = 10
	}
	if opt.Episodes <= 0 {
		opt.Episodes = 5
	}
	store := e.cfg.Store
	factKinds, epKinds := []graph.Kind{graph.KindFact}, []graph.Kind{graph.KindEpisode}
	// Over-fetch so fusion (and the reranker) have something to choose from.
	factOpt := graph.SearchOptions{Limit: opt.Facts * 3, IncludeInvalid: opt.IncludeHistory}
	epOpt := graph.SearchOptions{Limit: opt.Episodes * 3}

	channels := []recall.Channel{
		{Name: "facts", Weight: 1, Run: recall.Text(store, scope, factKinds, query, factOpt)},
		{Name: "graph", Weight: 0.7, Run: recall.ViaEntities(store, scope, query, 5, opt.Facts, opt.IncludeHistory)},
		{Name: "episodes", Weight: 1, Run: recall.Text(store, scope, epKinds, query, epOpt)},
	}
	if e.cfg.Embedder != nil {
		// One query embedding serves both channels. They are optional: if the
		// embedding API is down, search degrades to lexical + graph.
		qv := &recall.QueryVector{}
		channels = append(channels,
			recall.Channel{Name: "facts-vector", Weight: 1, Optional: true, Run: recall.Vector(store, e.cfg.Embedder, qv, scope, factKinds, query, factOpt)},
			recall.Channel{Name: "episodes-vector", Weight: 1, Optional: true, Run: recall.Vector(store, e.cfg.Embedder, qv, scope, epKinds, query, epOpt)},
		)
	}
	items, err := recall.Fuse(ctx, channels)
	if err != nil {
		return nil, err
	}

	if opt.Rerank && e.cfg.Reranker != nil {
		// Rerank only the head of each kind; the tail cannot make the cut anyway.
		var head []recall.Item
		nf, ne := 0, 0
		for _, it := range items {
			switch {
			case it.Node.Kind == graph.KindFact && nf < opt.Facts*2:
				nf++
				head = append(head, it)
			case it.Node.Kind == graph.KindEpisode && ne < opt.Episodes*2:
				ne++
				head = append(head, it)
			}
		}
		if reranked, err := recall.Rerank(ctx, e.cfg.Reranker, query, head); err == nil {
			items = reranked
		} // on failure keep the fused order: retrieval must not depend on the reranker
	}

	res := &Result{Query: query}
	for _, it := range items {
		switch it.Node.Kind {
		case graph.KindFact:
			if len(res.Facts) < opt.Facts {
				res.Facts = append(res.Facts, it)
			}
		case graph.KindEpisode:
			if len(res.Episodes) < opt.Episodes {
				res.Episodes = append(res.Episodes, it)
			}
		}
	}
	return res, nil
}

// Context renders the result as a block to place in a prompt, using at most
// about maxTokens tokens (0 = no limit). Facts come first because they are
// the densest; raw messages follow as supporting evidence.
func (r *Result) Context(maxTokens int) string {
	if len(r.Facts) == 0 && len(r.Episodes) == 0 {
		return ""
	}
	var b strings.Builder
	budget := maxTokens
	add := func(line string) bool {
		if maxTokens > 0 {
			t := textutil.EstimateTokens(line)
			if t > budget {
				return false
			}
			budget -= t
		}
		b.WriteString(line)
		return true
	}
	b.WriteString("<memory>\n")
	if len(r.Facts) > 0 {
		add("Known facts (most relevant first; the date is when each became true):\n")
		for _, it := range r.Facts {
			n := it.Node
			line := fmt.Sprintf("- [%s] %s", n.ValidAt.Format("2006-01-02"), n.Body)
			if !n.Valid() {
				line += fmt.Sprintf(" (no longer true since %s)", n.InvalidAt.Format("2006-01-02"))
			}
			if !add(line + "\n") {
				break
			}
		}
	}
	if len(r.Episodes) > 0 {
		add("Relevant past messages:\n")
		for _, it := range r.Episodes {
			n := it.Node
			if !add(fmt.Sprintf("- [%s] %s: %s\n", n.ValidAt.Format("2006-01-02"), n.Title, strings.ReplaceAll(n.Body, "\n", " "))) {
				break
			}
		}
	}
	b.WriteString("</memory>")
	return b.String()
}
