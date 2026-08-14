#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
go_bin=${NOEMA_GO_BIN:-"$repo_root/noema"}
rust_bin=${NOEMA_RUST_BIN:-"$repo_root/rust/target/debug/noema-rs"}
base_port=${NOEMA_HTTP_TEST_PORT:-39140}

test_root=$(mktemp -d /tmp/noema-http-smoke.XXXXXX)
test_home="$test_root/home"
cortex_parent="$test_root/cortexes"
mkdir -p "$test_home" "$cortex_parent"
server_pid=
trap 'if [ -n "$server_pid" ]; then kill "$server_pid" 2>/dev/null || true; fi; rm -rf "$test_root"' EXIT HUP INT TERM

env HOME="$test_home" "$go_bin" init --name shared --path "$cortex_parent" >/dev/null

probe() {
    label=$1
    binary=$2
    port=$3
    log="$test_root/$label.log"
    response="$test_root/$label.response"
    env HOME="$test_home" "$binary" --cortex shared serve \
        --transport http --host 127.0.0.1 --port "$port" >"$log" 2>&1 &
    server_pid=$!

    attempts=0
    while :; do
        if curl -fsS -X POST "http://127.0.0.1:$port/mcp" \
            -H 'Content-Type: application/json' \
            -H 'Accept: application/json, text/event-stream' \
            -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"http-smoke","version":"1"}}}' \
            >"$response" 2>/dev/null; then
            break
        fi
        attempts=$((attempts + 1))
        if [ "$attempts" -ge 50 ]; then
            echo "FAIL: $label HTTP server did not become ready" >&2
            sed -n '1,80p' "$log" >&2
            exit 1
        fi
        sleep 0.1
    done

    payload=$(sed -n 's/^data: //p' "$response" | sed -n '/./p' | tail -n 1)
    if [ -z "$payload" ]; then
        payload=$(cat "$response")
    fi
    printf '%s\n' "$payload" |
        jq -e '.result.protocolVersion == "2025-03-26" and .result.serverInfo.name == "noema"' >/dev/null
    kill "$server_pid"
    wait "$server_pid" 2>/dev/null || true
    server_pid=
    printf 'ok - %s Streamable HTTP initialize\n' "$label"
}

probe Go "$go_bin" "$base_port"
probe Rust "$rust_bin" "$((base_port + 1))"
printf '\nPASS: Go/Rust HTTP transport smoke suite\n'
