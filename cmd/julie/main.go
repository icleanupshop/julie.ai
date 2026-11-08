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
	"julie.ai/internal/sessions"
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
	listen := flag.String("listen", "", "Serve the HTTP API at this address")
	mcpURL := flag.String("mcp-url", os.Getenv("JULIE_MCP_URL"), "MCP Streamable HTTP endpoint")
	sessionsDB := flag.String("sessions-db", "julie-sessions.db", "Path to the persistent API sessions database")
	ingest := flag.String("ingest", "", "Ingest a .md/.txt file or directory into the configured knowledge store")
	soulFile := flag.String("soul-file", "", "Optional operator-provided identity and style guidance file")
	noTools := flag.Bool("no-tools", false, "Disable model-callable tools")
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var mcpTools *agent.MCPToolClient
	if !*noTools && model.ToolCalling && *mcpURL != "" {
		token := os.Getenv("JULIE_MCP_TOKEN")
		if token == "" {
			return fmt.Errorf("JULIE_MCP_TOKEN is required when JULIE_MCP_URL is set")
		}
		connectCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		mcpTools, err = connectMCP(connectCtx, *mcpURL, token)
		cancel()
		if err != nil {
			return err
		}
		defer mcpTools.Close()
	}
	var root *os.Root
	if !*noTools && model.ToolCalling && *mcpURL == "" {
		root, err = os.OpenRoot(*workspace)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer root.Close()
	}
	julie := agent.NewWithSoul(agent.NewClient(config, model), root, *maxSteps, soul)
	julie.Knowledge = store
	julie.MCPTools = mcpTools
	if *listen != "" {
		sessionStore, err := sessions.Open(ctx, *sessionsDB)
		if err != nil {
			return err
		}
		defer sessionStore.Close()
		var memoryStore *knowledge.Store
		if store != nil {
			memoryVectors := config.VectorStore
			collection := memoryVectors.Collection
			if collection == "" {
				collection = "julie_knowledge"
			}
			memoryVectors.Collection = collection + "_conversations"
			memoryStore, err = knowledge.NewStore(config.Embeddings, memoryVectors, config.EmbeddingAPIKey)
			if err != nil {
				return err
			}
		}
		handler := newHTTPHandler(*timeout, sessionStore, func(requestCtx context.Context, sessionID string, history []sessions.Message, question string) (string, error) {
			requestAgent := agent.NewWithSoul(julie.Client, root, *maxSteps, soul)
			requestAgent.Knowledge = store
			requestAgent.Memory = memoryStore
			requestAgent.MCPTools = mcpTools
			if len(history) > 40 {
				history = history[len(history)-40:]
			}
			messages := make([]agent.Message, len(history))
			for index, message := range history {
				messages[index] = agent.Message{Role: message.Role, Content: message.Content}
			}
			if err := requestAgent.SetHistory(messages); err != nil {
				return "", err
			}
			return requestAgent.Ask(requestCtx, question)
		}, func(requestCtx context.Context, sessionID string, turnID int64, question, answer string) error {
			if memoryStore == nil {
				return nil
			}
			return memoryStore.RememberConversation(requestCtx, sessionID, turnID, question, answer)
		}, func(requestCtx context.Context, sessionID string) error {
			if memoryStore == nil {
				return nil
			}
			return memoryStore.DeleteConversation(requestCtx, sessionID)
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

func connectMCP(ctx context.Context, endpoint, token string) (*agent.MCPToolClient, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		remote, err := agent.NewMCPToolClient(attemptCtx, endpoint, token)
		cancel()
		if err == nil {
			return remote, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect MCP server: %v: %w", lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}
