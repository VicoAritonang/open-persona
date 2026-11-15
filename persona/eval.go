package persona

import (
	"context"
	"strings"
	"sync"
	"unicode"

	"go.vicoworks.com/openpersona/internal/textutil"
	"go.vicoworks.com/openpersona/llm"
)

// EvalCase is one held-out question with the facts a good answer needs.
type EvalCase struct {
	Q string `json:"q"`
	// Expect lists strings that must reach the prompt (and, when answers
	// are scored, the answer). "a|b" accepts either. Matching ignores case,
	// spaces and punctuation, so "Rp 25.000.000" matches "Rp25.000.000".
	Expect []string `json:"expect"`
}

// EvalOptions tunes Evaluate.
type EvalOptions struct {
	Retrieve RetrieveOptions
	// Chat, when set, also answers every question as the persona and checks
	// the expected strings in the answer. One chat call per question.
	Chat llm.Client
	// Concurrency is the number of questions scored in parallel (default 4).
	Concurrency int
}

// EvalCaseResult is the outcome of one question.
type EvalCaseResult struct {
	Q             string   `json:"q"`
	Missing       []string `json:"missing,omitempty"` // expected strings not in the retrieved knowledge
	TopUnits      []string `json:"top_units,omitempty"`
	Answer        string   `json:"answer,omitempty"`
	AnswerMissing []string `json:"answer_missing,omitempty"`
}

// EvalResult scores a pack against a question set.
type EvalResult struct {
	Cases    int     `json:"cases"`
	Passed   int     `json:"passed"`   // questions whose every expected string was retrieved
	Coverage float64 `json:"coverage"` // share of all expected strings that were retrieved

	Answered       bool      `json:"answered,omitempty"`
	AnswerPassed   int       `json:"answer_passed,omitempty"`
	AnswerCoverage float64   `json:"answer_coverage,omitempty"`
	Usage          llm.Usage `json:"usage,omitzero"`

	Results []EvalCaseResult `json:"results"`
}

// EvalSummary is an EvalResult without the per-question detail.
type EvalSummary struct {
	Cases          int     `json:"cases"`
	Passed         int     `json:"passed"`
	Coverage       float64 `json:"coverage"`
	AnswerPassed   int     `json:"answer_passed,omitempty"`
	AnswerCoverage float64 `json:"answer_coverage,omitempty"`
}

// Summary drops the per-question detail.
func (r *EvalResult) Summary() *EvalSummary {
	return &EvalSummary{Cases: r.Cases, Passed: r.Passed, Coverage: r.Coverage, AnswerPassed: r.AnswerPassed, AnswerCoverage: r.AnswerCoverage}
}

// Evaluate measures whether the knowledge a question needs reaches the
// prompt. Retrieval needs no LLM (only a query embedding when the pack has
// vectors), so scoring a question set is close to free; opt.Chat adds an
// end-to-end check of the answers at one chat call per question.
func (p *Persona) Evaluate(ctx context.Context, cases []EvalCase, opt EvalOptions) (*EvalResult, error) {
	workers := opt.Concurrency
	if workers <= 0 {
		workers = 4
	}
	res := &EvalResult{Cases: len(cases), Answered: opt.Chat != nil, Results: make([]EvalCaseResult, len(cases))}
	meter := llm.NewMeter(opt.Chat)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, workers)
	)
	for i, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := p.evalOne(ctx, c, opt, meter)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			res.Results[i] = r
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	var expected, found, answerFound int
	for i, c := range cases {
		r := res.Results[i]
		expected += len(c.Expect)
		found += len(c.Expect) - len(r.Missing)
		if len(r.Missing) == 0 {
			res.Passed++
		}
		if res.Answered {
			answerFound += len(c.Expect) - len(r.AnswerMissing)
			if len(r.AnswerMissing) == 0 {
				res.AnswerPassed++
			}
		}
	}
	if expected > 0 {
		res.Coverage = float64(found) / float64(expected)
		if res.Answered {
			res.AnswerCoverage = float64(answerFound) / float64(expected)
		}
	}
	if opt.Chat != nil {
		res.Usage = meter.Usage()
	}
	return res, nil
}

func (p *Persona) evalOne(ctx context.Context, c EvalCase, opt EvalOptions, chat llm.Client) (EvalCaseResult, error) {
	r := EvalCaseResult{Q: c.Q}
	k, err := p.Retrieve(ctx, c.Q, opt.Retrieve)
	if err != nil {
		return r, err
	}
	var text strings.Builder
	for i, it := range k.Units {
		text.WriteString(it.Node.Title + "\n" + it.Node.Body + "\n")
		if i < 3 {
			r.TopUnits = append(r.TopUnits, it.Node.Title)
		}
	}
	for _, it := range k.Sources {
		text.WriteString(textutil.Truncate(it.Node.Body, 2400) + "\n")
	}
	r.Missing = missing(text.String(), c.Expect)
	if opt.Chat != nil {
		resp, err := chat.Complete(ctx, llm.Request{
			Messages:    []llm.Message{{Role: "system", Content: p.SystemPrompt(k)}, {Role: "user", Content: c.Q}},
			Temperature: llm.Temp(0),
		})
		if err != nil {
			return r, err
		}
		r.Answer = resp.Content
		r.AnswerMissing = missing(resp.Content, c.Expect)
	}
	return r, nil
}

// missing returns the expectations that do not occur in text.
func missing(text string, expect []string) []string {
	hay := squash(text)
	var out []string
	for _, e := range expect {
		ok := false
		for _, alt := range strings.Split(e, "|") {
			if a := squash(alt); a != "" && strings.Contains(hay, a) {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, e)
		}
	}
	return out
}

// squash lowercases s and keeps only letters, digits and "%".
func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '%' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
