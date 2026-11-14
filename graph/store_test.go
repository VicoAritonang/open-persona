package graph

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "g.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSearchRanksAndIsolatesScopes(t *testing.T) {
	s, ctx := open(t), context.Background()
	nodes := []*Node{
		{Scope: "user-1", Kind: KindFact, Body: "Budi works at Bank Mandiri as a credit analyst"},
		{Scope: "user-1", Kind: KindFact, Body: "Budi likes hiking in Bandung"},
		{Scope: "user-2", Kind: KindFact, Body: "Sari works at Bank Mandiri"},
		{Scope: "user-1", Kind: KindEpisode, Title: "user", Body: "I work at a bank"},
	}
	if err := s.Insert(ctx, nodes, nil); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, "user-1", []Kind{KindFact}, "Where does Budi work? (bank)", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Node.ID != nodes[0].ID {
		t.Fatalf("want the Mandiri fact first of 2 hits, got %d hits: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.Node.Scope != "user-1" || h.Node.Kind != KindFact {
			t.Errorf("leaked node %+v", h.Node)
		}
	}
	// Stemming: "hikes" must find "hiking".
	if hits, _ := s.Search(ctx, "user-1", []Kind{KindFact}, "hikes", SearchOptions{}); len(hits) != 1 {
		t.Errorf("stemmed search: got %d hits, want 1", len(hits))
	}
	// Query syntax characters must not break the query.
	if _, err := s.Search(ctx, "user-1", []Kind{KindFact}, `"AND" NOT (bank* OR -x:`, SearchOptions{}); err != nil {
		t.Errorf("hostile query: %v", err)
	}
}

func TestKeyDedupeEdgesAndInvalidate(t *testing.T) {
	s, ctx := open(t), context.Background()
	first := []*Node{
		{Scope: "u", Kind: KindFact, Body: "User lives in Jakarta", Key: "user lives in jakarta"},
		{Scope: "u", Kind: KindEntity, Title: "Jakarta", Key: "jakarta"},
	}
	if err := s.Insert(ctx, first, []Edge{{Src: Ref(0), Dst: Ref(1), Rel: RelMentions}}); err != nil {
		t.Fatal(err)
	}
	// Same entity key resolves to the existing node.
	second := []*Node{
		{Scope: "u", Kind: KindFact, Body: "User moved to Surabaya", ValidAt: time.Now().Add(time.Hour)},
		{Scope: "u", Kind: KindEntity, Title: "jakarta", Key: "jakarta"},
	}
	if err := s.Insert(ctx, second, []Edge{{Src: Ref(0), Dst: Ref(1), Rel: RelMentions}}); err != nil {
		t.Fatal(err)
	}
	if second[1].ID != first[1].ID {
		t.Fatalf("entity not deduplicated: %d vs %d", second[1].ID, first[1].ID)
	}
	links, err := s.Incoming(ctx, []int64{first[1].ID}, RelMentions, 10)
	if err != nil || len(links) != 2 {
		t.Fatalf("incoming: %d links, err %v", len(links), err)
	}
	if links[0].Node.ID != second[0].ID {
		t.Errorf("incoming should be newest first")
	}

	if err := s.Invalidate(ctx, "u", []int64{first[0].ID}, time.Now(), second[0].ID); err != nil {
		t.Fatal(err)
	}
	if hits, _ := s.Search(ctx, "u", []Kind{KindFact}, "Jakarta", SearchOptions{}); len(hits) != 0 {
		t.Errorf("invalidated fact still returned")
	}
	hits, _ := s.Search(ctx, "u", []Kind{KindFact}, "Jakarta", SearchOptions{IncludeInvalid: true})
	if len(hits) != 1 || hits[0].Node.Valid() || hits[0].Node.SupersededBy != second[0].ID {
		t.Errorf("history lost: %+v", hits)
	}
	// An invalidated key can be asserted again as a new node.
	again := []*Node{{Scope: "u", Kind: KindFact, Body: "User lives in Jakarta", Key: "user lives in jakarta"}}
	if err := s.Insert(ctx, again, nil); err != nil || again[0].ID == first[0].ID {
		t.Errorf("re-assertion: id %d, err %v", again[0].ID, err)
	}
}

func TestVectorSearch(t *testing.T) {
	s, ctx := open(t), context.Background()
	nodes := []*Node{{Scope: "u", Kind: KindFact, Body: "a"}, {Scope: "u", Kind: KindFact, Body: "b"}}
	if err := s.Insert(ctx, nodes, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PutVectors(ctx, map[int64][]float32{nodes[0].ID: {1, 0}, nodes[1].ID: {0, 1}}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchVector(ctx, "u", []Kind{KindFact}, []float32{0.1, 0.9}, SearchOptions{})
	if err != nil || len(hits) != 2 || hits[0].Node.ID != nodes[1].ID {
		t.Fatalf("vector search: %+v, err %v", hits, err)
	}
}

func TestFreezeMakesReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pack.db")
	ctx := context.Background()
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, []*Node{{Scope: "p", Kind: KindFact, Body: "expense approval limit"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("frozen store opened for writing")
	}
	ro, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.Insert(ctx, []*Node{{Scope: "p", Kind: KindFact, Body: "x"}}, nil); !errors.Is(err, ErrReadOnly) {
		t.Errorf("insert on read-only: %v", err)
	}
	if hits, err := ro.Search(ctx, "p", []Kind{KindFact}, "approval", SearchOptions{}); err != nil || len(hits) != 1 {
		t.Errorf("search on frozen store: %d hits, err %v", len(hits), err)
	}
}
