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
(the model endpoint's Bearer token) and `auth-token` (a separate strong, random
token protecting Julie's HTTP API). Use your secret manager, or use existing
local files without putting secrets into command arguments or shell history:

```sh
kubectl -n julie create secret generic julie-credentials \
  --from-file=api-key=/secure/path/model-api-key \
  --from-file=auth-token=/secure/path/julie-auth-token
```

These files must contain raw values without trailing newlines. An empty `api-key`
omits model authorization; `auth-token` must not be empty. To use another Secret,
set `credentials.existingSecret`, `credentials.apiKeyKey`, and
`credentials.authTokenKey`. After rotating credentials, restart the Deployment
because environment variables are loaded only on startup.

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

The chart creates a Deployment, ClusterIP Service, and model ConfigMap. It uses
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

In another terminal, with `JULIE_AUTH_TOKEN` already set securely:

```sh
curl --fail-with-body http://localhost:8080/ask \
  -H "Authorization: Bearer ${JULIE_AUTH_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Explain Go interfaces"}'
```

The response is `{"answer":"..."}`. Each request has an independent conversation;
no history is shared between users or replicas. Requests are limited to 1 MiB
and eight simultaneous requests per pod; saturation returns HTTP 429. Requests
use `agent.timeout` (default `3m`) and `agent.maxSteps` (default 8).

The Service is cluster-internal, not an Internet-facing endpoint. Use a TLS
ingress or gateway plus appropriate network policies and rate limits before
exposing it externally. The built-in server uses plain HTTP; do not send tokens
over untrusted networks without TLS. Prompts and tool output are sent to the
model endpoint.

## Workspace and Scaling

Workspace tools are disabled by default. To enable them, set
`workspace.toolsEnabled=true` and `workspace.existingClaim` to a PVC containing
nonsensitive files. It is mounted read-only at `/workspace`. Without a PVC the
workspace is empty. Choose a PVC access mode compatible with your replica count
and node placement; use `replicaCount` to scale stateless API replicas.

If you increase `agent.timeout`, also increase
`terminationGracePeriodSeconds` to allow at least the timeout plus 15 seconds.
Ensure upstream proxy timeouts permit long-running model responses. The default
termination grace period is 200 seconds.

Uninstall with `helm uninstall julie -n julie`. The externally managed Secret and
PVC are not removed by Helm.