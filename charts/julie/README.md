# Julie Helm Chart

Requires Helm 3+ and a Kubernetes cluster. The default model ID, endpoint, and
tool-calling capability mirror `etc/model.json`; override `model.*` when changing
models. Credentials are never embedded in the chart or container image.

## Build and Publish

Replace the example registry with your container registry. Build for your cluster's
CPU architecture, or publish a multi-architecture image using Docker Buildx.

```sh
docker build -t ghcr.io/YOUR_ORG/julie:0.1.0 .
docker push ghcr.io/YOUR_ORG/julie:0.1.0
```

For a private registry, create a registry pull Secret in the target namespace and
set `imagePullSecrets` to a list such as `[{name: registry-credentials}]` in your
own values file.

## Credentials

```sh
kubectl create namespace julie
```

Create a Secret named `julie-credentials` in that namespace containing `api-key`
(the model endpoint's Bearer token). Julie's HTTP API does not require an API
token. Use your secret manager, or use existing local files without putting
secrets into command arguments or shell history:

```sh
kubectl -n julie create secret generic julie-credentials \
  --from-file=api-key=/secure/path/model-api-key
```

The file must contain the raw value without a trailing newline. An empty
`api-key` value omits model authorization. To use another Secret, set
`credentials.existingSecret` and `credentials.apiKeyKey`. After rotating model
credentials, restart the Deployment because environment variables load on startup.

## Install

```sh
helm lint ./charts/julie
helm upgrade --install julie ./charts/julie \
  --namespace julie \
  --set image.repository=ghcr.io/YOUR_ORG/julie \
  --set-string image.tag=0.1.0 \
  --wait --timeout 5m
kubectl -n julie rollout status deployment/julie-julie
kubectl -n julie logs deployment/julie-julie
```

The chart creates a Deployment, ClusterIP Service, model ConfigMap, and
persistent volume claim for SQLite session history. It uses
non-root execution, a read-only filesystem, resource limits, health probes, and
no service-account token. Config changes trigger a rollout. Model requests need
outbound DNS and HTTPS access to the configured endpoint. Health probes check
the local process, not the model endpoint or credential validity.

## Document Knowledge

Retrieval is disabled by default. `values-k3s.yaml` enables the chart's local
Ollama embeddings service (`nomic-embed-text`) and persistent Qdrant instance.
Apply it with:

```sh
helm upgrade --install julie ./charts/julie \
  --namespace julie \
  --values ./charts/julie/values-k3s.yaml \
  --wait --timeout 15m
```

The chart provisions single-replica Deployments and persistent claims for both
services. Qdrant creates the cosine-distance collection on first use, deriving
the vector size from the embeddings response. The model weights and vector data
survive pod replacement; back up their PVCs before maintenance. Ingest `.md`
and `.txt` files explicitly with `-ingest`; see the root README for limits and
the command. The local inference pod requests 512 MiB and is limited to 2 GiB.
For a remote embeddings endpoint instead, set `knowledge.local.enabled=false`
and configure `knowledge.embeddings` and `knowledge.vectorStore`.

Document contents are sent to the in-cluster embedding service and persisted in
Qdrant. Both services have ClusterIP-only access; ingest only approved material.

## Reset Memory

Run `./scripts/reset-julie-memory.sh --dry-run` to inspect the targets. To
permanently delete all indexed documents, conversation memories, and session
transcripts, run `./scripts/reset-julie-memory.sh` and type `RESET` when
prompted. The script stops Julie, deletes the configured document and
conversation Qdrant collections, clears the SQLite database on the sessions
PVC using a temporary unprivileged pod, and restarts Julie. Collections are
recreated on next use. This requires `kubectl`, `curl`, and `jq`; the active
Kubernetes context must target the intended cluster. `JULIE_NAMESPACE`,
`JULIE_RELEASE`, and `JULIE_RESET_IMAGE` can override the defaults.

## Soul File

The k3s values file expects an operator-managed ConfigMap named `julie-soul`.
Create it from a file outside the repository; the file contents are not stored
in Git or the container image:

```sh
kubectl create configmap julie-soul -n julie \
  --from-file=soul.md=/path/outside/repo/soul.md \
  --dry-run=client -o yaml | kubectl apply -f -
```

The chart mounts it read-only at `/run/julie-soul`, outside the agent's
workspace, and loads it once when Julie starts. The file is limited to 16 KiB of
UTF-8 text. After changing the ConfigMap, restart the Deployment so the new
contents are loaded. ConfigMaps are not secret storage; use a Secret instead if
the text contains confidential material.

## Call the Agent

```sh
kubectl -n julie port-forward service/julie-julie 8080:8080
```

Create a conversation and send a message without an Authorization header:

```sh
SESSION=$(curl --fail-with-body -sS -X POST http://localhost:8080/sessions | jq -r .session_id)
curl --fail-with-body http://localhost:8080/sessions/${SESSION}/messages \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Explain Go interfaces"}'
```

The response contains the `session_id` and `answer`. `GET /sessions/{id}` returns
the transcript, and `DELETE /sessions/{id}` removes it. `POST /ask` remains
available for a one-off request without creating a session; when memory is
configured it can still retrieve relevant prior turns. Session history is stored
in SQLite; when knowledge is enabled, turns are also embedded in a separate
Qdrant collection and can be recalled across sessions. Deleting a session
removes both its transcript and indexed memories. Session history requires one
Julie replica; the chart rejects other replica counts. Configure
`sessions.existingClaim` to use an externally managed PVC instead of the
chart-created one.

The Service is cluster-internal, but the API has no caller authentication or
session ownership checks. Any client that can reach it can create sessions;
possession of a session ID permits reading or deleting that session.
Conversation memories are shared across all sessions and callers. Keep it on a
trusted network; do not expose it through a public ingress without adding an
authenticated gateway.
Requests are limited to 1 MiB and eight simultaneous model requests per pod;
saturation returns HTTP 429. Requests use `agent.timeout` (default `3m`) and
`agent.maxSteps` (default 8). Prompts and tool output are sent to the model
endpoint.

## Workspace and Scaling

Workspace tools are disabled by default. To enable them, set
`workspace.toolsEnabled=true` and `workspace.existingClaim` to a PVC containing
nonsensitive files. It is mounted read-only at `/workspace`. Without a PVC the
workspace is empty. Choose a PVC access mode compatible with node placement.
API replicas are fixed at one because SQLite session history is single-writer.

If you increase `agent.timeout`, also increase
`terminationGracePeriodSeconds` to allow at least the timeout plus 15 seconds.
Ensure upstream proxy timeouts permit long-running model responses. The default
termination grace period is 200 seconds.

Uninstall with `helm uninstall julie -n julie`. Externally managed Secrets and
PVCs are not removed by Helm. The chart-managed sessions PVC is removed with the
release; configure `sessions.existingClaim` to retain storage independently.