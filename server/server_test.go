package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"go.vicoworks.com/open-persona/graph"
	"go.vicoworks.com/open-persona/memory"
)

func TestMemoryEndpointsAndAuth(t *testing.T) {
	store, err := graph.Open(filepath.Join(t.TempDir(), "m.db"), graph.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mem, err := memory.New(memory.Config{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mem.Close(); store.Close() })

	ts := httptest.NewServer((&Server{Memory: mem, APIKey: "secret"}).Handler())
	defer ts.Close()

	call := func(method, path, key, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	if code, _ := call("GET", "/healthz", "", ""); code != 200 {
		t.Errorf("healthz without key: %d", code)
	}
	for _, key := range []string{"", "wrong"} {
		if code, _ := call("POST", "/v1/memory/u1/search", key, `{"query":"x"}`); code != 401 {
			t.Errorf("key %q: got %d, want 401", key, code)
		}
	}
	if code, _ := call("POST", "/v1/memory/u1/messages", "secret", `{"messages":[{"role":"user","content":"My cat Mochi is allergic to chicken."}]}`); code != 202 {
		t.Fatalf("add: %d", code)
	}
	code, out := call("POST", "/v1/memory/u1/search", "secret", `{"query":"Mochi allergy"}`)
	if code != 200 || !strings.Contains(out["context"].(string), "allergic to chicken") {
		t.Errorf("search: %d %v", code, out)
	}
	if _, out := call("POST", "/v1/memory/u2/search", "secret", `{"query":"Mochi allergy"}`); out["context"] != "" {
		t.Errorf("scope leak: %v", out)
	}
	if code, _ := call("POST", "/v1/memory/u1/search", "secret", `not json`); code != 400 {
		t.Errorf("bad body: %d", code)
	}
	if code, _ := call("POST", "/v1/personas/nobody/prompt", "secret", `{"query":"x"}`); code != 404 {
		t.Errorf("unknown persona: %d", code)
	}
}
