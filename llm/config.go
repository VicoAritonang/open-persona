package llm

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Providers maps a provider name to its base URL and the environment
// variable that holds its API key. Add entries to support more providers.
var Providers = map[string]struct{ BaseURL, KeyEnv string }{
	"openai":     {"https://api.openai.com/v1", "OPENAI_API_KEY"},
	"openrouter": {"https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"},
}

// Defaults for the two jobs a model does in this stack.
const (
	DefaultExtractModel = "openai:gpt-4o-mini"             // high-volume structured extraction
	DefaultChatModel    = "openrouter:typesafe/jev-router" // user-facing answers
	DefaultEmbedModel   = "openai:text-embedding-3-small"  // semantic retrieval
)

// New builds a client from a "provider:model" spec such as
// "openai:gpt-4o-mini" or "openrouter:typesafe/jev-router".
func New(spec string) (*OpenAI, error) {
	provider, model, ok := strings.Cut(spec, ":")
	if !ok || model == "" {
		return nil, fmt.Errorf("llm: model spec %q must look like provider:model", spec)
	}
	p, known := Providers[provider]
	if !known {
		return nil, fmt.Errorf("llm: unknown provider %q", provider)
	}
	key := os.Getenv(p.KeyEnv)
	if key == "" {
		return nil, fmt.Errorf("llm: %s is not set", p.KeyEnv)
	}
	return &OpenAI{BaseURL: p.BaseURL, APIKey: key, Model: model}, nil
}

// FromEnv builds a client from the spec in environment variable name,
// falling back to def when it is unset.
func FromEnv(name, def string) (*OpenAI, error) {
	spec := os.Getenv(name)
	if spec == "" {
		spec = def
	}
	return New(spec)
}

// LoadDotEnv reads KEY=VALUE lines from the first of the given files that
// exists (default: .env, then .env.txt) into the process environment.
// Variables that are already set are left alone.
func LoadDotEnv(files ...string) {
	if len(files) == 0 {
		files = []string{".env", ".env.txt"}
	}
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), string(rune(0xFEFF))))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
			if os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
		f.Close()
		return
	}
}
