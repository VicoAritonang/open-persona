// Package textutil holds the small text helpers shared by every engine:
// tokenizing, building full-text queries, chunking and token estimates.
package textutil

import (
	"strings"
	"unicode"
)

// Tokenize lowercases s and splits it on anything that is not a letter or digit.
func Tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// stopwords covers English and Indonesian function words. They carry no
// ranking signal and make OR-queries slow, so they are dropped from queries.
var stopwords = func() map[string]struct{} {
	words := `a an and are as at be been but by can could did do does for from had has have he her him his how i if in into is it its
me my of on or our she so than that the their them then there these they this to was we were what when where which who whom why will
with would you your about after before
yang dan di ke dari untuk dengan pada adalah itu ini atau juga saya aku kamu anda dia mereka kami kita akan sudah telah tidak bukan
apa siapa kapan dimana mana bagaimana mengapa kenapa berapa apakah ada bisa dapat harus oleh sebagai dalam jika kalau agar supaya
karena tetapi tapi namun lalu serta nya lah kah pun saat ketika seperti lebih sangat hanya saja`
	m := make(map[string]struct{})
	for _, w := range strings.Fields(words) {
		m[w] = struct{}{}
	}
	return m
}()

// IsStopword reports whether the lowercase token w is a stopword.
func IsStopword(w string) bool {
	_, ok := stopwords[w]
	return ok
}

// Keywords returns the unique non-stopword tokens of s, in order.
// If every token is a stopword it falls back to all tokens.
func Keywords(s string) []string {
	toks := Tokenize(s)
	seen := make(map[string]struct{}, len(toks))
	var out []string
	for _, t := range toks {
		if IsStopword(t) {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		for _, t := range toks {
			if _, dup := seen[t]; !dup {
				seen[t] = struct{}{}
				out = append(out, t)
			}
		}
	}
	return out
}

// maxQueryTerms bounds the size of a generated full-text query.
const maxQueryTerms = 40

// Analyzer expands one lowercase token into its index terms (the token and
// any normalized forms). It runs on both indexed text and queries.
type Analyzer func(token string) []string

// IndexText rewrites s into the terms that get indexed.
func IndexText(s string, analyze Analyzer) string {
	var b strings.Builder
	for _, tok := range Tokenize(s) {
		for _, term := range analyze(tok) {
			b.WriteString(term)
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// FTSQuery turns free text into an FTS5 expression: the analyzed keywords
// OR-ed together, each quoted so user input can never be read as FTS5
// syntax. BM25 then ranks documents by how many (and how rare) terms they
// match. It returns "" when s has no usable tokens.
func FTSQuery(s string, analyze Analyzer) string {
	kw := Keywords(s)
	if len(kw) > maxQueryTerms {
		kw = kw[:maxQueryTerms]
	}
	var b strings.Builder
	seen := make(map[string]struct{})
	for _, k := range kw {
		for _, term := range analyze(k) {
			if _, dup := seen[term]; dup {
				continue
			}
			seen[term] = struct{}{}
			if b.Len() > 0 {
				b.WriteString(" OR ")
			}
			b.WriteByte('"')
			b.WriteString(term)
			b.WriteByte('"')
		}
	}
	return b.String()
}

// FTSPhrase quotes s as a single FTS5 phrase.
func FTSPhrase(s string) string {
	return `"` + strings.Join(Tokenize(s), " ") + `"`
}

// EstimateTokens approximates the LLM token count of s (about 4 chars per token).
func EstimateTokens(s string) int {
	return (len([]rune(s)) + 3) / 4
}

// Normalize lowercases s and collapses it to single-spaced tokens. It is the
// identity key used to deduplicate entities, facts and knowledge units.
func Normalize(s string) string {
	return strings.Join(Tokenize(s), " ")
}

// Truncate shortens s to at most n runes, adding an ellipsis when cut.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Chunk is one piece of a document with the heading path it sits under.
type Chunk struct {
	Heading string // e.g. "Policy > Expenses > Approval"
	Text    string
}

// ChunkMarkdown splits text into chunks of roughly maxTokens, breaking on
// blank lines and never inside a paragraph unless the paragraph alone is too
// big. It tracks markdown headings so each chunk knows its place in the
// document; prefixing that path to the chunk gives contextual retrieval
// without spending an LLM call.
func ChunkMarkdown(text string, maxTokens int) []Chunk {
	if maxTokens <= 0 {
		maxTokens = 800
	}
	var (
		chunks  []Chunk
		path    []string // heading stack, index = level-1
		cur     strings.Builder
		curHead string
	)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			chunks = append(chunks, Chunk{Heading: curHead, Text: s})
		}
		cur.Reset()
	}
	headingPath := func() string {
		var parts []string
		for _, p := range path {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, " > ")
	}

	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		// A paragraph may start with a heading line.
		first, rest, _ := strings.Cut(para, "\n")
		if level := headingLevel(first); level > 0 {
			flush()
			for len(path) < level {
				path = append(path, "")
			}
			path = path[:level]
			path[level-1] = strings.TrimSpace(strings.TrimLeft(first, "# "))
			curHead = headingPath()
			para = strings.TrimSpace(rest)
			if para == "" {
				continue
			}
		}
		if cur.Len() == 0 {
			curHead = headingPath()
		}
		if cur.Len() > 0 && EstimateTokens(cur.String())+EstimateTokens(para) > maxTokens {
			flush()
			curHead = headingPath()
		}
		for EstimateTokens(para) > maxTokens { // oversized paragraph: hard split
			r := []rune(para)
			cut := maxTokens * 4
			if i := lastBreak(r[:cut]); i > cut/2 {
				cut = i
			}
			chunks = append(chunks, Chunk{Heading: curHead, Text: strings.TrimSpace(string(r[:cut]))})
			para = strings.TrimSpace(string(r[cut:]))
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(para)
	}
	flush()
	return chunks
}

func headingLevel(line string) int {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(line) || line[n] != ' ' {
		return 0
	}
	return n
}

// lastBreak finds the last sentence or line end in r, or -1.
func lastBreak(r []rune) int {
	for i := len(r) - 1; i > 0; i-- {
		if r[i] == '\n' || (r[i] == ' ' && (r[i-1] == '.' || r[i-1] == '?' || r[i-1] == '!')) {
			return i
		}
	}
	return -1
}
