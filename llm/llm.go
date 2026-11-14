// Package llm is a minimal client for OpenAI-compatible chat and embedding
// APIs (OpenAI, OpenRouter, Ollama, vLLM, ...), plus helpers for JSON output
// and usage metering.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// Request is a chat completion request.
type Request struct {
	Messages    []Message
	JSON        bool     // ask for a JSON object
	Temperature *float64 // nil = provider default
	MaxTokens   int      // 0 = provider default
}

// Usage counts tokens for one or more calls.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	Calls            int64 `json:"calls"`
}

// Response is a chat completion result.
type Response struct {
	Content string
	Model   string
	Usage   Usage
}

// Client completes chats. Implement it to plug in any model.
type Client interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Embedder turns texts into vectors. It is optional everywhere.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Temp returns a pointer to t, for Request.Temperature.
func Temp(t float64) *float64 { return &t }

// OpenAI talks to any OpenAI-compatible endpoint.
type OpenAI struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Model   string
	HTTP    *http.Client
	// MaxRetries is the number of retries on 429/5xx/network errors (default 3).
	MaxRetries int
}

var defaultHTTP = &http.Client{Timeout: 120 * time.Second}

func (c *OpenAI) post(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	retries := c.MaxRetries
	if retries == 0 {
		retries = 3
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * 500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("llm: %s: HTTP %d: %s", c.Model, resp.StatusCode, snippet(data))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("llm: %s: HTTP %d: %s", c.Model, resp.StatusCode, snippet(data))
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("llm: %s: bad response: %w", c.Model, err)
		}
		return nil
	}
	return lastErr
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

// Complete implements Client.
func (c *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	body := map[string]any{"model": c.Model, "messages": req.Messages}
	if req.JSON {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.post(ctx, "/chat/completions", body, &out); err != nil {
		return Response{}, err
	}
	if out.Error != nil {
		return Response{}, fmt.Errorf("llm: %s: %s", c.Model, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("llm: %s: empty response", c.Model)
	}
	return Response{
		Content: out.Choices[0].Message.Content,
		Model:   out.Model,
		Usage:   Usage{PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens, Calls: 1},
	}, nil
}

// Embed implements Embedder using the /embeddings endpoint.
func (c *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/embeddings", map[string]any{"model": c.Model, "input": texts}, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("llm: %s: got %d embeddings for %d texts", c.Model, len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("llm: %s: embedding index %d out of range", c.Model, d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

// Meter wraps a Client and adds up the tokens it spends.
type Meter struct {
	Client                    Client
	prompt, completion, calls atomic.Int64
}

// NewMeter wraps c.
func NewMeter(c Client) *Meter { return &Meter{Client: c} }

// Complete implements Client.
func (m *Meter) Complete(ctx context.Context, req Request) (Response, error) {
	resp, err := m.Client.Complete(ctx, req)
	if err == nil {
		m.prompt.Add(resp.Usage.PromptTokens)
		m.completion.Add(resp.Usage.CompletionTokens)
		m.calls.Add(1)
	}
	return resp, err
}

// Usage returns the running total.
func (m *Meter) Usage() Usage {
	return Usage{PromptTokens: m.prompt.Load(), CompletionTokens: m.completion.Load(), Calls: m.calls.Load()}
}

// JSON runs req in JSON mode and decodes the reply into out. Models
// sometimes wrap JSON in prose or code fences, so the outermost object is
// extracted first; one retry is made if decoding still fails.
func JSON(ctx context.Context, c Client, req Request, out any) error {
	req.JSON = true
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := c.Complete(ctx, req)
		if err != nil {
			return err
		}
		obj, ok := extractObject(resp.Content)
		if !ok {
			lastErr = errors.New("llm: reply contains no JSON object")
			continue
		}
		if err := json.Unmarshal([]byte(obj), out); err != nil {
			lastErr = fmt.Errorf("llm: decode JSON reply: %w", err)
			continue
		}
		return nil
	}
	return lastErr
}

// extractObject returns the outermost {...} in s, respecting strings.
func extractObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case inStr:
			if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
		case ch == '"':
			inStr = true
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}
