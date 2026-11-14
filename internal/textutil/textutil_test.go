package textutil

import (
	"slices"
	"strings"
	"testing"
)

func TestVariantsShareIndonesianRoot(t *testing.T) {
	// Each group is one root in different inflections; all must share a term.
	groups := [][]string{
		{"setuju", "menyetujui", "persetujuan", "disetujui"},
		{"pecah", "memecah", "dipecah"},
		{"beli", "membeli", "pembelian", "dibelikan"},
		{"bayar", "pembayaran", "dibayarkan", "membayar"},
		{"kirim", "mengirim", "pengiriman", "dikirimkan"},
		{"tulis", "menulis", "penulisan", "ditulisnya"},
	}
	for _, g := range groups {
		root := g[0]
		for _, w := range g {
			if !slices.Contains(Variants(w), root) {
				t.Errorf("%s -> %v, want it to include %q", w, Variants(w), root)
			}
		}
	}
	// English and short words pass through without junk.
	for _, w := range []string{"server", "budget", "kernel", "go", "2026"} {
		if v := Variants(w); len(v) != 1 {
			t.Errorf("%s -> %v, want only itself", w, v)
		}
	}
}

func TestFTSQueryIsSafe(t *testing.T) {
	q := FTSQuery(`Who "approves" (expenses) AND NOT x:*?`, Variants)
	if strings.ContainsAny(strings.ReplaceAll(q, `"`, ""), `()*:`) {
		t.Errorf("unsafe query: %s", q)
	}
	if FTSQuery("???", Variants) != "" {
		t.Error("empty input should give an empty query")
	}
	if q := FTSQuery("the of and", Variants); q == "" {
		t.Error("all-stopword input should still search")
	}
}

func TestChunkMarkdownKeepsHeadingPath(t *testing.T) {
	doc := "# Policy\n\nIntro.\n\n## Expenses\n\nRule one.\n\nRule two.\n\n## Close\n\n" + strings.Repeat("Long sentence here. ", 60)
	chunks := ChunkMarkdown(doc, 50)
	if chunks[0].Heading != "Policy" || chunks[1].Heading != "Policy > Expenses" {
		t.Fatalf("headings: %+v", chunks[:2])
	}
	if !strings.Contains(chunks[1].Text, "Rule one.") || !strings.Contains(chunks[1].Text, "Rule two.") {
		t.Errorf("small paragraphs under one heading should share a chunk: %q", chunks[1].Text)
	}
	last := chunks[len(chunks)-1]
	if last.Heading != "Policy > Close" || len(chunks) < 4 {
		t.Errorf("oversized paragraph should be split under its heading: %d chunks, last %q", len(chunks), last.Heading)
	}
	for _, c := range chunks {
		if EstimateTokens(c.Text) > 60 {
			t.Errorf("chunk too large: %d tokens", EstimateTokens(c.Text))
		}
	}
}
