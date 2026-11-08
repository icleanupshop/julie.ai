package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"julie.ai/internal/agent"
	"julie.ai/internal/knowledge"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Julie:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "etc/model.json", "Model configuration file")
	modelID := flag.String("model", "", "Model ID (defaults to the first configured model)")
	workspace := flag.String("workspace", ".", "Directory available to read-only workspace tools")
	prompt := flag.String("prompt", "", "Ask a single question and exit")
	listen := flag.String("listen", "", "Serve the HTTP API at this address (requires JULIE_AUTH_TOKEN)")
	ingest := flag.String("ingest", "", "Ingest a .md/.txt file or directory into the configured knowledge store")
	soulFile := flag.String("soul-file", "", "Optional operator-provided identity and style guidance file")
	noTools := flag.Bool("no-tools", false, "Disable workspace tools")
	maxSteps := flag.Int("max-steps", 8, "Maximum model calls per question")
	timeout := flag.Duration("timeout", 3*time.Minute, "Total timeout per question")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments; use -prompt for a single question")
	}
	if *maxSteps < 1 || *timeout <= 0 {
		return fmt.Errorf("max-steps and timeout must be positive")
	}
	if (*listen != "" && *prompt != "") || (*ingest != "" && (*listen != "" || *prompt != "")) {
		return fmt.Errorf("ingest, listen, and prompt modes cannot be combined")
	}
	config, model, err := agent.LoadConfig(*configPath, *modelID)
	if err != nil {
		return err
	}
	soul, err := agent.LoadSoul(*soulFile)
	if err != nil {
		return err
	}
	store, err := knowledge.NewStore(config.Embeddings, config.VectorStore, config.EmbeddingAPIKey)
	if err != nil {
		return err
	}
	if *ingest != "" {
		if store == nil {
			return fmt.Errorf("knowledge store is not configured")
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		count, err := store.IngestPath(ctx, *ingest)
		if err != nil {
			return err
		}
		fmt.Printf("Ingested %d knowledge passages.\n", count)
		return nil
	}
	var root *os.Root
	if !*noTools && model.ToolCalling {
		root, err = os.OpenRoot(*workspace)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer root.Close()
	}
	julie := agent.NewWithSoul(agent.NewClient(config, model), root, *maxSteps, soul)
	julie.Knowledge = store
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *listen != "" {
		token := os.Getenv("JULIE_AUTH_TOKEN")
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("JULIE_AUTH_TOKEN is required in server mode")
		}
		handler := newHTTPHandler(token, *timeout, func(requestCtx context.Context, question string) (string, error) {
			return agent.NewWithSoul(julie.Client, root, *maxSteps, soul).Ask(requestCtx, question)
		})
		return serveHTTP(ctx, *listen, handler, *timeout)
	}
	ask := func(question string) (string, error) {
		turnCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		return julie.Ask(turnCtx, question)
	}
	if *prompt != "" {
		answer, err := ask(*prompt)
		if err != nil {
			return err
		}
		fmt.Println(answer)
		return nil
	}
	fmt.Fprintln(os.Stderr, "Julie | model:", model.ID)
	fmt.Fprintln(os.Stderr, "Enter a message, /reset to clear the conversation, or /exit to quit.")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for {
		fmt.Fprint(os.Stderr, "you> ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		question := strings.TrimSpace(scanner.Text())
		switch question {
		case "":
			continue
		case "/exit":
			return nil
		case "/reset":
			julie.Reset()
			fmt.Fprintln(os.Stderr, "Conversation cleared.")
			continue
		}
		answer, err := ask(question)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(os.Stderr, "Julie:", err)
			continue
		}
		fmt.Println("Julie:", answer)
	}
}
