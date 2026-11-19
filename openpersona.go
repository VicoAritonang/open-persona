// Package openpersona is the client library for OpenPersona personas, such
// as Kora, the travel agent. Import it with an explicit name, since the
// path's last element is not an identifier:
//
//	import openpersona "go.vicoworks.com/open-persona"
//
// One interface, Persona, works two ways:
//
//   - Hosted: the persona runs on an OpenPersona server and is called over
//     HTTPS (NewClient, then Client.Persona). Nothing to download; answers
//     come back synchronously, as an async job, or streamed. This package
//     depends on the standard library only.
//   - Local: the persona's pack runs inside your own process (package
//     local). Knowledge retrieval is then a local lookup with no network
//     call except the query embedding; answers use your own model keys.
//
// Either way, KnowledgeTool turns a persona into a tool your agent's model
// can call, once per sub-question:
//
//	client := openpersona.NewClient(os.Getenv("OPENPERSONA_API_KEY"), openpersona.ClientOptions{})
//	kora := client.Persona("kora")                          // hosted
//	// or: kora, _ := local.Open("kora.persona", local.Options{})   // in process
//
//	k, _ := kora.Knowledge(ctx, "kereta Nagoya ke Kariya", openpersona.KnowledgeOptions{Units: 6})
//	reply, _ := kora.Chat(ctx, []openpersona.Message{{Role: "user", Content: "..."}}, openpersona.ChatOptions{})
//	tool, _ := openpersona.NewKnowledgeTool(ctx, kora, openpersona.ToolOptions{})
//
// Import only what you use: the engines (persona, memory, graph, recall),
// the HTTP server (server) and the gRPC services (server/rpc) are separate
// packages, so a hosted-only client does not compile the storage engine.
package openpersona

import (
	"context"
	"fmt"
	"strings"

	"go.vicoworks.com/open-persona/llm"
)

// Message is one chat message ("user" or "assistant"; the persona supplies
// the system message).
type Message = llm.Message

// Usage counts model tokens.
type Usage = llm.Usage

// Info describes a persona.
type Info struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	Description string   `json:"description,omitempty"`
	Expertise   []string `json:"expertise,omitempty"`
	Tone        string   `json:"tone,omitempty"`
	Language    string   `json:"language,omitempty"`
	Boundaries  []string `json:"boundaries,omitempty"`
	Author      string   `json:"author,omitempty"`
}

// KnowledgeToolDescription describes this persona's knowledge search to an
// agent's model, for a tool definition (MCP, OpenAI or Anthropic tools).
func (i Info) KnowledgeToolDescription() string {
	d := fmt.Sprintf("Search the working knowledge of %s (%s).", i.Name, i.Role)
	if len(i.Expertise) > 0 {
		d += " Covers: " + strings.Join(i.Expertise, "; ") + "."
	}
	return d + " Ask one focused question per call (one place, one rule, one route); call it several times for a multi-part task. Returns knowledge units, not an answer."
}

// Unit is one piece of a persona's knowledge.
type Unit struct {
	Kind  string  `json:"kind"` // rule, procedure, reference, concept, principle, example, source
	Title string  `json:"title"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}

// Knowledge is what a persona knows about one query.
type Knowledge struct {
	Persona string `json:"persona"`
	Version string `json:"version"`
	// System is the persona's full system prompt for this query: identity,
	// boundaries, this knowledge, and grounding rules. Use it when your own
	// model should answer as the persona.
	System string `json:"system"`
	// Context is the knowledge alone, as text: the right shape for a tool
	// result returned to an agent.
	Context string `json:"context"`
	Units   []Unit `json:"units"`
	Sources []Unit `json:"sources"`
}

// KnowledgeOptions tunes retrieval. Zero values use the pack's defaults.
type KnowledgeOptions struct {
	Units   int // knowledge units (at most 64)
	Sources int // verbatim source excerpts (at most 16); -1 for none
}

// ChatOptions tunes an answer.
type ChatOptions struct {
	// Context is extra material for the system prompt: what your app knows
	// about the user or the task (dates, passport, budget, live flight
	// results).
	Context   string
	MaxTokens int
}

// Reply is a persona's answer.
type Reply struct {
	Persona string `json:"persona"`
	Version string `json:"version"`
	Content string `json:"reply"`
	Units   []Unit `json:"units"` // the knowledge the answer was grounded in
	Usage   Usage  `json:"usage"`
}

// Persona is a persona, hosted or local.
type Persona interface {
	// ID is the persona id, such as "kora".
	ID() string
	// Info describes the persona.
	Info(ctx context.Context) (*Info, error)
	// Knowledge retrieves what the persona knows about query. It makes no
	// chat-model call.
	Knowledge(ctx context.Context, query string, opt KnowledgeOptions) (*Knowledge, error)
	// Chat answers the conversation's last user message as the persona.
	Chat(ctx context.Context, messages []Message, opt ChatOptions) (*Reply, error)
	// Stream is Chat with the answer delivered as it is generated.
	Stream(ctx context.Context, messages []Message, opt ChatOptions, onDelta func(text string) error) (*Reply, error)
	// Close releases the persona (a local pack file; nothing for hosted).
	Close() error
}

// Ask is Chat with a single question.
func Ask(ctx context.Context, p Persona, question string) (*Reply, error) {
	return p.Chat(ctx, []Message{{Role: "user", Content: question}}, ChatOptions{})
}
