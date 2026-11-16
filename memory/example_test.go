package memory_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.vicoworks.com/open-persona/graph"
	"go.vicoworks.com/open-persona/memory"
)

// Raw-only mode: no model configured, so nothing here costs anything.
// Messages are stored verbatim and found with BM25.
func Example() {
	dir, _ := os.MkdirTemp("", "openpersona")
	defer os.RemoveAll(dir)

	store, err := graph.Open(filepath.Join(dir, "memory.db"), graph.Options{})
	if err != nil {
		panic(err)
	}
	defer store.Close()
	mem, err := memory.New(memory.Config{Store: store})
	if err != nil {
		panic(err)
	}
	defer mem.Close()

	ctx := context.Background()
	mem.Add(ctx, "user-42", []memory.Message{
		{Role: "user", Content: "My cat Mochi is allergic to chicken."},
		{Role: "user", Content: "Remind me to renew my passport in March."},
	})

	res, err := mem.Search(ctx, "user-42", "What is Mochi allergic to?", memory.SearchOptions{Episodes: 1})
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Episodes[0].Node.Body)
	// Output: My cat Mochi is allergic to chicken.
}
