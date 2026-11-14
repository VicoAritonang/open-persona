// Command openpersona trains and serves personas and long-term memory.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openpersona/openpersona/graph"
	"github.com/openpersona/openpersona/llm"
	"github.com/openpersona/openpersona/memory"
	"github.com/openpersona/openpersona/persona"
	"github.com/openpersona/openpersona/server"
)

const usage = `openpersona - persona and context engineering

Persona (train once, serve read-only):
  openpersona train   -spec spec.json -src <file|dir> -out finance.persona
  openpersona inspect -pack finance.persona
  openpersona chat    -pack finance.persona [-q "question"] [-show] [-db mem.db -scope user1]

Memory (read-write context):
  openpersona remember -db mem.db -scope user1 -file conversation.json
  openpersona recall   -db mem.db -scope user1 -q "question" [-history] [-rerank]

Server:
  openpersona serve -addr :8080 [-db mem.db] [-personas dir]

Keys are read from .env (OPENAI_API_KEY, OPENROUTER_API_KEY).
Models: OPENPERSONA_EXTRACT_MODEL (default ` + llm.DefaultExtractModel + `),
        OPENPERSONA_CHAT_MODEL    (default ` + llm.DefaultChatModel + `).
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	llm.LoadDotEnv()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	commands := map[string]func(context.Context, []string) error{
		"train": train, "inspect": inspect, "chat": chat,
		"remember": remember, "recall": recallCmd, "serve": serve,
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Print(usage)
		os.Exit(2)
	}
	if err := cmd(ctx, os.Args[2:]); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func extractModel() (*llm.OpenAI, error) {
	return llm.FromEnv("OPENPERSONA_EXTRACT_MODEL", llm.DefaultExtractModel)
}

func chatModel() (*llm.OpenAI, error) {
	return llm.FromEnv("OPENPERSONA_CHAT_MODEL", llm.DefaultChatModel)
}

// readSources loads one file, or every .md/.txt file under a directory.
func readSources(path string) ([]persona.Source, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var files []string
	if !info.IsDir() {
		files = []string{path}
	} else {
		err = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ext := strings.ToLower(filepath.Ext(p)); !d.IsDir() && (ext == ".md" || ext == ".txt") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
	}
	var out []persona.Source
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		out = append(out, persona.Source{Name: strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)), Text: string(data)})
	}
	return out, nil
}

func train(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("train", flag.ExitOnError)
	specPath := fs.String("spec", "", "persona spec (JSON)")
	src := fs.String("src", "", "training material: a file, or a directory of .md/.txt files")
	out := fs.String("out", "", "output pack file")
	workers := fs.Int("workers", 6, "parallel extraction calls")
	fs.Parse(args)
	if *specPath == "" || *src == "" || *out == "" {
		return errors.New("train needs -spec, -src and -out")
	}
	var spec persona.Spec
	data, err := os.ReadFile(*specPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("%s: %w", *specPath, err)
	}
	sources, err := readSources(*src)
	if err != nil {
		return err
	}
	model, err := extractModel()
	if err != nil {
		return err
	}
	meter := llm.NewMeter(model)
	start := time.Now()
	tr := persona.Trainer{LLM: meter, Concurrency: *workers, Logf: log.Printf}
	report, err := tr.Train(ctx, spec, sources, *out)
	if err != nil {
		return err
	}
	u := meter.Usage()
	log.Printf("done in %s: %d units %v | %s: %d calls, %d prompt + %d completion tokens",
		time.Since(start).Round(time.Millisecond), report.Units, report.ByKind, model.Model, u.Calls, u.PromptTokens, u.CompletionTokens)
	return nil
}

func inspect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	pack := fs.String("pack", "", "persona pack")
	fs.Parse(args)
	p, err := persona.Open(*pack)
	if err != nil {
		return err
	}
	defer p.Close()
	stats, err := p.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%s - %s\n\n%s\n\nnodes:", p.Spec.Name, p.Spec.Role, p.Core)
	kinds := make([]string, 0, len(stats))
	for k := range stats {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Printf(" %s=%d", k, stats[graph.Kind(k)])
	}
	fmt.Println()
	return nil
}

// openMemory opens a memory engine on db. Extraction is enabled when the
// extraction model is configured; otherwise the engine runs raw-only.
func openMemory(db string) (*memory.Engine, *graph.Store, *llm.Meter, error) {
	store, err := graph.Open(db, graph.Options{})
	if err != nil {
		return nil, nil, nil, err
	}
	cfg := memory.Config{Store: store, OnError: func(err error) { log.Printf("warning: %v", err) }}
	var meter *llm.Meter
	if model, err := extractModel(); err == nil {
		meter = llm.NewMeter(model)
		cfg.Extractor, cfg.Reranker = meter, meter
	} else {
		log.Printf("note: %v; running memory in raw-only mode", err)
	}
	eng, err := memory.New(cfg)
	if err != nil {
		store.Close()
		return nil, nil, nil, err
	}
	return eng, store, meter, nil
}

func chat(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	pack := fs.String("pack", "", "persona pack")
	question := fs.String("q", "", "ask one question and exit (default: interactive)")
	show := fs.Bool("show", false, "print the system prompt used for each answer")
	db := fs.String("db", "", "memory database, to remember this user across chats")
	scope := fs.String("scope", "default", "memory scope (user id)")
	fs.Parse(args)
	p, err := persona.Open(*pack)
	if err != nil {
		return err
	}
	defer p.Close()
	model, err := chatModel()
	if err != nil {
		return err
	}
	var mem *memory.Engine
	if *db != "" {
		eng, store, _, err := openMemory(*db)
		if err != nil {
			return err
		}
		defer store.Close()
		defer eng.Close()
		mem = eng
	}

	var history []llm.Message
	ask := func(q string) error {
		history = append(history, llm.Message{Role: "user", Content: q})
		opt := persona.ChatOptions{}
		if mem != nil {
			res, err := mem.Search(ctx, *scope, q, memory.SearchOptions{})
			if err != nil {
				return err
			}
			opt.Context = res.Context(1500)
		}
		reply, err := p.Chat(ctx, model, history, opt)
		if err != nil {
			history = history[:len(history)-1]
			return err
		}
		if *show {
			fmt.Printf("--- system prompt ---\n%s\n---------------------\n", reply.System)
		}
		fmt.Printf("%s\n", reply.Content)
		history = append(history, llm.Message{Role: "assistant", Content: reply.Content})
		if mem != nil {
			return mem.Add(ctx, *scope, []memory.Message{{Role: "user", Content: q}, {Role: "assistant", Content: reply.Content}})
		}
		return nil
	}
	if *question != "" {
		return ask(*question)
	}
	fmt.Printf("%s (%s). Empty line or Ctrl+C to quit.\n", p.Spec.Name, p.Spec.Role)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for fmt.Print("\n> "); in.Scan(); fmt.Print("\n> ") {
		q := strings.TrimSpace(in.Text())
		if q == "" {
			break
		}
		if err := ask(q); err != nil {
			log.Printf("error: %v", err)
		}
	}
	return nil
}

func remember(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("remember", flag.ExitOnError)
	db := fs.String("db", "memory.db", "memory database")
	scope := fs.String("scope", "default", "memory scope (user id)")
	file := fs.String("file", "", `JSON file: [{"role":"user","content":"...","time":"2026-01-02T15:04:05Z"}, ...]`)
	fs.Parse(args)
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var msgs []memory.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}
	eng, store, meter, err := openMemory(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	defer eng.Close()
	start := time.Now()
	if err := eng.Add(ctx, *scope, msgs); err != nil {
		return err
	}
	if err := eng.Flush(ctx); err != nil {
		return err
	}
	counts, err := store.Count(ctx, *scope)
	if err != nil {
		return err
	}
	log.Printf("stored %d messages in %s; scope %q now holds %v", len(msgs), time.Since(start).Round(time.Millisecond), *scope, counts)
	if meter != nil {
		u := meter.Usage()
		log.Printf("extraction: %d calls, %d prompt + %d completion tokens", u.Calls, u.PromptTokens, u.CompletionTokens)
	}
	return nil
}

func recallCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recall", flag.ExitOnError)
	db := fs.String("db", "memory.db", "memory database")
	scope := fs.String("scope", "default", "memory scope (user id)")
	q := fs.String("q", "", "query")
	history := fs.Bool("history", false, "include facts that are no longer true")
	rerank := fs.Bool("rerank", false, "rerank with the extraction model (one LLM call)")
	fs.Parse(args)
	eng, store, _, err := openMemory(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	defer eng.Close()
	start := time.Now()
	res, err := eng.Search(ctx, *scope, *q, memory.SearchOptions{IncludeHistory: *history, Rerank: *rerank})
	if err != nil {
		return err
	}
	fmt.Println(res.Context(0))
	log.Printf("(%d facts, %d messages, %s)", len(res.Facts), len(res.Episodes), time.Since(start).Round(time.Microsecond))
	return nil
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	db := fs.String("db", "memory.db", "memory database")
	dir := fs.String("personas", "", "directory of *.persona packs to serve")
	fs.Parse(args)

	srv := &server.Server{Personas: make(map[string]*persona.Persona)}
	eng, store, _, err := openMemory(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	defer eng.Close()
	srv.Memory = eng

	if *dir != "" {
		packs, err := filepath.Glob(filepath.Join(*dir, "*.persona"))
		if err != nil {
			return err
		}
		for _, path := range packs {
			p, err := persona.Open(path)
			if err != nil {
				return err
			}
			defer p.Close()
			name := strings.TrimSuffix(filepath.Base(path), ".persona")
			srv.Personas[name] = p
			log.Printf("loaded persona %q (%s)", name, p.Spec.Role)
		}
	}
	if model, err := chatModel(); err == nil {
		srv.Chat = model
	} else {
		log.Printf("note: %v; persona chat endpoint is disabled", err)
	}

	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	log.Printf("listening on %s", *addr)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
