#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
go_bin=${NOEMA_GO_BIN:-"$repo_root/noema"}
rust_bin=${NOEMA_RUST_BIN:-"$repo_root/rust/target/debug/noema-rs"}

if [ ! -x "$go_bin" ]; then
    echo "error: Go binary not found at $go_bin (run make build)" >&2
    exit 1
fi
if [ ! -x "$rust_bin" ]; then
    echo "error: Rust binary not found at $rust_bin (run make rust-build)" >&2
    exit 1
fi

comparison_root=$(mktemp -d /tmp/noema-go-rust-compat.XXXXXX)
comparison_home="$comparison_root/home"
cortex_parent="$comparison_root/cortexes"
mkdir -p "$comparison_home" "$cortex_parent"
trap 'rm -rf "$comparison_root"' EXIT HUP INT TERM

go_noema() {
    env HOME="$comparison_home" "$go_bin" --cortex shared "$@"
}

rust_noema() {
    env HOME="$comparison_home" "$rust_bin" --cortex shared "$@"
}

assert_contains() {
    haystack=$1
    needle=$2
    label=$3
    case "$haystack" in
        *"$needle"*) ;;
        *)
            echo "FAIL: $label did not contain: $needle" >&2
            echo "$haystack" >&2
            exit 1
            ;;
    esac
    printf 'ok - %s\n' "$label"
}

mcp_tools() {
    implementation=$1
    printf '%s\n' \
        '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"interop-test","version":"1"}}}' \
        '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
        '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' |
        env HOME="$comparison_home" "$implementation" --cortex shared serve --transport stdio 2>/dev/null |
        jq -r 'select(.id == 2) | .result.tools[].name' |
        sort
}

env HOME="$comparison_home" "$go_bin" init --name shared --path "$cortex_parent" >/dev/null
go_noema keygen >/dev/null

go_id=$(go_noema add \
    --title "Go interoperability trace" \
    --type fact \
    --author go \
    --tag interop \
    --body "alpha bravo shared format" | sed -n 's/^Trace added: //p')
test -n "$go_id"
assert_contains "$(rust_noema get "$go_id")" "alpha bravo shared format" "Rust reads Go-created Markdown and SQLite row"
assert_contains "$(rust_noema search alpha)" "$go_id" "Rust FTS finds Go-created trace"

rust_noema append "$go_id" --content "appended by Rust" >/dev/null
assert_contains "$(go_noema get "$go_id")" "appended by Rust" "Go reads Rust mutation"

rust_noema archive "$go_id" >/dev/null
assert_contains "$(go_noema list --archived)" "$go_id" "Go sees Rust archive transition"
go_noema unarchive "$go_id" >/dev/null
assert_contains "$(rust_noema list)" "$go_id" "Rust sees Go unarchive transition"

rust_id=$(rust_noema add \
    --title "Rust interoperability trace" \
    --type decision \
    --author rust \
    --tag interop \
    --body "charlie delta shared format" | sed -n 's/^Trace added: //p')
test -n "$rust_id"
assert_contains "$(go_noema get "$rust_id")" "charlie delta shared format" "Go reads Rust-created Markdown and SQLite row"
assert_contains "$(go_noema search charlie)" "$rust_id" "Go FTS finds Rust-created trace"
rust_signature_count=$(sqlite3 "$cortex_parent/shared/db/noema.db" \
    "SELECT count(*) FROM events WHERE action='create' AND trace_id='$rust_id' AND signature LIKE 'ed25519:%' AND pubkey LIKE 'ed25519:%';")
test "$rust_signature_count" -eq 1
printf 'ok - Rust signs events with Go-generated key material\n'

go_noema append "$rust_id" --content "appended by Go" >/dev/null
assert_contains "$(rust_noema get "$rust_id")" "appended by Go" "Rust reads Go mutation"

