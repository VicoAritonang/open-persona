package openpersona

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.vicoworks.com/openpersona/cloud"
	"go.vicoworks.com/openpersona/llm"
	"go.vicoworks.com/openpersona/persona"
	"go.vicoworks.com/openpersona/server"
)

// fakeModel extracts two units at training time and answers chats in
// three streamed pieces.
type fakeModel struct{}

const fakeAnswer = "Naik JR Tokaido, 15-20 menit."

func (fakeModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	system := req.Messages[0].Content
	switch {
	case strings.HasPrefix(system, "You are writing the core identity"):
		return llm.Response{Content: `{"identity":"You are Kora, a travel planner.","principles":["Plan by the hour."],"style":"Be practical."}`}, nil
	case strings.HasPrefix(system, "You are compiling"):
		return llm.Response{Content: `{"units":[
			{"kind":"reference","title":"Kereta Nagoya ke Kariya","body":"JR Tokaido Line dari Nagoya ke Kariya 15-20 menit, sekitar 420 yen.","tags":["Kariya","Nagoya"]},
			{"kind":"rule","title":"Tiba sebelum janji bisnis","body":"WHEN ada janji bisnis THEN tiba di kota tujuan paling lambat malam sebelumnya.","tags":["bisnis"]}]}`}, nil
	}
	return llm.Response{Content: fakeAnswer, Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 10, Calls: 1}}, nil
}

