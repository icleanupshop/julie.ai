package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"julie.ai/internal/agent"
	"julie.ai/internal/knowledge"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Julie MCP:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", ":8081", "MCP HTTP listen address")
	configPath := flag.String("config", "/etc/julie/model.json", "Model configuration file")
	workspacePath := flag.String("workspace", "/workspace", "Read-only workspace directory")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	token := os.Getenv("JULIE_MCP_TOKEN")
	if token == "" {
		return fmt.Errorf("JULIE_MCP_TOKEN is required")
	}
	root, err := os.OpenRoot(*workspacePath)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	defer root.Close()
	config, _, err := agent.LoadConfig(*configPath, "")
	if err != nil {
		return err
	}
	store, err := knowledge.NewStore(config.Embeddings, config.VectorStore, config.EmbeddingAPIKey)
	if err != nil {
		return err
	}
	handler, err := agent.NewMCPHTTPHandler(agent.NewMCPServer(root, store), token)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("serve MCP: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down MCP server: %w", err)
		}
	}
	return nil
}
