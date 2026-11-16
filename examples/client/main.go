// Command client shows the openpersona library from a customer's backend:
// the same calls against a hosted persona and against a downloaded pack.
//
//	go run ./examples/client -url https://api.example.com -key opk_... -q "Kereta Nagoya ke Kariya?"
//	go run ./examples/client ... -mode chat      # async job, then the answer
//	go run ./examples/client ... -mode stream    # streamed answer (point -stream-url at the EC2 host)
//	go run ./examples/client ... -mode local     # download the pack, answer locally with your own model key
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	openpersona "go.vicoworks.com/open-persona"
	"go.vicoworks.com/open-persona/llm"
)

func main() {
	url := flag.String("url", os.Getenv("OPENPERSONA_URL"), "service URL")
	streamURL := flag.String("stream-url", "", "streaming host, if different")
	key := flag.String("key", os.Getenv("OPENPERSONA_API_KEY"), "API key")
	id := flag.String("persona", "kora", "persona id")
	q := flag.String("q", "Dari Nagoya ke Kariya naik apa dan berapa lama?", "question")
	mode := flag.String("mode", "knowledge", "knowledge, chat, stream or local")
	flag.Parse()
	llm.LoadDotEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := openpersona.NewClient(*key, openpersona.ClientOptions{BaseURL: *url, StreamURL: *streamURL})
	var p openpersona.Persona = client.Persona(*id)
	if *mode == "local" {
		chat, err := llm.FromEnv("OPENPERSONA_CHAT_MODEL", llm.DefaultChatModel)
		if err != nil {
			log.Fatal(err)
		}
		local, err := client.OpenLocal(ctx, *id, "packs", openpersona.LocalOptions{Chat: chat})
		if err != nil {
			log.Fatal(err)
		}
		defer local.Close()
		p = local
	}

	start := time.Now()
	switch *mode {
	case "knowledge":
		k, err := p.Knowledge(ctx, *q, openpersona.KnowledgeOptions{Units: 6, Sources: -1})
		if err != nil {
			log.Fatal(err)
		}
		for _, u := range k.Units {
			fmt.Printf("[%s] %s\n", u.Kind, u.Title)
		}
	case "chat", "local":
		r, err := openpersona.Ask(ctx, p, *q)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(r.Content)
		fmt.Printf("\n(%s@%s, %d+%d tokens)\n", r.Persona, r.Version, r.Usage.PromptTokens, r.Usage.CompletionTokens)
	case "stream":
		r, err := p.Stream(ctx, []openpersona.Message{{Role: "user", Content: *q}}, openpersona.ChatOptions{}, func(s string) error {
			fmt.Print(s)
			return nil
		})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("\n\n(%s@%s, %d+%d tokens)\n", r.Persona, r.Version, r.Usage.PromptTokens, r.Usage.CompletionTokens)
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
	fmt.Fprintf(os.Stderr, "%s in %s\n", *mode, time.Since(start).Round(time.Millisecond))
}