func (m fakeModel) Stream(ctx context.Context, req llm.Request, onDelta func(string) error) (llm.Response, error) {
	resp, _ := m.Complete(ctx, req)
	for _, piece := range []string{"Naik JR Tokaido,", " 15-20", " menit."} {
		if err := onDelta(piece); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

func trainPack(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "kora.persona")
	spec := persona.Spec{ID: "kora", Version: "1.0.0", Name: "Kora", Role: "Travel planner", Expertise: []string{"Japan rail"}}
	src := "# Aichi\n\n## Kariya\n\nJR Tokaido Line Nagoya to Kariya, 15-20 minutes.\n"
	if _, err := (&persona.Trainer{LLM: fakeModel{}}).Train(context.Background(), spec, []persona.Source{{Name: "aichi", Text: src}}, out); err != nil {
		t.Fatal(err)
	}
	return out
}

// keys maps tokens to principals.
type keys map[string]*cloud.Principal

func (k keys) Authenticate(_ context.Context, token string) (*cloud.Principal, error) {
	if p := k[token]; p != nil {
		return p, nil
	}
	return nil, cloud.ErrUnauthorized
}

func startService(t *testing.T, pack string) (*httptest.Server, *Local) {
	t.Helper()
	p, err := persona.Open(pack)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	srv := &server.Server{
		Personas: map[string]*persona.Persona{"kora": p},
		Chat:     fakeModel{},
		Jobs:     &cloud.MemoryJobs{},
		Auth: keys{
			"full":     {KeyID: "k1", Personas: []string{"*"}, Scopes: cloud.AllScopes},
			"readonly": {KeyID: "k2", Personas: []string{"kora"}, Scopes: []string{cloud.ScopeKnowledge}},
		},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	local, err := Open(pack, LocalOptions{Chat: fakeModel{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	return ts, local
}

// The same calls work on a hosted and a local persona.
func TestHostedAndLocalBehaveAlike(t *testing.T) {
	ctx := context.Background()
	ts, local := startService(t, trainPack(t))
	client := NewClient("full", ClientOptions{BaseURL: ts.URL, PollInterval: 10e6})
	remote := client.Persona("kora")

	for name, p := range map[string]Persona{"hosted": remote, "local": local} {
		t.Run(name, func(t *testing.T) {
			info, err := p.Info(ctx)
			if err != nil || info.ID != "kora" || info.Version != "1.0.0" {
				t.Fatalf("info: %+v, %v", info, err)
			}
			k, err := p.Knowledge(ctx, "kereta dari Nagoya ke Kariya", KnowledgeOptions{Units: 2, Sources: -1})
			if err != nil {
				t.Fatal(err)
			}
			if len(k.Units) == 0 || k.Units[0].Title != "Kereta Nagoya ke Kariya" || !strings.Contains(k.Context, "15-20 menit") || !strings.Contains(k.System, "You are Kora") {
				t.Fatalf("knowledge: %+v", k)
			}
			reply, err := Ask(ctx, p, "Dari Nagoya ke Kariya naik apa?")
			if err != nil || reply.Content != fakeAnswer || reply.Persona != "kora" {
				t.Fatalf("chat: %+v, %v", reply, err)
			}
			var got strings.Builder
			reply, err = p.Stream(ctx, []Message{{Role: "user", Content: "Nagoya ke Kariya?"}}, ChatOptions{}, func(s string) error {
				got.WriteString(s)
				return nil
			})
			if err != nil || got.String() != fakeAnswer || reply.Content != fakeAnswer {
				t.Fatalf("stream: deltas %q, reply %+v, %v", got.String(), reply, err)
			}
			tool, err := NewKnowledgeTool(ctx, p, ToolOptions{Units: 2})
			if err != nil {
				t.Fatal(err)
			}
			out, err := tool.Call(ctx, json.RawMessage(`{"query":"Nagoya Kariya"}`))
			if err != nil || !strings.Contains(out, "420 yen") || tool.Name != "kora_knowledge" || !strings.Contains(tool.Description, "Japan rail") {
				t.Fatalf("tool: %q %+v %v", out, tool, err)
			}
		})
	}

	// Sync mode and an explicit job.
	sync := NewClient("full", ClientOptions{BaseURL: ts.URL, ChatMode: ChatSync}).Persona("kora")
	if r, err := Ask(ctx, sync, "Kariya?"); err != nil || r.Content != fakeAnswer {
		t.Fatalf("sync chat: %+v, %v", r, err)
	}
	job, err := remote.Submit(ctx, []Message{{Role: "user", Content: "Kariya?"}}, ChatOptions{})
	if err != nil || job.ID == "" {
		t.Fatalf("submit: %+v, %v", job, err)
	}
	if r, err := client.Wait(ctx, job.ID); err != nil || r.Content != fakeAnswer {
		t.Fatalf("wait: %+v, %v", r, err)
	}
	// Another key cannot see the job.
	if _, err := NewClient("readonly", ClientOptions{BaseURL: ts.URL}).Job(ctx, job.ID); !IsNotFound(err) {
		t.Errorf("foreign job visible: %v", err)
	}
}

func TestScopes(t *testing.T) {
	ctx := context.Background()
	ts, _ := startService(t, trainPack(t))
	ro := NewClient("readonly", ClientOptions{BaseURL: ts.URL})
	if _, err := ro.Persona("kora").Knowledge(ctx, "Kariya", KnowledgeOptions{}); err != nil {
		t.Fatalf("knowledge scope refused: %v", err)
	}
	var apiErr *APIError
	if _, err := Ask(ctx, ro.Persona("kora"), "Kariya?"); !asAPIError(err, &apiErr) || apiErr.Status != 403 {
		t.Errorf("chat without scope: %v", err)
	}
	if _, err := ro.Download(ctx, "kora", t.TempDir()); !asAPIError(err, &apiErr) || apiErr.Status != 403 {
		t.Errorf("download without scope: %v", err)
	}
	if _, err := NewClient("nope", ClientOptions{BaseURL: ts.URL}).List(ctx); !asAPIError(err, &apiErr) || apiErr.Status != 401 {
		t.Errorf("bad key: %v", err)
	}
	if _, err := ro.Persona("fina").Knowledge(ctx, "x", KnowledgeOptions{}); !IsNotFound(err) {
		t.Errorf("persona outside the key: %v", err)
	}
}

func asAPIError(err error, target **APIError) bool {
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}

// Download fetches the pack in ranged chunks (as it must under Lambda's
// response limit), verifies it, and skips the download when it is current.
func TestDownloadThenRunLocally(t *testing.T) {
	ctx := context.Background()
	pack := trainPack(t)
	ts, _ := startService(t, pack)
	defer func(n int64) { downloadChunk = n }(downloadChunk)
	downloadChunk = 4096

	client := NewClient("full", ClientOptions{BaseURL: ts.URL})
	dir := t.TempDir()
	path, err := client.Download(ctx, "kora", dir)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(pack)
	got, _ := os.ReadFile(path)
	if string(got) != string(want) || filepath.Base(path) != "kora@1.0.0.persona" {
		t.Fatalf("downloaded %s: %d bytes, want %d", path, len(got), len(want))
	}
	if len(want) <= int(downloadChunk) {
		t.Fatalf("pack too small (%d bytes) to exercise ranged download", len(want))
	}
	st, _ := os.Stat(path)
	if _, err := client.Download(ctx, "kora", dir); err != nil {
		t.Fatal(err)
	}
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(st.ModTime()) {
		t.Error("current pack was downloaded again")
	}
	local, err := client.OpenLocal(ctx, "kora", dir, LocalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if _, err := Ask(ctx, local, "x"); err != ErrNoChatModel {
		t.Errorf("chat without model: %v", err)
	}
}
