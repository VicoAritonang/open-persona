package memory

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openpersona/openpersona/graph"
	"github.com/openpersona/openpersona/llm"
)

type fakeLLM func(req llm.Request) string

func (f fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	return llm.Response{Content: f(req), Usage: llm.Usage{Calls: 1}}, nil
}

func newEngine(t *testing.T, extractor llm.Client) (*Engine, *graph.Store) {
	t.Helper()
	store, err := graph.Open(filepath.Join(t.TempDir(), "m.db"), graph.Options{})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(Config{Store: store, Extractor: extractor})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close(); store.Close() })
	return eng, store
}

func TestRawOnlyModeNeedsNoLLM(t *testing.T) {
	eng, _ := newEngine(t, nil)
	ctx := context.Background()
	err := eng.Add(ctx, "u1", []Message{
		{Role: "user", Content: "My cat Mochi is allergic to chicken."},
		{Role: "assistant", Content: "Noted, I will avoid chicken-based food for Mochi."},
		{Role: "user", Content: "Also remind me to renew my passport."},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Search(ctx, "u1", "what is Mochi allergic to?", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Facts) != 0 || len(res.Episodes) == 0 || !strings.Contains(res.Episodes[0].Node.Body, "allergic") {
		t.Fatalf("unexpected result: %+v", res)
	}
	if ctxBlock := res.Context(0); !strings.Contains(ctxBlock, "Mochi") {
		t.Errorf("context block misses the evidence:\n%s", ctxBlock)
	}
	if res, _ := eng.Search(ctx, "someone-else", "Mochi", SearchOptions{}); len(res.Episodes) != 0 {
		t.Error("scope leak")
	}
}

func TestExtractionSupersedesAndLinksEntities(t *testing.T) {
	var calls atomic.Int32
	idRe := regexp.MustCompile(`\[(\d+)\] \([^)]*\) Rina works at Tokopedia`)
	extractor := fakeLLM(func(req llm.Request) string {
		calls.Add(1)
		system, user := req.Messages[0].Content, req.Messages[1].Content
		switch {
		case strings.HasPrefix(system, "You keep a memory of facts consistent"):
			// The raw message "I joined Gojek" shares no words with the
			// Tokopedia fact; only this second pass can see the conflict.
			m := idRe.FindStringSubmatch(user)
			if m == nil {
				t.Errorf("reconcile was not shown the existing fact:\n%s", user)
				return `{"outdated":[]}`
			}
			// 999 is an id the model made up; it must be ignored.
			return `{"outdated":[{"id":` + m[1] + `,"by":1},{"id":999,"by":1}]}`
		case strings.Contains(user, "joined Gojek"):
			return `{"facts":[{"text":"Rina works at Gojek as a data engineer","date":"2026-06-01","entities":["Rina","Gojek"],"supersedes":[999]}]}`
		case strings.Contains(user, "Tokopedia"):
			return `Sure! {"facts":[{"text":"Rina works at Tokopedia","date":"2026-01-10","entities":["Rina","Tokopedia","User"],"supersedes":[]},
			                 {"text":"Rina's sister is named Dewi","date":"2026-01-10","entities":["Rina","Dewi"],"supersedes":[]}]}`
		}
		return `{"facts":[]}`
	})
	eng, store := newEngine(t, extractor)
	ctx := context.Background()
	jan := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	jun := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	if err := eng.Add(ctx, "rina", []Message{{Role: "user", Content: "I work at Tokopedia. My sister is Dewi.", Time: jan}}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.Add(ctx, "rina", []Message{{Role: "user", Content: "Big news, I joined Gojek as a data engineer!", Time: jun}}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// Batch 1: one call (nothing to reconcile). Batch 2: extraction + reconcile.
	if calls.Load() != 3 {
		t.Errorf("got %d LLM calls, want 3", calls.Load())
	}

	res, err := eng.Search(ctx, "rina", "Where does Rina work?", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Facts) == 0 || !strings.Contains(res.Facts[0].Node.Body, "Gojek") {
		t.Fatalf("current employer should rank first: %s", res.Context(0))
	}
	for _, f := range res.Facts {
		if strings.Contains(f.Node.Body, "Tokopedia") {
			t.Errorf("superseded fact returned by default: %s", f.Node.Body)
		}
	}
	// History is kept and labelled.
	hist, _ := eng.Search(ctx, "rina", "Where does Rina work?", SearchOptions{IncludeHistory: true})
	if block := hist.Context(0); !strings.Contains(block, "Tokopedia (no longer true since 2026-06-01)") {
		t.Errorf("history not rendered:\n%s", block)
	}
	// Graph channel: "Dewi" shares no keyword with the sister fact's query,
	// but the entity link from Rina reaches it.
	res, _ = eng.Search(ctx, "rina", "Tell me about Rina", SearchOptions{})
	var sawSister bool
	for _, f := range res.Facts {
		sawSister = sawSister || strings.Contains(f.Node.Body, "Dewi")
	}
	if !sawSister {
		t.Errorf("entity graph did not surface the related fact: %s", res.Context(0))
	}
	counts, _ := store.Count(ctx, "rina")
	if counts[graph.KindEntity] != 4 { // Rina, Tokopedia, Dewi, Gojek - not "User"
		t.Errorf("entities = %d, want 4", counts[graph.KindEntity])
	}
}

func TestContextRespectsTokenBudget(t *testing.T) {
	eng, _ := newEngine(t, nil)
	ctx := context.Background()
	var msgs []Message
	for i := 0; i < 20; i++ {
		msgs = append(msgs, Message{Role: "user", Content: "budget note " + strings.Repeat("lorem ipsum dolor ", 20)})
	}
	if err := eng.Add(ctx, "u", msgs); err != nil {
		t.Fatal(err)
	}
	res, _ := eng.Search(ctx, "u", "budget note", SearchOptions{Episodes: 20})
	small, full := res.Context(200), res.Context(0)
	if len(small) >= len(full) || len(small) > 200*4+64 {
		t.Errorf("budget not applied: %d chars with budget vs %d without", len(small), len(full))
	}
}
