package openpersona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ToolOptions configures a knowledge tool.
type ToolOptions struct {
	// Name of the tool (default "<id>_knowledge", e.g. "kora_knowledge").
	Name string
	// Units per call (default: the pack's own budget, which is sized for
	// it; Kora's is 20).
	Units int
}

// Tool exposes a persona's knowledge as a function your agent's model can
// call. Give its definition to the model next to your own tools (flights,
// hotels, search) and route calls with its name to Call.
type Tool struct {
	Name        string
	Description string
	// Parameters is the JSON Schema of the arguments: {"query": string}.
	Parameters json.RawMessage

	p     Persona
	units int
}

var toolSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"One focused question, in the user's language"}},"required":["query"]}`)

// NewKnowledgeTool builds the knowledge tool of p. It asks p for its
// description once (a request, for a hosted persona).
func NewKnowledgeTool(ctx context.Context, p Persona, opt ToolOptions) (*Tool, error) {
	info, err := p.Info(ctx)
	if err != nil {
		return nil, err
	}
	name := opt.Name
	if name == "" {
		name = strings.ReplaceAll(p.ID(), "-", "_") + "_knowledge"
	}
	return &Tool{Name: name, Description: info.KnowledgeToolDescription(), Parameters: toolSchema, p: p, units: opt.Units}, nil
}

// Call runs the tool with the model's JSON arguments and returns the text
// to hand back as the tool result.
func (t *Tool) Call(ctx context.Context, arguments json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(arguments, &a); err != nil {
		return "", fmt.Errorf("%s: bad arguments: %w", t.Name, err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", errors.New(t.Name + ": query is empty")
	}
	k, err := t.p.Knowledge(ctx, a.Query, KnowledgeOptions{Units: t.units, Sources: -1})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(k.Context) == "" {
		return "Nothing in this persona's knowledge matches the question. Use another source (web search) or ask the user.", nil
	}
	return strings.TrimSpace(k.Context), nil
}

// OpenAI is the tool's definition for OpenAI-compatible chat APIs
// (OpenAI, OpenRouter, vLLM, ...): {"type":"function","function":{...}}.
func (t *Tool) OpenAI() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": t.Name, "description": t.Description, "parameters": t.Parameters,
	}}
}

// Anthropic is the tool's definition for the Anthropic Messages API.
func (t *Tool) Anthropic() map[string]any {
	return map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Parameters}
}
