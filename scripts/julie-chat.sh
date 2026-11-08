#!/usr/bin/env bash
set -euo pipefail

API_URL=${JULIE_API_URL:-http://localhost:8080}
API_URL=${API_URL%/}
CURL_MAX_TIME=${JULIE_CURL_MAX_TIME:-240}
SESSION_ID=

for dependency in curl jq; do
	if ! command -v "$dependency" >/dev/null 2>&1; then
		printf 'Required command not found: %s\n' "$dependency" >&2
		exit 1
	fi
done

request() {
	curl --fail-with-body --silent --show-error \
		--connect-timeout 5 --max-time "$CURL_MAX_TIME" "$@"
}

create_session() {
	local response
	response=$(request -X POST "$API_URL/sessions")
	SESSION_ID=$(jq -er '.session_id | strings | select(length > 0)' <<<"$response")
	printf 'Started session %s\n' "$SESSION_ID"
}

show_history() {
	local response
	response=$(request "$API_URL/sessions/$SESSION_ID")
	jq -r '.messages[]? | "\(.role)> \(.content)"' <<<"$response"
}

send_message() {
	local prompt=$1 response answer
	response=$(request -X POST "$API_URL/sessions/$SESSION_ID/messages" \
		-H 'Content-Type: application/json' \
		--data-binary "$(jq -cn --arg prompt "$prompt" '{prompt: $prompt}')")
	answer=$(jq -er '.answer | strings' <<<"$response")
	printf 'Julie> %s\n' "$answer"
}

delete_session() {
	request -X DELETE "$API_URL/sessions/$SESSION_ID" >/dev/null
	printf 'Deleted session %s\n' "$SESSION_ID"
	SESSION_ID=
}

if ! request "$API_URL/healthz" >/dev/null; then
	printf 'Julie API is not reachable at %s\n' "$API_URL" >&2
	exit 1
fi

create_session
printf 'Commands: /new, /history, /delete, /help, /exit\n'

while IFS= read -r -p 'you> ' line || [[ -n "$line" ]]; do
	case "$line" in
		'')
			continue
			;;
		/exit)
			printf 'Session retained: %s\n' "$SESSION_ID"
			exit 0
			;;
		/new)
			create_session
			;;
		/history)
			show_history
			;;
		/delete)
			delete_session
			exit 0
			;;
		/help)
			printf '/new starts another session; /history shows this transcript; /delete removes this session; /exit keeps it.\n'
			;;
		*)
			send_message "$line"
			;;
	esac
done

printf '\nSession retained: %s\n' "$SESSION_ID"