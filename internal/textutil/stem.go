package textutil

import "strings"

// Variants returns the index terms for one lowercase token: the token itself
// plus its candidate Indonesian root forms.
//
// Indonesian builds words with affixes ("setuju" -> "menyetujui",
// "persetujuan", "disetujui"), so exact-token search misses most inflections.
// Finding the one true root needs a dictionary; instead every plausible root
// is emitted on both the index and the query side, and BM25 matches on the
// ones they share. English words mostly pass through unchanged, and English
// inflection is handled afterwards by the Porter stemmer in the index.
func Variants(tok string) []string {
	out := []string{tok}
	if len(tok) < 5 || !isASCIILetters(tok) {
		return out
	}
	seen := map[string]struct{}{tok: {}}
	add := func(s string) {
		if _, dup := seen[s]; !dup && plausibleRoot(s) {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}

	// Inflectional endings: particle (-lah, -kah, -pun), then possessive (-ku, -mu, -nya).
	base := stripAny(tok, "lah", "kah", "pun")
	base = stripAny(base, "nya", "ku", "mu")
	// Derivational suffix (-kan, -an, -i): keep both readings.
	stems := []string{base}
	if s := stripAny(base, "kan", "an", "i"); s != base {
		stems = append(stems, s)
	}
	for _, s := range stems {
		if s != tok {
			add(s)
		}
		for _, r := range stripPrefix(s) {
			add(r)
			for _, r2 := range stripInner(r) { // second layer: di-per-, mem-ber-, ...
				add(r2)
			}
		}
	}
	return out
}

func isASCIILetters(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

func isVowel(c byte) bool { return strings.IndexByte("aiueo", c) >= 0 }

// plausibleRoot rejects fragments that cannot be an Indonesian root, which
// filters most accidental strips of English words ("server" -> "rver").
func plausibleRoot(s string) bool {
	return len(s) >= 3 && (isVowel(s[0]) || isVowel(s[1]))
}

// stripAny removes the first matching suffix, if at least 3 letters remain.
func stripAny(s string, suffixes ...string) string {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) && len(s)-len(suf) >= 3 {
			return s[:len(s)-len(suf)]
		}
	}
	return s
}

// stripPrefix returns the candidate roots after removing one verbal or
// nominal prefix. The nasal prefixes (meN-, peN-) swallow the first
// consonant of some roots, so each restored form is a candidate:
// "memukul" -> pukul, "menulis" -> tulis, "menyapu" -> sapu, "mengirim" -> kirim.
func stripPrefix(s string) []string {
	for _, p := range []string{"me", "pe"} {
		if !strings.HasPrefix(s, p) {
			continue
		}
		rest := s[2:]
		switch {
		case strings.HasPrefix(rest, "ng") && len(rest) > 3:
			r := rest[2:]
			if isVowel(r[0]) {
				return []string{r, "k" + r}
			}
			return []string{r}
		case strings.HasPrefix(rest, "ny") && len(rest) > 3:
			return []string{"s" + rest[2:]}
		case strings.HasPrefix(rest, "m") && len(rest) > 2:
			r := rest[1:]
			if isVowel(r[0]) {
				return []string{"p" + r, r, rest}
			}
			return []string{r}
		case strings.HasPrefix(rest, "n") && len(rest) > 2:
			r := rest[1:]
			if isVowel(r[0]) {
				return []string{"t" + r, r, rest}
			}
			return []string{r}
		case p == "pe" && strings.HasPrefix(rest, "r") && len(rest) > 3:
			return []string{rest[1:], rest}
		}
		return []string{rest}
	}
	for _, p := range []string{"ter", "ber", "di", "ke", "se"} {
		if strings.HasPrefix(s, p) && len(s)-len(p) >= 3 {
			return []string{s[len(p):]}
		}
	}
	return nil
}

// stripInner removes a second-layer prefix (per-, ber-).
func stripInner(s string) []string {
	for _, p := range []string{"per", "ber"} {
		if strings.HasPrefix(s, p) && len(s)-len(p) >= 3 {
			return []string{s[len(p):]}
		}
	}
	return nil
}
