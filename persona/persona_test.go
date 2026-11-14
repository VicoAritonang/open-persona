package persona

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openpersona/openpersona/llm"
)

type fakeLLM func(req llm.Request) string

func (f fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	return llm.Response{Content: f(req)}, nil
}

const policy = `# Finance Policy

## Expenses

Expenses above Rp 5,000,000 need CFO approval before payment.

## Month-end close

Reconcile bank accounts, post accruals, then lock the period by working day 5.
`

func trainFake(t *testing.T) string {
	t.Helper()
	model := fakeLLM(func(req llm.Request) string {
		system, user := req.Messages[0].Content, req.Messages[1].Content
		switch {
		case strings.HasPrefix(system, "You are writing the core identity"):
			return `{"identity":"You are Fina, the finance controller.","principles":["Follow policy exactly."],"style":"Be precise."}`
		case strings.Contains(user, "CFO approval"):
			return `{"units":[{"kind":"rule","title":"CFO approval threshold","body":"WHEN an expense exceeds Rp 5,000,000 THEN obtain CFO approval before payment.","tags":["expenses","approval"]}]}`
		case strings.Contains(user, "Reconcile bank"):
			return `{"units":[{"kind":"procedure","title":"Month-end close steps","body":"1. Reconcile bank accounts. 2. Post accruals. 3. Lock the period by working day 5.","tags":["month-end close"]},
			                  {"kind":"bogus","title":"Close deadline","body":"The period locks on working day 5.","tags":["month-end close"]}]}`
		}
		return `{"units":[]}`
	})
	out := filepath.Join(t.TempDir(), "fina.persona")
	tr := Trainer{LLM: model, ChunkTokens: 40}
	report, err := tr.Train(context.Background(),
		Spec{Name: "Fina", Role: "Finance controller", Boundaries: []string{"Never approve payments yourself."}},
		[]Source{{Name: "policy", Text: policy}}, out)
	if err != nil {
		t.Fatal(err)
	}
	if report.Units != 3 || report.ByKind["rule"] != 1 || report.ByKind["reference"] != 1 {
		t.Fatalf("report: %+v", report)
	}
	return out
}

func TestTrainThenServeReadOnly(t *testing.T) {
	p, err := Open(trainFake(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !strings.Contains(p.Core, "You are Fina") || !strings.Contains(p.Core, "Never approve payments yourself.") {
		t.Errorf("core: %s", p.Core)
	}
	if !p.store.ReadOnly() {
		t.Fatal("runtime store must be read-only")
	}

	ctx := context.Background()
	k, err := p.Retrieve(ctx, "Who must approve a 7 million rupiah expense?", RetrieveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Units) == 0 || k.Units[0].Node.Title != "CFO approval threshold" {
		t.Fatalf("wrong top unit: %+v", k.Units)
	}
	prompt := p.SystemPrompt(k)
	for _, want := range []string{"<role>", "You are Fina", "[rule] CFO approval threshold", "Rp 5,000,000", "<instructions>"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}

	// The graph channel reaches "Close deadline" through the shared tag even
	// though the query matches only the procedure's words.
	k, _ = p.Retrieve(ctx, "month-end close", RetrieveOptions{Sources: -1})
	titles := ""
	for _, u := range k.Units {
		titles += u.Node.Title + "|"
	}
	if !strings.Contains(titles, "Month-end close steps") || !strings.Contains(titles, "Close deadline") {
		t.Errorf("units: %s", titles)
	}
	if len(k.Sources) != 0 {
		t.Error("Sources: -1 should disable source excerpts")
	}
}

func TestChatUsesPersonaPromptAndFollowUps(t *testing.T) {
	p, err := Open(trainFake(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var system string
	model := fakeLLM(func(req llm.Request) string {
		system = req.Messages[0].Content
		return "ok"
	})
	reply, err := p.Chat(context.Background(), model, []llm.Message{
		{Role: "user", Content: "Tell me about expense approval."},
		{Role: "assistant", Content: "Sure."},
		{Role: "user", Content: "and the limit?"}, // follow-up with no keywords of its own
	}, ChatOptions{Context: "<memory>User is the CFO's assistant.</memory>"})
	if err != nil || reply.Content != "ok" {
		t.Fatalf("chat: %v", err)
	}
	if !strings.Contains(system, "CFO approval threshold") {
		t.Error("follow-up lost the topic of the previous user turn")
	}
	if !strings.HasSuffix(system, "</memory>") {
		t.Error("extra context not appended")
	}
}
