#!/usr/bin/env bash
set -euo pipefail

namespace=${JULIE_NAMESPACE:-julie}
release=${JULIE_RELEASE:-julie}
reset_image=${JULIE_RESET_IMAGE:-busybox:1.37}
local_qdrant_port=${JULIE_QDRANT_LOCAL_PORT:-16333}
dry_run=0

if [[ $# -gt 1 || ( $# -eq 1 && $1 != --dry-run ) ]]; then
	printf 'Usage: %s [--dry-run]\n' "$0" >&2
	exit 2
fi
if [[ ${1:-} == --dry-run ]]; then
	dry_run=1
fi

for dependency in kubectl curl jq; do
	if ! command -v "$dependency" >/dev/null 2>&1; then
		printf 'Required command not found: %s\n' "$dependency" >&2
		exit 1
	fi
done

instance_selector="app.kubernetes.io/instance=$release"
deployment_list=$(kubectl get deployments -n "$namespace" -l "$instance_selector" -o json)
deployment_count=$(jq '[.items[] | select(.spec.template.metadata.labels["app.kubernetes.io/component"] == "agent")] | length' <<<"$deployment_list")
if [[ $deployment_count != 1 ]]; then
	printf 'Expected exactly one Julie agent Deployment for release %s in namespace %s; found %s.\n' "$release" "$namespace" "$deployment_count" >&2
	exit 1
fi
deployment_json=$(jq -c '[.items[] | select(.spec.template.metadata.labels["app.kubernetes.io/component"] == "agent")][0]' <<<"$deployment_list")
deployment=$(jq -r '.metadata.name' <<<"$deployment_json")
replicas=$(jq -r '.spec.replicas // 1' <<<"$deployment_json")
selector=$(jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")' <<<"$deployment_json")
selector="$selector,app.kubernetes.io/component=agent"
sessions_claim=$(jq -er '.spec.template.spec.volumes[] | select(.name == "sessions") | .persistentVolumeClaim.claimName' <<<"$deployment_json")
sessions_mount=$(jq -er '.spec.template.spec.containers[] | select(.name == "julie") | .volumeMounts[] | select(.name == "sessions") | .mountPath' <<<"$deployment_json")
sessions_db=$(jq -er '.spec.template.spec.containers[] | select(.name == "julie") | .args as $args | $args[($args | index("-sessions-db") + 1)]' <<<"$deployment_json")
if [[ $replicas != 1 || $sessions_db != "$sessions_mount/sessions.db" ]]; then
	printf 'Unexpected Julie replica or session database configuration; refusing to reset.\n' >&2
	exit 1
fi

config_map=$(jq -er '.spec.template.spec.volumes[] | select(.name == "model-config") | .configMap.name' <<<"$deployment_json")
model_config=$(kubectl get configmap "$config_map" -n "$namespace" -o json | jq -er '.data["model.json"] | fromjson')
qdrant_url=$(jq -er '.vectorStore.url | strings | select(length > 0)' <<<"$model_config")
collection=$(jq -r '(.vectorStore.collection // "julie_knowledge") | if . == "" then "julie_knowledge" else . end' <<<"$model_config")
qdrant_authority=${qdrant_url#*://}
qdrant_authority=${qdrant_authority%%/*}
qdrant_host=${qdrant_authority%%:*}
qdrant_port=${qdrant_authority#*:}
if [[ $qdrant_port == "$qdrant_authority" ]]; then
	qdrant_port=6333
fi
if [[ ! $qdrant_port =~ ^[0-9]+$ || ! $local_qdrant_port =~ ^[0-9]+$ || ! $collection =~ ^[a-zA-Z0-9_-]+$ ]]; then
	printf 'Unsupported Qdrant URL, local port, or collection name; refusing to reset.\n' >&2
	exit 1
fi
qdrant_service=$(kubectl get services -n "$namespace" -o json | jq -er --arg host "$qdrant_host" '[.items[] | select(.metadata.name == $host)] | if length == 1 then .[0].metadata.name else empty end') || {
	printf 'Qdrant host %s is not a Service in namespace %s. This script currently supports the chart-local Qdrant service.\n' "$qdrant_host" "$namespace" >&2
	exit 1
}

collections=("$collection" "${collection}_conversations")
if (( dry_run )); then
	printf 'Dry run only. No changes made.\n'
	printf 'Would stop Deployment: %s/%s\n' "$namespace" "$deployment"
	printf 'Would delete Qdrant collections: %s\n' "${collections[*]}"
	printf 'Would clear SQLite session files under PVC %s at %s\n' "$sessions_claim" "$sessions_db"
	printf 'Would restart Deployment with %s replica.\n' "$replicas"
	exit 0
fi

printf 'This permanently deletes all indexed documents, conversation memories, and session transcripts for %s/%s.\n' "$namespace" "$release"
read -r -p 'Type RESET to continue: ' confirmation
if [[ $confirmation != RESET ]]; then
	printf 'Reset cancelled.\n'
	exit 1
fi

port_forward_pid=
port_forward_log=
response_file=
reset_pod=
deployment_scaled=0
cleanup() {
	local status=$?
	trap - EXIT
	if [[ -n $reset_pod ]]; then
		kubectl delete pod "$reset_pod" -n "$namespace" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
	fi
	if (( deployment_scaled )); then
		kubectl scale deployment "$deployment" -n "$namespace" --replicas="$replicas" >/dev/null 2>&1 || true
		kubectl rollout status deployment "$deployment" -n "$namespace" --timeout=180s >/dev/null 2>&1 || true
	fi
	if [[ -n $port_forward_pid ]]; then
		kill "$port_forward_pid" >/dev/null 2>&1 || true
		wait "$port_forward_pid" 2>/dev/null || true
	fi
	[[ -z $port_forward_log ]] || rm -f "$port_forward_log"
	[[ -z $response_file ]] || rm -f "$response_file"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

port_forward_log=$(mktemp)
kubectl port-forward --address 127.0.0.1 -n "$namespace" "service/$qdrant_service" "$local_qdrant_port:$qdrant_port" >"$port_forward_log" 2>&1 &
port_forward_pid=$!
if ! curl --fail --silent --connect-timeout 2 --max-time 20 --retry 15 --retry-connrefused --retry-delay 1 "http://127.0.0.1:$local_qdrant_port/collections" >/dev/null 2>&1; then
	cat "$port_forward_log" >&2
	printf 'Could not connect to Qdrant through kubectl port-forward.\n' >&2
	exit 1
fi

kubectl scale deployment "$deployment" -n "$namespace" --replicas=0 >/dev/null
deployment_scaled=1
kubectl wait --for=delete pod -n "$namespace" -l "$selector" --timeout=180s

response_file=$(mktemp)
for name in "${collections[@]}"; do
	status=$(curl --silent --show-error --output "$response_file" --write-out '%{http_code}' -X DELETE "http://127.0.0.1:$local_qdrant_port/collections/$name")
	case $status in
		200)
			printf 'Deleted Qdrant collection %s.\n' "$name"
			;;
		404)
			printf 'Qdrant collection %s did not exist.\n' "$name"
			;;
		*)
			cat "$response_file" >&2
			printf 'Qdrant rejected deletion of collection %s (HTTP %s).\n' "$name" "$status" >&2
			exit 1
			;;
	esac
done

reset_pod="julie-session-reset-$$"
pod_manifest=$(jq -cn \
	--arg namespace "$namespace" \
	--arg name "$reset_pod" \
	--arg image "$reset_image" \
	--arg claim "$sessions_claim" \
	--arg db "$sessions_db" \
	'{apiVersion:"v1",kind:"Pod",metadata:{name:$name,namespace:$namespace},spec:{restartPolicy:"Never",automountServiceAccountToken:false,securityContext:{runAsNonRoot:true,runAsUser:65532,runAsGroup:65532,fsGroup:65532,seccompProfile:{type:"RuntimeDefault"}},containers:[{name:"reset",image:$image,imagePullPolicy:"IfNotPresent",command:["sh","-c","rm -f -- \"$1\" \"$1-wal\" \"$1-shm\" && echo Session database cleared","reset-sessions",$db],securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}},volumeMounts:[{name:"sessions",mountPath:"/var/lib/julie"}]}],volumes:[{name:"sessions",persistentVolumeClaim:{claimName:$claim}}]}}')
kubectl create -f - <<<"$pod_manifest" >/dev/null
if ! kubectl wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$reset_pod" -n "$namespace" --timeout=180s; then
	kubectl logs "$reset_pod" -n "$namespace" >&2 || true
	printf 'The session reset pod did not complete successfully.\n' >&2
	exit 1
fi
kubectl logs "$reset_pod" -n "$namespace"
kubectl delete pod "$reset_pod" -n "$namespace" --wait=true --timeout=120s >/dev/null
reset_pod=

kubectl scale deployment "$deployment" -n "$namespace" --replicas="$replicas" >/dev/null
deployment_scaled=0
kubectl rollout status deployment "$deployment" -n "$namespace" --timeout=180s
printf 'Julie RAG and session memories have been reset. Collections will be recreated on use.\n'