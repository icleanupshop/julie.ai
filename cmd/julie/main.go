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
	config, model, err := agent.LoadConfig(*configPath, *modelID)
	if err != nil {
		return err
	}
	var root *os.Root
	if !*noTools && model.ToolCalling {
		root, err = os.OpenRoot(*workspace)
		if err != nil {
			return fmt.Errorf("open workspace: %w", err)
		}
		defer root.Close()
	}
	julie := agent.New(agent.NewClient(config, model), root, *maxSteps)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
