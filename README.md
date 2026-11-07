# julie.ai

Julie is a Go command-line AI agent using the chat-completions model configured
in `etc/model.json`. It supports interactive conversations and a bounded tool
loop with read-only workspace access. It uses only the Go standard library.

## Run

Requires Go 1.24 or newer. Run from the repository root:

```sh
go run ./cmd/julie
```

Ask one question:

```sh
go run ./cmd/julie -prompt "What files are in this project?"
```

In interactive mode, `/reset` clears conversation history and `/exit` quits.
History is kept in memory for this process only. Failed turns are not retained.

## Configuration

Julie uses the first model in `etc/model.json` by default and sends requests to
its exact `url`. The configured endpoint must accept OpenAI-compatible
chat-completions requests; Julie does not call the AWS Bedrock API directly or
perform AWS SigV4 signing.

The configured `apiKey` is sent as a Bearer token. Set `JULIE_API_KEY` in your
environment to override it without editing the configuration. An explicitly
empty override omits the Authorization header. Do not commit real credentials.

```sh
go run ./cmd/julie -config etc/model.json -model llama3.3:70b-bedrock
go run ./cmd/julie -workspace ./etc -prompt "Describe model.json"
go run ./cmd/julie -no-tools -prompt "Explain Go interfaces"
```

`-max-steps` limits model calls per question (default 8). `-timeout` sets a total
question timeout (default `3m`); individual HTTP requests also have a 2-minute
timeout. Run `go run ./cmd/julie -help` for all flags.

## Workspace Tools

- `list_files`: lists up to 200 directory entries, without recursion.
- `read_file`: reads a regular file up to 64 KiB; larger files are rejected.

Tools are enabled only when the model declares `toolCalling: true` and
`-no-tools` is not set. Go's `os.Root` confines paths and symlink traversal to
`-workspace` (the current directory by default). Julie cannot execute commands
or modify files. Tool errors are returned to the model so it can recover.

**Privacy:** prompts, conversation history, and any files the model reads are
sent to the configured endpoint. All readable files inside the chosen workspace
are available, including hidden files and credentials. Choose a directory
without sensitive data, or use `-no-tools`. Path confinement is not a full OS
sandbox and does not protect against mounts or hard links placed inside it.

## Test

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests use local mock servers and never call the configured live endpoint.
