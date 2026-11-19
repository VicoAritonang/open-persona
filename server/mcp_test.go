package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/persona"
)

type fakeLLM func(req llm.Request) string

func (f fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	return llm.Response{Content: f(req)}, nil
}

func testPersona(t *testing.T) *persona.Persona {
	t.Helper()
	model := fakeLLM(func(req llm.Request) string {
		switch system := req.Messages[0].Content; {
		case strings.HasPrefix(system, "You are writing the core identity"):
			return `{"identity":"You are Kora.","principles":[],"style":""}`
		case strings.HasPrefix(system, "You are compiling"):
			return `{"units":[{"kind":"reference","title":"Haramain train Makkah to Madinah","body":"About 2 hours 15 minutes; economy about SAR 162 before VAT.","tags":["Haramain"]}]}`
		}
		return "Take the Haramain train."
	})
	out := filepath.Join(t.TempDir(), "kora.persona")
	spec := persona.Spec{ID: "kora", Version: "1.0.0", Name: "Kora", Role: "Travel planner", Expertise: []string{"Saudi rail"}}
	if _, err := (&persona.Trainer{LLM: model}).Train(context.Background(), spec, []persona.Source{{Name: "saudi", Text: "# Rail\n\nHaramain train Makkah to Madinah.\n"}}, out); err != nil {
		t.Fatal(err)
	}
	p, err := persona.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestMCP(t *testing.T) {
	p := testPersona(t)
	srv := &Server{Personas: map[string]*persona.Persona{"kora": p}, ChatModel: fakeLLM(func(llm.Request) string { return "Take the Haramain train." }),
		Auth: AnyOf{StaticKey("op"), keyFor("ro", []string{ScopeKnowledge})}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	rpc := func(key, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out map[string]any
		json.Unmarshal(data, &out)
		return resp.StatusCode, out
	}

	_, out := rpc("op", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || res["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize: %v", out)
	}
	if code, _ := rpc("op", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Errorf("notification: HTTP %d", code)
	}

	_, out = rpc("op", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	names := toolNames(out)
	if names != "kora_knowledge,kora_ask" {
		t.Fatalf("tools for full key: %s", names)
	}
	if _, out = rpc("ro", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); toolNames(out) != "kora_knowledge" {
		t.Errorf("tools for knowledge-only key: %s", toolNames(out))
	}

	_, out = rpc("ro", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"kora_knowledge","arguments":{"query":"Haramain train Madinah"}}}`)
	text := toolResult(t, out)
	if !strings.Contains(text, "SAR 162") {
		t.Errorf("knowledge tool: %s", text)
	}
	_, out = rpc("ro", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"kora_ask","arguments":{"question":"How do I get to Madinah?"}}}`)
	if r := out["result"].(map[string]any); r["isError"] != true {
		t.Errorf("ask without chat scope: %v", out)
	}
	_, out = rpc("op", `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"kora_ask","arguments":{"question":"How do I get to Madinah?"}}}`)
	if text := toolResult(t, out); text != "Take the Haramain train." {
		t.Errorf("ask: %s", text)
	}
	_, out = rpc("op", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"fina_knowledge","arguments":{"query":"x"}}}`)
	if out["error"] == nil {
		t.Errorf("unknown tool: %v", out)
	}
	if code, _ := rpc("bad", `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`); code != 401 {
		t.Errorf("bad key: HTTP %d", code)
	}
}

func keyFor(token string, scopes []string) Authenticator {
	return authFunc(func(tok string) (*Principal, error) {
		if tok != token {
			return nil, ErrUnauthorized
		}
		return &Principal{KeyID: token, Personas: []string{"*"}, Scopes: scopes}, nil
	})
}

type authFunc func(string) (*Principal, error)

func (f authFunc) Authenticate(_ context.Context, token string) (*Principal, error) {
	return f(token)
}

func toolNames(out map[string]any) string {
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	return strings.Join(names, ",")
}

func toolResult(t *testing.T, out map[string]any) string {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok || res["isError"] == true {
		t.Fatalf("tool call failed: %v", out)
	}
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestCORS(t *testing.T) {
	ts := httptest.NewServer((&Server{APIKey: "k", AllowOrigins: []string{"https://app.example.com"}}).Handler())
	defer ts.Close()
	pre := func(origin string) *http.Response {
		req, _ := http.NewRequest("OPTIONS", ts.URL+"/v1/personas/kora/knowledge", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := pre("https://app.example.com"); r.StatusCode != 204 || r.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		!strings.Contains(r.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("allowed preflight: %d %v", r.StatusCode, r.Header)
	}
	if r := pre("https://evil.example"); r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign origin allowed: %v", r.Header)
	}
}
