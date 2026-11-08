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

## HTTP API and Kubernetes

For a long-running service, start server mode:

```sh
go run ./cmd/julie -listen :8080 -no-tools
```

`POST /sessions` creates a conversation and returns its opaque `session_id`.
Post turns to `/sessions/{session_id}/messages`, read its transcript with
`GET /sessions/{session_id}`, and delete it with `DELETE /sessions/{session_id}`.
`POST /ask` remains available for a one-off request without creating a session;
when memory is configured it can still retrieve relevant prior turns. The API
does not require caller tokens or check session ownership: possession of a
session ID allows reading or deleting that session, and conversation memories
are shared across all sessions. Keep it on a trusted network. Server mode shuts
down gracefully on SIGTERM.

For an interactive Bash client, with `curl` and `jq` installed and the API
reachable at `http://localhost:8080`:

```sh
./scripts/julie-chat.sh
```

Set `JULIE_API_URL` to use another API address. The shell starts a new session;
`/history` displays its transcript, `/new` starts another session, `/delete`
removes the current session, and `/exit` keeps it for later.

The [Helm chart](charts/julie/README.md) includes container build, Secret setup,
installation, and API access instructions. The container does not include model
configuration or credentials; Helm mounts the configuration and references an
existing Kubernetes Secret.

## Document Knowledge

Document retrieval is disabled unless `embeddings.url`, `embeddings.model`, and
`vectorStore.url` are configured. The embedding endpoint is independent of the
chat-completions model. Julie creates a cosine-distance Qdrant collection on
first use; vector dimensions are taken from the first embedding response.

The initial ingestion path accepts `.md` and `.txt` files or directories, with a
1 MiB per-file limit and at most 200 files per directory run. It splits text into
overlapping passages, stores source/title/content-hash metadata, and replaces
previous passages for the same source. Ingestion is explicit:

```sh
go run ./cmd/julie -config etc/model.json -ingest ./documents
```

Configure the `embeddings` and `vectorStore` objects in the selected model
configuration first. Set `JULIE_EMBEDDING_API_KEY` separately from
`JULIE_API_KEY` when the embedding service requires authentication. The model
can then call `search_knowledge`; retrieved content is untrusted evidence, and
answers should cite the returned source names.

Document text and, when the HTTP API uses configured embeddings and Qdrant,
conversation turns are sent to the embedding provider. Document vectors and
conversation memories use separate collections. Conversation memories are
shared across sessions; deleting a session removes its SQLite transcript and
Qdrant memories. Session transcripts are persisted to `julie-sessions.db` by
default, configurable with `-sessions-db`. Ingest only approved documents.

## Make Workflow

GNU Make and Go 1.24+ are required. AMD means Linux `amd64` (x86-64); ARM means
Linux `arm64` (AArch64), matching the current k3s nodes.

```sh
make check
```

This runs native race tests and `go vet`, builds both agent binaries under
`build/amd64` and `build/arm64`, and cross-compiles test binaries. Cross-compiling
tests does not execute them on the other CPU architecture.

Install Docker Buildx to run per-architecture tests and publish images:

```sh
make test-amd64
make test-arm64
make test-platforms
```

The Buildx test stage executes the full suite on each selected architecture.
The non-native platform needs working QEMU/binfmt emulation or a configured
native multi-node builder. Docker Desktop normally includes emulation; on Linux,
have an administrator configure it. The Makefile does not install privileged
emulators or restart Docker/k3s services.

The default registry is the existing k3s HTTP registry at `100.0.0.2:30500`.
`buildkitd.toml` enables HTTP for this endpoint in a dedicated Buildx builder;
it does not modify the Docker daemon. If changing the registry, also provide a
matching `BUILDKIT_CONFIG` for that registry's HTTP/TLS configuration. An existing
builder can be selected with `BUILDER`; its registry configuration must already
be correct. The registry is unauthenticated and should stay on a trusted network.

```sh
make publish VERSION=0.2.0
make apply VERSION=0.2.0
```

`publish` (alias `push`) requires native checks and passing AMD64/ARM64 runtime
tests, then pushes a multi-architecture image under one tag. `apply` (alias
`deploy`) checks that the credential Secret exists, publishes the image, then
uses `helm upgrade --install --wait` to apply the chart. Images must be pushed
before applying the Deployment so pods can pull them.

Create the namespace and `julie-credentials` Secret first as described in the
[Helm instructions](charts/julie/README.md). Helm must be on PATH, and kubectl's
current context must point to the intended cluster. The default release and
namespace are both `julie`. k3s nodes must be configured to pull from the HTTP
registry; the current cluster already has this configuration.

```sh
make apply VERSION=0.2.1 NAMESPACE=julie RELEASE=julie \
	REGISTRY=100.0.0.2:30500 IMAGE_NAME=julie \
	CREDENTIALS_SECRET=julie-credentials
```

Use `HELM_VALUES=/path/to/values.yaml` for model, resource, or workspace overrides.
Without `VERSION`, a UTC timestamp is used and preserved across the pipeline.
Use a new version for each release rather than overwriting a deployed tag.
`make help` lists the main targets. `make apply` publishes again; use it directly
for the full workflow rather than running `publish` first.
