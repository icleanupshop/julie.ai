package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"julie.ai/internal/knowledge"
	"julie.ai/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Julie Telegram:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/julie-telegram/config.json", "Telegram service configuration")
	statePath := flag.String("state-db", "/var/lib/julie-telegram/state.db", "Path to persistent Telegram state database")
	listen := flag.String("listen", ":8082", "MCP HTTP listen address")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	file, err := os.Open(*configPath)
	if err != nil {
		return fmt.Errorf("open Telegram configuration: %w", err)
	}
	defer file.Close()
	var config telegram.Config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("decode Telegram configuration: %w", err)
	}
	store, err := knowledge.NewStore(config.Embeddings, config.VectorStore, os.Getenv("JULIE_EMBEDDING_API_KEY"))
	if err != nil {
		return err
	}
	state, err := telegram.OpenSQLiteState(context.Background(), *statePath)
	if err != nil {
		return err
	}
	defer state.Close()
	service, err := telegram.NewService(config, os.Getenv("TELEGRAM_BOT_TOKEN"), store, state)
	if err != nil {
		return err
	}
	handler, err := service.MCPHandler(os.Getenv("TELEGRAM_MCP_TOKEN"))
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go service.Poll(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve Telegram MCP: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down Telegram MCP: %w", err)
		}
	}
	return nil
}
