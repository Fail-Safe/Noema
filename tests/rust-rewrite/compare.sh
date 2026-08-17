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
    NOEMA_DURABILITY=strong \
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
assert_contains "$(rust_noema verify)" "All hashes OK." "Rust integrity verifier accepts shared cortex"
assert_contains "$(go_noema verify cortex)" "0 fail" "Go cortex doctor accepts shared cortex"
assert_contains "$(rust_noema verify cortex)" "0 fail" "Rust cortex doctor accepts shared cortex"
assert_contains "$(go_noema verify drift)" "No federated traces with source hashes found." "Go drift verifier handles local-only cortex"
assert_contains "$(rust_noema verify drift)" "No federated traces with source hashes found." "Rust drift verifier handles local-only cortex"

orphan_id=$(go_noema add \
    --title "Recoverable interoperability trace" \
    --type note \
    --author go \
    --tag interop \
    --body "event snapshot recovery" | sed -n 's/^Trace added: //p')
test -n "$orphan_id"
rm "$cortex_parent/shared/traces/$orphan_id.md"
assert_contains "$(rust_noema sync --recover)" "Recovered: 1  Orphaned: 0" "Rust sync recovers a Go event snapshot"
assert_contains "$(go_noema get "$orphan_id")" "event snapshot recovery" "Go reads Rust-recovered snapshot"

guided_output=$(printf '%s\n' \
    'Guided Rust trace' \
    'fact' \
    'rust' \
    'guided, interop' \
    'body from guided input' |
    rust_noema add)
guided_id=$(printf '%s\n' "$guided_output" | sed -n 's/.*Trace added: //p')
test -n "$guided_id"
assert_contains "$(go_noema get "$guided_id")" "body from guided input" "Go reads Rust guided-add trace"

backfill_id=$(go_noema add \
    --title "Rust event backfill trace" \
    --type fact \
    --author go \
    --tag backfill \
    --body "backfill interoperability" | sed -n 's/^Trace added: //p')
test -n "$backfill_id"
sqlite3 "$cortex_parent/shared/db/noema.db" \
    "DELETE FROM events WHERE action='create' AND trace_id='$backfill_id';"
assert_contains "$(rust_noema events backfill --dry-run)" "$backfill_id" "Rust previews missing create events"
rust_noema events backfill --yes >/dev/null
assert_contains "$(go_noema events "$backfill_id")" "create" "Go reads Rust-backfilled create event"

rust_noema memory stats --output json | jq -e '.tiers.Short >= 1 and .tiers.Purged == 0' >/dev/null
rust_noema memory popular --output json | jq -e '.schema_version == 1 and (.traces | type) == "array"' >/dev/null
rust_noema memory health --output json | jq -e '.schema_version == 1 and .activity and .latency and .one_source_mid' >/dev/null
printf 'ok - Rust memory observability emits the Go-compatible JSON envelopes\n'

purge_id=$(go_noema add \
    --title "Rust ceremonial purge trace" \
    --type fact \
    --author go \
    --tag purge \
    --body "purge interoperability" | sed -n 's/^Trace added: //p')
go_noema memory promote "$purge_id" >/dev/null
go_noema memory promote "$purge_id" >/dev/null
rust_noema memory purge "$purge_id" --tier long --reason interop --confirm >/dev/null
test "$(sqlite3 "$cortex_parent/shared/db/noema.db" "SELECT purge_reason FROM traces WHERE id='$purge_id';")" = interop
assert_contains "$(go_noema events "$purge_id")" "purge_long_term" "Go reads Rust long-term purge event"
rust_noema memory purge "$purge_id" --tier long --reason erasure --confirm --hard >/dev/null
test "$(sqlite3 "$cortex_parent/shared/db/noema.db" "SELECT count(*) FROM traces WHERE id='$purge_id';")" -eq 0
printf 'ok - Rust ceremonial soft and hard purge stays readable by Go\n'

force_id=$(go_noema add \
    --title "Rust force remove trace" \
    --type note \
    --author go \
    --tag remove \
    --body "force remove interoperability" | sed -n 's/^Trace added: //p')
