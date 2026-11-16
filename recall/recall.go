// Package recall is the retrieval layer shared by the memory and persona
// engines: independent search channels run in parallel goroutines and are
// merged with reciprocal rank fusion, then optionally reranked by an LLM.
package recall

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.vicoworks.com/open-persona/graph"
	"go.vicoworks.com/open-persona/internal/textutil"
	"go.vicoworks.com/open-persona/llm"
)

// Channel is one way of finding candidates. Add your own to customize
// retrieval; a channel only has to return hits in ranked order.
type Channel struct {
	Name   string
	Weight float64 // contribution to the fused score (default 1)
	Run    func(ctx context.Context) ([]graph.Hit, error)
	// Optional channels are skipped when they fail instead of failing the
	// search. Use it for channels that depend on a network call.
	Optional bool
}

// Item is a fused result.
type Item struct {
	Node     *graph.Node
	Score    float64
	Channels []string // which channels found it
}

// rrfK is the standard reciprocal-rank-fusion constant. Fusing on rank
// rather than raw score lets BM25, cosine and graph scores mix without
// calibration.
const rrfK = 60

// Fuse runs all channels concurrently and merges their rankings.
func Fuse(ctx context.Context, channels []Channel) ([]Item, error) {
	results := make([][]graph.Hit, len(channels))
	errs := make([]error, len(channels))
	var wg sync.WaitGroup
	for i, ch := range channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = ch.Run(ctx)
		}()
	}
	wg.Wait()

	byID := make(map[int64]*Item)
	var order []int64
	for i, hits := range results {
		if errs[i] != nil {
			if channels[i].Optional {
				continue
			}
			return nil, fmt.Errorf("recall: channel %s: %w", channels[i].Name, errs[i])
		}
		w := channels[i].Weight
		if w == 0 {
			w = 1
		}
		for rank, h := range hits {
			it := byID[h.Node.ID]
			if it == nil {
				it = &Item{Node: h.Node}
				byID[h.Node.ID] = it
				order = append(order, h.Node.ID)
			}
			it.Score += w / float64(rrfK+rank+1)
			it.Channels = append(it.Channels, channels[i].Name)
		}
	}
	out := make([]Item, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// Text returns a channel that ranks nodes of the given kinds with BM25.
func Text(s *graph.Store, scope string, kinds []graph.Kind, query string, opt graph.SearchOptions) func(context.Context) ([]graph.Hit, error) {
	return func(ctx context.Context) ([]graph.Hit, error) {
		return s.Search(ctx, scope, kinds, query, opt)
	}
}

// QueryVector embeds a query once, however many channels ask for it.
type QueryVector struct {
	once sync.Once
	vec  []float32
	err  error
}

func (q *QueryVector) get(ctx context.Context, emb llm.Embedder, query string) ([]float32, error) {
	q.once.Do(func() {
		vecs, err := emb.Embed(ctx, []string{query})
		if err == nil && len(vecs) != 1 {
			err = fmt.Errorf("embedder returned %d vectors for 1 text", len(vecs))
		}
		if err != nil {
			q.err = err
			return
		}
		q.vec = vecs[0]
	})
	return q.vec, q.err
}

// Vector returns a channel that ranks nodes by embedding similarity. Pass
// the same qv to several Vector channels of one search to share the query
// embedding; nil embeds separately.
func Vector(s *graph.Store, emb llm.Embedder, qv *QueryVector, scope string, kinds []graph.Kind, query string, opt graph.SearchOptions) func(context.Context) ([]graph.Hit, error) {
	if qv == nil {
		qv = &QueryVector{}
	}
	return func(ctx context.Context) ([]graph.Hit, error) {
		vec, err := qv.get(ctx, emb, query)
		if err != nil {
			return nil, err
		}
		return s.SearchVector(ctx, scope, kinds, vec, opt)
	}
}

// EmbedNodes embeds nodes in batches and stores the vectors. text chooses
// what represents each node.
func EmbedNodes(ctx context.Context, s *graph.Store, emb llm.Embedder, nodes []*graph.Node, text func(*graph.Node) string) error {
	const batch = 96
	for start := 0; start < len(nodes); start += batch {
		part := nodes[start:min(start+batch, len(nodes))]
		texts := make([]string, len(part))
		for i, n := range part {
			texts[i] = text(n)
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed: %w", err)
		}
		if len(vecs) != len(part) {
			return fmt.Errorf("embed: got %d vectors for %d texts", len(vecs), len(part))
		}
		byID := make(map[int64][]float32, len(part))
		for i, v := range vecs {
			byID[part[i].ID] = v
		}
		if err := s.PutVectors(ctx, byID); err != nil {
			return err
		}
	}
	return nil
}

// ViaEntities returns the graph channel: it finds the entities the query
// names, then walks one hop to the nodes that mention them. This surfaces
// facts that share no words with the query but are about the same thing.
// Results are interleaved across entities, newest first within each.
//
// Hub entities are skipped: an entity that a large share of the scope
// mentions (the user's own name in a personal memory, the company name in a
// policy pack) says nothing about which nodes are relevant.
func ViaEntities(s *graph.Store, scope, query string, maxEntities, perEntity int, includeInvalid bool) func(context.Context) ([]graph.Hit, error) {
	return func(ctx context.Context) ([]graph.Hit, error) {
		ents, err := s.Search(ctx, scope, []graph.Kind{graph.KindEntity}, query, graph.SearchOptions{Limit: maxEntities})
		if err != nil || len(ents) == 0 {
			return nil, err
		}
		ents, err = dropHubs(ctx, s, scope, ents)
		if err != nil || len(ents) == 0 {
			return nil, err
		}
		ids := make([]int64, len(ents))
		pos := make(map[int64]int, len(ents))
		for i, e := range ents {
			ids[i] = e.Node.ID
			pos[e.Node.ID] = i
		}
		links, err := s.Incoming(ctx, ids, graph.RelMentions, perEntity)
		if err != nil {
			return nil, err
		}
		// Bucket per entity (links arrive newest first), then interleave.
		buckets := make([][]*graph.Node, len(ents))
		for _, l := range links {
			if !includeInvalid && !l.Node.Valid() {
				continue
			}
			buckets[pos[l.From]] = append(buckets[pos[l.From]], l.Node)
		}
		var hits []graph.Hit
		seen := make(map[int64]bool)
		for depth := 0; depth < perEntity; depth++ {
			for _, b := range buckets {
				if depth < len(b) && !seen[b[depth].ID] {
					seen[b[depth].ID] = true
					hits = append(hits, graph.Hit{Node: b[depth], Score: 1 / float64(len(hits)+1)})
				}
			}
		}
		return hits, nil
	}
}

// An entity is a hub when more than hubMinDegree nodes mention it and they
// make up more than hubShare of all the nodes that could.
const (
	hubMinDegree = 4
	hubShare     = 0.4
)

func dropHubs(ctx context.Context, s *graph.Store, scope string, ents []graph.Hit) ([]graph.Hit, error) {
	ids := make([]int64, len(ents))
	for i, e := range ents {
		ids[i] = e.Node.ID
	}
	degree, err := s.InDegree(ctx, ids, graph.RelMentions)
	if err != nil {
		return nil, err
	}
	counts, err := s.Count(ctx, scope)
	if err != nil {
		return nil, err
	}
	total := 0 // nodes that mention entities: everything but raw text and entities
	for kind, n := range counts {
		if kind != graph.KindEntity && kind != graph.KindEpisode && kind != "source" {
			total += n
		}
	}
	kept := ents[:0:0]
	for _, e := range ents {
		d := degree[e.Node.ID]
		if d > hubMinDegree && float64(d) > hubShare*float64(total) {
			continue
		}
		kept = append(kept, e)
	}
	return kept, nil
}

const rerankPrompt = `You rank retrieved passages for a question.
Return JSON: {"ranked": [passage numbers, most useful first]}.
Include only passages that help answer the question; omit the rest.`

// Rerank asks an LLM to order items by usefulness for query (one call).
// Items the model omits keep their fused order after the ones it picked, so
// a bad rerank can demote a result but never lose it.
func Rerank(ctx context.Context, c llm.Client, query string, items []Item) ([]Item, error) {
	if len(items) < 2 {
		return items, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Question: %s\n\nPassages:\n", query)
	for i, it := range items {
		text := it.Node.Body
		if it.Node.Title != "" {
			text = it.Node.Title + ": " + text
		}
		fmt.Fprintf(&b, "[%d] %s\n", i+1, textutil.Truncate(strings.ReplaceAll(text, "\n", " "), 400))
	}
	var out struct {
		Ranked []int `json:"ranked"`
	}
	err := llm.JSON(ctx, c, llm.Request{
		Messages:    []llm.Message{{Role: "system", Content: rerankPrompt}, {Role: "user", Content: b.String()}},
		Temperature: llm.Temp(0),
	}, &out)
	if err != nil {
		return items, err
	}
	used := make([]bool, len(items))
	ranked := make([]Item, 0, len(items))
	for _, n := range out.Ranked {
		if n >= 1 && n <= len(items) && !used[n-1] {
			used[n-1] = true
			ranked = append(ranked, items[n-1])
		}
	}
	for i, it := range items {
		if !used[i] {
			ranked = append(ranked, it)
		}
	}
	return ranked, nil
}
