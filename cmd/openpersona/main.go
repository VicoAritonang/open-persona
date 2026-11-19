// Command openpersona trains and serves personas and long-term memory.
package main

import (
	"bufio"
	"bytes"
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
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"go.vicoworks.com/open-persona/graph"
	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/memory"
	"go.vicoworks.com/open-persona/persona"
	"go.vicoworks.com/open-persona/server"
	"go.vicoworks.com/open-persona/server/rpc"
)

const usage = `openpersona - persona and context engineering

Persona (train once, serve read-only):
  openpersona train   -spec spec.json -src <file|dir> -out fina.persona [-eval eval.json]
  openpersona inspect -pack fina.persona [-units]
  openpersona eval    -pack fina.persona -set eval.json [-answer] [-v]
  openpersona chat    -pack fina.persona [-q "question"] [-show] [-db mem.db -scope user1]

Memory (read-write context):
  openpersona remember -db mem.db -scope user1 -file conversation.json
  openpersona recall   -db mem.db -scope user1 -q "question" [-history] [-rerank]

Server (HTTP API, gRPC and MCP on one port; contract in api/):
  openpersona serve -addr :8080 [-db mem.db] [-personas dir] [-reflection=false]

  openpersona version

Keys are read from .env (OPENAI_API_KEY, OPENROUTER_API_KEY).
Models: OPENPERSONA_EXTRACT_MODEL (default ` + llm.DefaultExtractModel + `),
        OPENPERSONA_CHAT_MODEL    (default ` + llm.DefaultChatModel + `),
        OPENPERSONA_EMBED_MODEL   (default ` + llm.DefaultEmbedModel + `; "off" disables).
Server: OPENPERSONA_API_KEY requires "Authorization: Bearer <key>" on every request.
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	llm.LoadDotEnv()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	commands := map[string]func(context.Context, []string) error{
		"train": train, "inspect": inspect, "chat": chat,
		"remember": remember, "recall": recallCmd, "serve": serve, "eval": evalCmd,
		"version": func(context.Context, []string) error { fmt.Println(buildVersion()); return nil },
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

// embedModel returns the embedding client and its spec, or nil when
// embeddings are turned off (OPENPERSONA_EMBED_MODEL=off) or unavailable.
func embedModel() (*llm.OpenAI, string) {
	spec := os.Getenv("OPENPERSONA_EMBED_MODEL")
	if spec == "" {
		spec = llm.DefaultEmbedModel
	}
	if spec == "off" {
		return nil, ""
	}
	model, err := llm.New(spec)
	if err != nil {
		log.Printf("note: %v; running without embeddings", err)
		return nil, ""
	}
	return model, spec
}

// openPersona opens a pack and, if it carries vectors, connects the
// embedding model it was trained with.
func openPersona(path string) (*persona.Persona, error) {
	p, err := persona.Open(path)
	if err != nil {
		return nil, err
	}
	return p, configurePersona(p)
}

// configurePersona connects the embedding model a pack was trained with,
// when it has vectors and embeddings are not turned off.
func configurePersona(p *persona.Persona) error {
	if p.EmbedModel != "" && os.Getenv("OPENPERSONA_EMBED_MODEL") != "off" {
		if model, err := llm.New(p.EmbedModel); err == nil {
			p.Embedder = model
		} else {
			log.Printf("note: %v; %s runs without semantic retrieval", err, p.Spec.ID)
		}
	}
	return nil
}

// version is set at build time (-ldflags "-X main.version=v0.2.0"); a
// binary built by go install reports its module version instead.
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
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
		data, err := readFile(f)
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
	evalPath := fs.String("eval", "", "held-out question set (JSON) to score before the pack is frozen")
	fs.Parse(args)
	if *specPath == "" || *src == "" || *out == "" {
		return errors.New("train needs -spec, -src and -out")
	}
	var spec persona.Spec
	data, err := readFile(*specPath)
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
	tr := persona.Trainer{LLM: meter, Concurrency: *workers, Logf: log.Printf, TrainModel: modelSpec("OPENPERSONA_EXTRACT_MODEL", llm.DefaultExtractModel)}
	if emb, spec := embedModel(); emb != nil {
		tr.Embedder, tr.EmbedModel = emb, spec
	}
	if *evalPath != "" {
		if tr.Eval, err = readEvalSet(*evalPath); err != nil {
			return err
		}
	}
	report, err := tr.Train(ctx, spec, sources, *out)
	if err != nil {
		return err
	}
	u := meter.Usage()
	log.Printf("done in %s: %d units %v | %s: %d calls, %d prompt + %d completion tokens",
		time.Since(start).Round(time.Millisecond), report.Units, report.ByKind, model.Model, u.Calls, u.PromptTokens, u.CompletionTokens)
	return nil
}

func modelSpec(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

func readEvalSet(path string) ([]persona.EvalCase, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	var cases []persona.EvalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cases, nil
}

func inspect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	pack := fs.String("pack", "", "persona pack")
	units := fs.Bool("units", false, "print every knowledge unit")
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
	m, err := p.Manifest(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%s@%s - %s - %s (pack format %d)\n", m.Spec.ID, m.Spec.Version, p.Spec.Name, p.Spec.Role, m.Format)
	if !m.TrainedAt.IsZero() {
		fmt.Printf("trained %s with %s", m.TrainedAt.Format(time.RFC3339), orDefault(m.TrainModel, "(unknown model)"))
		if m.EmbedModel != "" {
			fmt.Printf(", embeddings %s", m.EmbedModel)
		}
		fmt.Println()
	}
	if m.Eval != nil {
		fmt.Printf("eval: %d/%d questions fully covered, %.0f%% of expected facts retrieved\n", m.Eval.Passed, m.Eval.Cases, 100*m.Eval.Coverage)
	}
	fmt.Printf("\n%s\n\nnodes:", p.Core)
	kinds := make([]string, 0, len(stats))
	for k := range stats {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Printf(" %s=%d", k, stats[graph.Kind(k)])
	}
	fmt.Println()
	if *units {
		list, err := p.Units(ctx)
		if err != nil {
			return err
		}
		for _, u := range list {
			fmt.Printf("\n[%s] %s  {%s}\n%s\n", u.Kind, u.Title, strings.Join(u.Tags, ", "), u.Body)
		}
	}
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func evalCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	pack := fs.String("pack", "", "persona pack")
	set := fs.String("set", "", "question set (JSON): [{\"q\":\"...\",\"expect\":[\"...\"]}]")
	answer := fs.Bool("answer", false, "also answer each question with the chat model and check the answers (one call per question)")
	verbose := fs.Bool("v", false, "print every question, not only failures")
	fs.Parse(args)
	if *pack == "" || *set == "" {
		return errors.New("eval needs -pack and -set")
	}
	cases, err := readEvalSet(*set)
	if err != nil {
		return err
	}
	p, err := openPersona(*pack)
	if err != nil {
		return err
	}
	defer p.Close()
	opt := persona.EvalOptions{}
	if *answer {
		if opt.Chat, err = chatModel(); err != nil {
			return err
		}
	}
	start := time.Now()
	res, err := p.Evaluate(ctx, cases, opt)
	if err != nil {
		return err
	}
	for _, r := range res.Results {
		failed := len(r.Missing) > 0 || len(r.AnswerMissing) > 0
		if !failed && !*verbose {
			continue
		}
		mark := "ok  "
		if failed {
			mark = "FAIL"
		}
		fmt.Printf("%s %s\n     top: %s\n", mark, r.Q, strings.Join(r.TopUnits, " | "))
		if len(r.Missing) > 0 {
			fmt.Printf("     not retrieved: %s\n", strings.Join(r.Missing, ", "))
		}
		if len(r.AnswerMissing) > 0 {
			fmt.Printf("     not in answer: %s\n", strings.Join(r.AnswerMissing, ", "))
		}
		if *verbose && r.Answer != "" {
			fmt.Printf("     answer: %s\n", strings.ReplaceAll(r.Answer, "\n", "\n             "))
		}
	}
	fmt.Printf("\nretrieval: %d/%d questions fully covered, %.0f%% of expected facts in context\n", res.Passed, res.Cases, 100*res.Coverage)
	if res.Answered {
		fmt.Printf("answers:   %d/%d fully correct, %.0f%% of expected facts in answers (%d calls, %d prompt + %d completion tokens)\n",
			res.AnswerPassed, res.Cases, 100*res.AnswerCoverage, res.Usage.Calls, res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
	log.Printf("(%s)", time.Since(start).Round(time.Millisecond))
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
	if emb, _ := embedModel(); emb != nil {
		cfg.Embedder = emb
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
	p, err := openPersona(*pack)
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
	data, err := readFile(*file)
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
	addr := fs.String("addr", ":8080", "listen address (HTTP/1.1, and cleartext HTTP/2 for gRPC)")
	db := fs.String("db", "memory.db", "memory database (\"\" disables memory)")
	dir := fs.String("personas", "", "directory of *.persona packs to serve")
	reflection := fs.Bool("reflection", true, "serve gRPC server reflection (for grpcurl, Postman)")
	fs.Parse(args)

	srv := &server.Server{Personas: make(map[string]*persona.Persona), Version: buildVersion(), Jobs: &server.MemoryJobs{}}
	srv.AllowOrigins = splitList(os.Getenv("OPENPERSONA_CORS_ORIGINS"))
	srv.APIKey = os.Getenv("OPENPERSONA_API_KEY")
	if srv.APIKey == "" {
		log.Printf("warning: OPENPERSONA_API_KEY is not set; the API accepts unauthenticated requests")
	}
	if model, err := chatModel(); err == nil {
		srv.ChatModel = model
	} else {
		log.Printf("note: %v; persona chat is disabled", err)
	}
	if *dir != "" {
		packs, err := filepath.Glob(filepath.Join(*dir, "*"+persona.PackExt))
		if err != nil {
			return err
		}
		for _, path := range packs {
			p, err := openPersona(path)
			if err != nil {
				return err
			}
			defer p.Close()
			srv.Personas[p.Spec.ID] = p
			log.Printf("loaded persona %s@%s (%s)", p.Spec.ID, p.Spec.Version, p.Spec.Role)
		}
	}
	if *db != "" {
		eng, store, _, err := openMemory(*db)
		if err != nil {
			return err
		}
		defer store.Close()
		defer eng.Close()
		srv.Memory = eng
	}
	rpc.Mount(srv, rpc.Options{Reflection: *reflection})

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // gRPC without TLS (h2c); put TLS in front in production
	// No WriteTimeout: streamed answers can run for minutes.
	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), Protocols: protocols,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	log.Printf("openpersona %s listening on %s (HTTP API, gRPC, MCP)", buildVersion(), *addr)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// readFile reads a file without the UTF-8 byte order mark that Windows
// editors and PowerShell often write, which JSON decoding rejects.
func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	return bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), err
}
