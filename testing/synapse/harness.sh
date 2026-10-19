#!/usr/bin/env bash
# Throwaway local Synapse for the Matrix adapter's live suite (`just e2e`).
#
# Adapted from rihma's testing/synapse, without Element Web. Deliberately
# NOT a real Matrix server: server_name `localhost`, no federation, open
# registration, sqlite, published on 127.0.0.1 only, all state under
# ./data (gitignored). The signing key is generated into ./data on first
# start, so nothing secret-shaped lives in the repository.
#
# Plain `run` commands rather than compose, so it needs only podman (or
# docker, via CONTAINER_ENGINE). The port and container name differ from
# rihma's harness so the two can run side by side.
#
# usage: harness.sh up | down | clean | status
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
data=${SYNAPSE_DATA_DIR:-"$here/data"}

engine=${CONTAINER_ENGINE:-}
if [ -z "$engine" ]; then
	if command -v podman >/dev/null; then engine=podman; else engine=docker; fi
fi

synapse_port=${SYNAPSE_PORT:-18118}
synapse_image=${SYNAPSE_IMAGE:-ghcr.io/element-hq/synapse:latest}
synapse_name=${SYNAPSE_NAME:-airut-synapse}

exists() { "$engine" container exists "$1" 2>/dev/null || "$engine" inspect "$1" >/dev/null 2>&1; }

up() {
	mkdir -p "$data"
	sed "s/@SYNAPSE_PORT@/$synapse_port/g" "$here/homeserver.yaml.in" >"$data/homeserver.yaml"

	if ! exists "$synapse_name"; then
		# Synapse runs as the container's root: under rootless podman that is
		# the invoking user, so ./data stays removable without sudo.
		"$engine" run -d --name "$synapse_name" \
			-p "127.0.0.1:$synapse_port:8008" \
			-v "$data/homeserver.yaml:/config/homeserver.yaml:ro" \
			-v "$here/log.config:/config/log.config:ro" \
			-v "$data:/data" \
			--entrypoint /bin/sh "$synapse_image" -c \
			'python -m synapse.app.homeserver --config-path /config/homeserver.yaml --generate-keys &&
			 exec python -m synapse.app.homeserver --config-path /config/homeserver.yaml' >/dev/null
	else
		"$engine" start "$synapse_name" >/dev/null
	fi

	for _ in $(seq 1 120); do
		if curl -fsS "http://127.0.0.1:$synapse_port/health" >/dev/null 2>&1; then
			echo "synapse up: http://127.0.0.1:$synapse_port (server_name: localhost, throwaway)"
			return 0
		fi
		sleep 1
	done
	echo "$synapse_name did not become healthy in 120s; last log lines:" >&2
	"$engine" logs --tail 30 "$synapse_name" >&2 || true
	return 1
}

down() {
	if exists "$synapse_name"; then "$engine" rm -f "$synapse_name" >/dev/null; fi
}

clean() {
	down
	rm -rf "$data"
}

status() {
	"$engine" ps -a --filter "name=^$synapse_name$" --format '{{.Names}}  {{.Status}}'
	if curl -fsS "http://127.0.0.1:$synapse_port/health" >/dev/null 2>&1; then
		echo "synapse healthy on 127.0.0.1:$synapse_port"
	else
		echo "synapse not answering on 127.0.0.1:$synapse_port"
	fi
}

case "${1:-}" in
up) up ;;
down) down ;;
clean) clean ;;
status) status ;;
*)
	echo "usage: $0 up | down | clean | status" >&2
	exit 2
	;;
esac
