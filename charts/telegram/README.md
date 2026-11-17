# Julie Telegram Chart

This chart deploys a Telegram Bot API long-poller and a private Streamable HTTP
MCP server. It accepts private text messages only from configured users, sends
each message to Julie as a turn in that user's persistent Julie session, and
returns Julie's answer to the same user. Incoming and sent messages are also
embedded in a dedicated Qdrant collection. The service provides
`telegram_search_history` and `telegram_send_message` tools to Julie.

## Prerequisites

- Install Julie in the `julie` namespace with its local embeddings and Qdrant
  services ready, or configure equivalent reachable endpoints below.
- Publish an image containing `/julie-telegram` for the target architectures.
- Create the bot-token and MCP-token Secrets. Tokens must not be stored in the
  ConfigMap or Helm values.

```sh
kubectl create secret generic telegram-credentials --namespace julie \
  --from-literal=bot-token='YOUR_TELEGRAM_BOT_TOKEN'

openssl rand -hex 32 | kubectl create secret generic telegram-mcp-auth \
  --namespace julie --from-file=token=/dev/stdin
```

## Install

The ConfigMap defaults both allowlists to empty, which denies all incoming
messages and outgoing destinations. Set `telegram.allowedUserIDs` to the
numeric IDs of the users Julie may talk with. Only private chats are processed;
group messages are ignored. `telegram.allowedChatIDs` can add a second
restriction, but cannot replace the required user allowlist.

```sh
VERSION=20261117000000
helm upgrade --install telegram ./charts/telegram \
  --namespace julie \
  --set-string image.repository=100.0.0.2:30500/julie \
  --set-string image.tag="$VERSION" \
  --set-json telegram.allowedUserIDs='[123456789]'
```

The service is a ClusterIP named `telegram-telegram` when installed with release
name `telegram`. Configure Julie's existing MCP service to proxy its tools by
upgrading the Julie release after the Telegram deployment is Ready:

```sh
helm upgrade --install julie ./charts/julie \
  --namespace julie \
  --values ./charts/julie/values-k3s.yaml \
  --set-string image.repository=100.0.0.2:30500/julie \
  --set-string image.tag="$VERSION" \
  --set mcp.telegram.enabled=true
```

The Julie chart defaults the Telegram endpoint to
`http://telegram-telegram:8082/mcp` and MCP token Secret to
`telegram-mcp-auth`. Override `mcp.telegram.endpoint` and
`mcp.telegram.existingSecret` when using different release names, namespaces,
or Secret names. Julie's MCP server connects to Telegram during startup, so
install and verify Telegram first.

## Configuration and Data

The chart ConfigMap contains allowed IDs, polling and agent timeouts, the Julie
API URL, and embedding/Qdrant endpoints. A persistent PVC stores the per-user
Julie session mapping and Telegram update checkpoint. `telegram-credentials`
contains the Bot API token; `telegram-mcp-auth` contains the bearer token shared
with Julie's MCP proxy. The optional embedding API key is read from the
configured existing Secret. The MCP Service is only cluster-internal and
requires bearer authentication.

History is embedded into `telegram_history` by default, with a Qdrant payload
filter restricting searches to the requested chat. Messages are sent to the
configured embedding and Julie model providers. The in-memory polling offset can
cause Telegram to redeliver pending updates after a restart; deterministic vector
IDs make repeated ingestions idempotent. Outgoing send calls are not automatically
retried when their result is uncertain, to avoid duplicate messages. The tool
reports `history_embedded: false` if sending succeeds but embedding fails.

Only private text messages from allowlisted users are processed. Julie sessions
are kept separate by Telegram user ID and persisted across Telegram pod
restarts. Telegram updates are retained by the Bot API for a limited time, so
prolonged outages may cause missed messages. The chart uses one replica with a
`Recreate` strategy to avoid competing pollers. Incoming prompts and Julie's
answers go to the configured model provider; approved message text is also sent
to the embedding service.