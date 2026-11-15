package persona

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"go.vicoworks.com/openpersona/llm"
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

func trainFake(t *testing.T) string { return trainFakeWith(t, Trainer{}, Spec{}) }

// trainFakeWith trains the test policy with a fake LLM. Fields left zero in
// tr and spec get the test defaults.
func trainFakeWith(t *testing.T, tr Trainer, spec Spec) string {
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
	tr.LLM, tr.ChunkTokens = model, 40
	if spec.Name == "" {
		spec.Name, spec.Role, spec.Boundaries = "Fina", "Finance controller", []string{"Never approve payments yourself."}
	}
	report, err := tr.Train(context.Background(), spec, []Source{{Name: "policy", Text: policy}}, out)
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

// Two places with an "Opening hours" unit each: the reduce step must keep
// both instead of merging them by title.
func TestSameTitleDifferentKnowledgeKeepsBoth(t *testing.T) {
	model := fakeLLM(func(req llm.Request) string {
		system, user := req.Messages[0].Content, req.Messages[1].Content
		switch {
		case strings.HasPrefix(system, "You are writing the core identity"):
			return `{"identity":"You are Kora.","principles":[],"style":""}`
		case strings.Contains(user, "Senso-ji"):
			return `{"units":[{"kind":"reference","title":"Opening hours","body":"Senso-ji main hall opens 06:00 to 17:00, free entry.","tags":["Asakusa"]}]}`
		case strings.Contains(user, "Kinkaku-ji"):
			return `{"units":[{"kind":"reference","title":"Opening hours","body":"Kinkaku-ji opens 09:00 to 17:00, entry 500 yen.","tags":["Kyoto"]},
			                  {"kind":"reference","title":"Opening hours","body":"Kinkaku-ji is open 09:00-17:00 daily; entry costs 500 yen.","tags":["Kyoto"]}]}`
		}
		return `{"units":[]}`
	})
	src := "# Tokyo\n\n## Asakusa\n\nSenso-ji main hall opens 06:00 to 17:00.\n\n# Kyoto\n\n## Kinkaku-ji\n\nKinkaku-ji opens 09:00 to 17:00, entry 500 yen.\n"
	out := filepath.Join(t.TempDir(), "kora.persona")
	spec := Spec{Name: "Kora", Role: "Travel planner", Retrieve: &RetrieveDefaults{Units: 1, Sources: -1}}
	report, err := (&Trainer{LLM: model, ChunkTokens: 20}).Train(context.Background(), spec, []Source{{Name: "guide", Text: src}}, out)
	if err != nil {
		t.Fatal(err)
	}
	// Senso-ji and Kinkaku-ji kept apart; the two Kinkaku-ji paraphrases merged.
	if report.Units != 2 {
		t.Fatalf("want 2 units, got %+v", report)
	}
	p, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	units, _ := p.Units(context.Background())
	titles := ""
	for _, u := range units {
		titles += u.Title + "|"
	}
	if !strings.Contains(titles, "Opening hours (Kyoto > Kinkaku-ji)") && !strings.Contains(titles, "Opening hours (Kinkaku-ji)") {
		t.Errorf("titles: %s", titles)
	}

	// The spec's retrieval budget applies when the caller sets none.
	k, err := p.Retrieve(context.Background(), "opening hours", RetrieveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Units) != 1 || len(k.Sources) != 0 {
		t.Errorf("spec budget ignored: %d units, %d sources", len(k.Units), len(k.Sources))
	}
	if k, _ = p.Retrieve(context.Background(), "opening hours", RetrieveOptions{Units: 2}); len(k.Units) != 2 {
		t.Errorf("caller budget ignored: %d units", len(k.Units))
	}
}

func TestFacets(t *testing.T) {
	q := "Saya punya budget Rp 20.000.000 dan tinggal di Jakarta. Rapat di Kariya, Aichi tanggal 12 Maret jam 14:00! Setelah itu wisata tiga hari di Kyoto?"
	got := facets(q)
	if len(got) != 3 || !strings.Contains(got[0], "20.000.000") || !strings.HasPrefix(got[2], "Setelah itu") {
		t.Fatalf("facets: %q", got)
	}
	if f := facets("Who approves a 7 million rupiah expense?"); f != nil {
		t.Errorf("single question split into %q", f)
	}
	if f := facets("Hi. Thanks."); f != nil {
		t.Errorf("short sentences kept: %q", f)
	}
}
