package llm

import (
	"strings"
	"testing"
)

func TestReadSSE(t *testing.T) {
	body := `data: {"model":"gpt-4o-mini","choices":[{"delta":{"role":"assistant"}}]}

: keep-alive
data: {"choices":[{"delta":{"content":"Halo"}}]}

data: {"choices":[{"delta":{"content":", Kora"}}]}

data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3}}

data: [DONE]
`
	var got []string
	resp, err := readSSE(strings.NewReader(body), "m", func(s string) error { got = append(got, s); return nil })
	if err != nil || resp.Content != "Halo, Kora" || strings.Join(got, "|") != "Halo|, Kora" || resp.Usage.PromptTokens != 12 || resp.Model != "gpt-4o-mini" {
		t.Fatalf("%+v %q %v", resp, got, err)
	}
	if _, err := readSSE(strings.NewReader(`data: {"error":{"message":"rate limited"}}`+"\n"), "m", nil); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error event: %v", err)
	}
}
