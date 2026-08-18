#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
go_bin=${NOEMA_GO_BIN:-"$repo_root/dist/noema-$(go env GOOS)-$(go env GOARCH)"}
rust_bin=${NOEMA_RUST_BIN:-"$repo_root/rust/target/release/noema-rs"}
trace_total=${NOEMA_BENCH_TRACES:-200}
read_iterations=${NOEMA_BENCH_READS:-100}

if [ ! -x "$go_bin" ] || [ ! -x "$rust_bin" ]; then
    echo "error: release binaries are missing (run make comparison-release)" >&2
    exit 1
fi

benchmark_root=$(mktemp -d /tmp/noema-go-rust-bench.XXXXXX)
trap 'rm -rf "$benchmark_root"' EXIT HUP INT TERM

now_ns() {
    python3 -c 'import time; print(time.perf_counter_ns())'
}

elapsed_ms() {
    start=$1
    end=$2
    python3 -c "print(round(($end - $start) / 1000000, 3))"
}

emit() {
    implementation=$1
    operation=$2
    iterations=$3
    milliseconds=$4
    printf '%s\t%s\t%s\t%s\n' "$implementation" "$operation" "$iterations" "$milliseconds"
}

run_suite() {
    implementation=$1
    binary=$2
    suite_home="$benchmark_root/$implementation/home"
    suite_parent="$benchmark_root/$implementation/cortexes"
    mkdir -p "$suite_home" "$suite_parent"

    env HOME="$suite_home" "$binary" init --name bench --path "$suite_parent" >/dev/null

    start=$(now_ns)
    index=1
    while [ "$index" -le "$trace_total" ]; do
        env HOME="$suite_home" "$binary" --cortex bench add \
            --title "Benchmark trace $index" \
            --type fact \
            --author benchmark \
            --tag performance \
            --body "shared benchmark corpus alpha beta gamma item $index" >/dev/null
        index=$((index + 1))
    done
    end=$(now_ns)
    emit "$implementation" ingest "$trace_total" "$(elapsed_ms "$start" "$end")"

    env HOME="$suite_home" "$binary" --cortex bench search alpha >/dev/null
    start=$(now_ns)
    index=1
    while [ "$index" -le "$read_iterations" ]; do
        env HOME="$suite_home" "$binary" --cortex bench search alpha >/dev/null
        index=$((index + 1))
    done
    end=$(now_ns)
    emit "$implementation" search "$read_iterations" "$(elapsed_ms "$start" "$end")"

    start=$(now_ns)
    index=1
    while [ "$index" -le "$read_iterations" ]; do
        env HOME="$suite_home" "$binary" --cortex bench list --tag performance >/dev/null
        index=$((index + 1))
    done
    end=$(now_ns)
    emit "$implementation" list "$read_iterations" "$(elapsed_ms "$start" "$end")"

    start=$(now_ns)
    env HOME="$suite_home" "$binary" --cortex bench sync >/dev/null
    end=$(now_ns)
    emit "$implementation" sync 1 "$(elapsed_ms "$start" "$end")"

    start=$(now_ns)
    env HOME="$suite_home" "$binary" --cortex bench verify >/dev/null
    end=$(now_ns)
    emit "$implementation" verify 1 "$(elapsed_ms "$start" "$end")"
}

printf 'implementation\toperation\titerations\telapsed_ms\n'
run_suite go "$go_bin"
run_suite rust "$rust_bin"

go_size=$(stat -f '%z' "$go_bin")
rust_size=$(stat -f '%z' "$rust_bin")
emit go binary_bytes 1 "$go_size"
emit rust binary_bytes 1 "$rust_size"