go_noema archive "$rust_id" >/dev/null
assert_contains "$(rust_noema list --archived)" "$rust_id" "Rust sees Go archive transition"
rust_noema unarchive "$rust_id" >/dev/null
assert_contains "$(go_noema list)" "$rust_id" "Go sees Rust unarchive transition"

rust_noema remove "$go_id" >/dev/null
assert_contains "$(go_noema list --trashed)" "$go_id" "Go sees Rust trash transition"
go_noema recover "$go_id" >/dev/null
assert_contains "$(rust_noema get "$go_id")" "alpha bravo" "Rust sees Go recovery"

go_noema remove "$rust_id" >/dev/null
assert_contains "$(rust_noema list --trashed)" "$rust_id" "Rust sees Go trash transition"
rust_noema recover "$rust_id" >/dev/null
assert_contains "$(go_noema get "$rust_id")" "charlie delta" "Go sees Rust recovery"

takeover_marker="$comparison_root/rust-mutation-complete"
env HOME="$comparison_home" \
    NOEMA_RUST_TEST_PAUSE_AFTER_FILESYSTEM_MUTATION="$takeover_marker" \
    "$rust_bin" --cortex shared append "$go_id" \
    --content "uncommitted mixed-runtime body" >/dev/null 2>&1 &
takeover_pid=$!
takeover_ready=false
takeover_attempt=0
while [ "$takeover_attempt" -lt 1000 ]; do
    if [ -f "$takeover_marker" ]; then
        takeover_ready=true
        break
    fi
    if ! kill -0 "$takeover_pid" 2>/dev/null; then
        break
    fi
    takeover_attempt=$((takeover_attempt + 1))
    sleep 0.01
done
if [ "$takeover_ready" != true ]; then
    kill -KILL "$takeover_pid" 2>/dev/null || true
    wait "$takeover_pid" 2>/dev/null || true
    echo "FAIL: Rust writer did not reach the mixed-runtime fault boundary" >&2
    exit 1
fi
kill -KILL "$takeover_pid"
wait "$takeover_pid" 2>/dev/null || true
if takeover_error=$(go_noema get "$go_id" 2>&1); then
    echo "FAIL: Go opened a Cortex with an interrupted Rust mutation" >&2
    exit 1
fi
assert_contains "$takeover_error" "interrupted Rust trace mutation" "Go refuses pending Rust recovery"
assert_contains "$(rust_noema get "$go_id")" "alpha bravo shared format" "Rust repairs killed mutation before takeover"
assert_contains "$(go_noema get "$go_id")" "alpha bravo shared format" "Go opens after Rust recovery"

go_noema sync >/dev/null
rust_noema sync >/dev/null
assert_contains "$(go_noema verify)" "All hashes OK." "Go integrity verifier accepts shared cortex"
assert_contains "$(rust_noema verify)" "All traces verified." "Rust integrity verifier accepts shared cortex"

database="$cortex_parent/shared/db/noema.db"
integrity=$(sqlite3 "$database" 'PRAGMA integrity_check;')
test "$integrity" = "ok"
printf 'ok - SQLite integrity check\n'

trace_count=$(sqlite3 "$database" 'SELECT count(*) FROM traces;')
event_count=$(sqlite3 "$database" 'SELECT count(*) FROM events;')
test "$trace_count" -eq 2
test "$event_count" -ge 10
printf 'ok - shared schema contains %s traces and %s mutation events\n' "$trace_count" "$event_count"

go_tools="$comparison_root/go-tools.txt"
rust_tools="$comparison_root/rust-tools.txt"
mcp_tools "$go_bin" >"$go_tools"
mcp_tools "$rust_bin" >"$rust_tools"
diff -u "$go_tools" "$rust_tools"
tool_count=$(wc -l <"$go_tools" | tr -d ' ')
test "$tool_count" -eq 28
printf 'ok - MCP discovery surfaces the same %s tools\n' "$tool_count"

printf '\nPASS: Go/Rust differential compatibility suite\n'