rust_noema remove --force "$force_id" >/dev/null
test "$(sqlite3 "$cortex_parent/shared/db/noema.db" "SELECT count(*) FROM traces WHERE id='$force_id';")" -eq 0
printf 'ok - Rust remove --force performs Go-compatible hard deletion\n'

collision_id=$(rust_noema add \
    --title "Rust collision trace" \
    --type note \
    --author rust \
    --tag collision \
    --body "first collision body" | sed -n 's/^Trace added: //p')
collision_output=$(printf '%s\n' v 'Rust collision alternate' | rust_noema add \
    --title "Rust collision trace" \
    --type note \
    --author rust \
    --tag collision \
    --body "second collision body" 2>/dev/null)
collision_alternate_id=$(printf '%s\n' "$collision_output" | sed -n 's/.*Trace added: //p')
test -n "$collision_id"
test -n "$collision_alternate_id"
test "$collision_id" != "$collision_alternate_id"
assert_contains "$(go_noema get "$collision_alternate_id")" "second collision body" "Rust collision workflow preserves new content"

env HOME="$comparison_home" "$go_bin" init --name migration --path "$cortex_parent" >/dev/null
migration_id=$(env HOME="$comparison_home" "$go_bin" --cortex migration add \
    --title "Migration interoperability trace" \
    --type fact \
    --author go \
    --tag migration \
    --body "identity migration body" | sed -n 's/^Trace added: //p')
test -n "$migration_id"
migration_manifest="$cortex_parent/migration/cortex.md"
sed -e 's/^id: .*/id: ""/' -e 's/^version: 2$/version: 1/' \
    "$migration_manifest" >"$migration_manifest.tmp"
mv "$migration_manifest.tmp" "$migration_manifest"
sqlite3 "$cortex_parent/migration/db/noema.db" \
    "UPDATE traces SET cortex_id=''; UPDATE events SET cortex_id=''; INSERT INTO federation_state(key,value) VALUES ('vclock','{\"migration\":1}') ON CONFLICT(key) DO UPDATE SET value=excluded.value;"
migration_output=$(env HOME="$comparison_home" "$rust_bin" --cortex migration migrate cortex-id --yes)
assert_contains "$migration_output" "Migration complete." "Rust migrates a Go-created v1 cortex"
assert_contains "$(env HOME="$comparison_home" "$go_bin" --cortex migration get "$migration_id")" \
    "identity migration body" "Go opens the Rust-migrated cortex"
migrated_manifest_id=$(sed -n 's/^id: //p' "$migration_manifest")
migrated_row_id=$(sqlite3 "$cortex_parent/migration/db/noema.db" \
    "SELECT cortex_id FROM traces WHERE id='$migration_id';")
if [ -f "$comparison_home/Library/Application Support/noema/config.yaml" ]; then
    migration_config="$comparison_home/Library/Application Support/noema/config.yaml"
else
    migration_config="$comparison_home/.config/noema/config.yaml"
fi
migrated_config_id=$(awk '
    $0 == "  migration:" { in_migration = 1; next }
    in_migration && /^    id: / { sub(/^    id: /, ""); print; exit }
    in_migration && /^  [^ ]/ { in_migration = 0 }
' "$migration_config")
test -n "$migrated_manifest_id"
test "$migrated_manifest_id" = "$migrated_row_id"
test "$migrated_manifest_id" = "$migrated_config_id"
test "$(find "$cortex_parent/migration" -maxdepth 1 -name 'cortex.md.*.bak' -type f | wc -l | tr -d ' ')" -eq 1
test "$(find "$cortex_parent/migration/db" -maxdepth 1 -name 'noema.db.*.bak' -type f | wc -l | tr -d ' ')" -eq 1
printf 'ok - migration identity, rows, config, and backups stay coherent\n'

database="$cortex_parent/shared/db/noema.db"
integrity=$(sqlite3 "$database" 'PRAGMA integrity_check;')
test "$integrity" = "ok"
printf 'ok - SQLite integrity check\n'

trace_count=$(sqlite3 "$database" 'SELECT count(*) FROM traces;')
event_count=$(sqlite3 "$database" 'SELECT count(*) FROM events;')
test "$trace_count" -eq 7
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
